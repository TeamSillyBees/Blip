// Package setup provides the interactive, local-only channel configuration wizard.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/TeamSillyBees/Blip/internal/config"
)

// ErrCancelled means the user abandoned the wizard. No changes are saved.
var ErrCancelled = errors.New("configuration cancelled")

// Choice is one entry in a prompt. Values are stable controller actions.
type Choice struct {
	Label string
	Value string
}

// TextInput describes one input without ever exposing an existing secret.
type TextInput struct {
	Title       string
	Description string
	Initial     string
	Secret      bool
	Validate    func(string) error
}

// Prompter separates the editing workflow from terminal rendering.
// Implementations must return ErrCancelled on interruption and mask Secret inputs.
type Prompter interface {
	Select(context.Context, string, string, []Choice, string) (string, error)
	Input(context.Context, TextInput) (string, error)
	Confirm(context.Context, string, string, bool) (bool, error)
}

// Run opens a Huh wizard on an interactive terminal. It never sends notifications,
// starts a worker, or installs hooks.
func Run(ctx context.Context, path string, input io.Reader, output io.Writer, accessible bool) error {
	if !isTerminal(input) || !isTerminal(output) {
		return errors.New("交互配置需要终端（TTY）输入和输出；请在终端中运行 blip configure。脚本请直接编辑配置文件")
	}
	return RunWithPrompter(ctx, path, newHuhPrompter(input, output, accessible), output)
}

// RunWithPrompter runs the same transactional controller with a supplied UI.
// Cancellation is a successful exit with no writes; all other errors propagate.
func RunWithPrompter(ctx context.Context, path string, prompts Prompter, output io.Writer) error {
	document, err := config.OpenDocument(path)
	if err != nil {
		return err
	}
	c := controller{prompts: prompts, output: output, working: document.Config.Clone()}
	original := document.Config.Clone()
	if err = c.run(ctx, func() error {
		document.Config = c.working.Clone()
		return document.Save(ctx)
	}); err != nil {
		if errors.Is(err, ErrCancelled) || errors.Is(err, context.Canceled) {
			fmt.Fprintln(output, "已取消，未保存任何修改。")
			return nil
		}
		return err
	}
	fmt.Fprintln(output, "配置已保存。可按需执行：")
	fmt.Fprintln(output, "  blip install codex")
	fmt.Fprintln(output, "  blip start")
	fmt.Fprintln(output, "  blip test")
	if !reflect.DeepEqual(original, c.working) {
		fmt.Fprintln(output, "若工作进程已在运行，请先执行 blip stop，再执行 blip start 以加载新配置。")
	}
	fmt.Fprintln(output, "使用自定义配置路径时，请为上述命令添加 --config 参数。")
	return nil
}

type controller struct {
	prompts Prompter
	output  io.Writer
	working config.Config
}

func (c *controller) run(ctx context.Context, save func() error) error {
	if c.working.DeviceName == "" {
		if err := c.editDeviceName(ctx); err != nil {
			return err
		}
	}
	if len(c.working.Destinations) == 0 {
		fmt.Fprintln(c.output, "添加第一个通知目的地。Ctrl+C 可随时取消。")
		if err := c.add(ctx); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		choices := []Choice{{"添加目的地", "add"}}
		if len(c.working.Destinations) > 0 {
			choices = append(choices, Choice{"编辑目的地", "edit"}, Choice{"启用 / 停用目的地", "toggle"}, Choice{"删除目的地", "delete"})
		}
		choices = append(choices, Choice{"修改设备名称", "device"}, Choice{"保存并退出", "save"}, Choice{"取消，不保存", "cancel"})
		action, err := c.prompts.Select(ctx, "通知渠道配置", c.summary(), choices, "add")
		if err != nil {
			return err
		}
		switch action {
		case "add":
			err = c.add(ctx)
		case "edit", "toggle", "delete":
			err = c.change(ctx, action)
		case "device":
			err = c.editDeviceName(ctx)
		case "save":
			var confirm bool
			confirm, err = c.prompts.Confirm(ctx, "保存配置并退出？", c.summary(), false)
			if err == nil && confirm {
				if err = ctx.Err(); err != nil {
					return err
				}
				return save()
			}
		case "cancel":
			return ErrCancelled
		default:
			return errors.New("未知的配置操作")
		}
		if err != nil {
			return err
		}
	}
}

func (c *controller) editDeviceName(ctx context.Context) error {
	initial := c.working.DeviceName
	if initial == "" {
		initial = config.DefaultDeviceName()
	}
	name, err := c.prompts.Input(ctx, TextInput{
		Title: "设备名称", Description: "用于在通知中识别这台设备。", Initial: initial,
		Validate: func(value string) error {
			if err := config.ValidateDeviceName(value); err != nil {
				return errors.New("设备名称须为 1–128 个字符，不能含首尾空格或控制字符")
			}
			return nil
		},
	})
	if err == nil {
		c.working.DeviceName = name
	}
	return err
}

func (c *controller) add(ctx context.Context) error {
	kind, err := c.prompts.Select(ctx, "选择通知渠道", "同一渠道可添加多个独立目的地。", []Choice{
		{"飞书群机器人", "feishu"}, {"企业微信群机器人", "wecom"}, {"Gotify", "gotify"}, {"自定义 Webhook", "webhook"}, {"返回主菜单", "back"},
	}, "feishu")
	if err != nil || kind == "back" {
		return err
	}
	id, err := c.prompts.Input(ctx, TextInput{Title: "目的地 ID", Description: "用于识别通知目标并关联投递记录；保存后 ID 保持不变。", Initial: c.nextID(kind), Validate: c.validateID})
	if err != nil {
		return err
	}
	d := config.Destination{ID: id, Kind: kind, Enabled: true}
	if kind == "gotify" {
		d.Priority = 5
	}
	if err = c.editFields(ctx, &d); err != nil {
		return err
	}
	c.working.Destinations = append(c.working.Destinations, d)
	fmt.Fprintf(c.output, "已添加 %s（待保存）。\n", safeLabel(id))
	return nil
}

func (c *controller) change(ctx context.Context, action string) error {
	choices := make([]Choice, 0, len(c.working.Destinations)+1)
	for index, d := range c.working.Destinations {
		choices = append(choices, Choice{destinationLabel(d), strconv.Itoa(index)})
	}
	choices = append(choices, Choice{"返回主菜单", "back"})
	selected, err := c.prompts.Select(ctx, "选择目的地", "", choices, "0")
	if err != nil || selected == "back" {
		return err
	}
	index, err := strconv.Atoi(selected)
	if err != nil || index < 0 || index >= len(c.working.Destinations) {
		return errors.New("无效的目的地选择")
	}
	d := cloneDestination(c.working.Destinations[index])
	switch action {
	case "edit":
		fmt.Fprintf(c.output, "编辑 %s。\n", safeLabel(d.ID))
		if err = c.editFields(ctx, &d); err == nil {
			c.working.Destinations[index] = d
		}
	case "toggle":
		verb := "启用"
		if d.Enabled {
			verb = "停用"
		}
		var confirm bool
		confirm, err = c.prompts.Confirm(ctx, verb+" "+safeLabel(d.ID)+"？", "", false)
		if err == nil && confirm {
			d.Enabled = !d.Enabled
			if d.Enabled {
				if validationErr := config.ValidateDestination(d); validationErr != nil {
					fmt.Fprintln(c.output, "无法启用：请先编辑并补全该目的地配置。")
					return nil
				}
			}
			c.working.Destinations[index] = d
		}
	case "delete":
		var confirm bool
		confirm, err = c.prompts.Confirm(ctx, "删除 "+safeLabel(d.ID)+"？", "保存后删除该目的地。", false)
		if err == nil && confirm {
			c.working.Destinations = append(c.working.Destinations[:index], c.working.Destinations[index+1:]...)
		}
	}
	return err
}

func (c *controller) editFields(ctx context.Context, d *config.Destination) error {
	urlTitle := "Webhook URL"
	if d.Kind == "gotify" {
		urlTitle = "Gotify 服务 URL"
	}
	var err error
	d.URL, err = c.prompts.Input(ctx, TextInput{Title: urlTitle, Initial: d.URL, Validate: validateURL})
	if err != nil {
		return err
	}
	switch d.Kind {
	case "feishu":
		d.Secret, err = c.credential(ctx, "飞书签名 Secret（可选）", d.Secret, false, validateSecret)
	case "wecom":
	case "gotify":
		d.Token, err = c.credential(ctx, "Gotify 应用 Token", d.Token, true, func(v string) error { return validateToken(v, true) })
		if err == nil {
			var priority string
			priority, err = c.prompts.Input(ctx, TextInput{Title: "消息优先级", Description: "请输入非负整数，默认 5。", Initial: strconv.Itoa(d.Priority), Validate: validatePriority})
			if err == nil {
				d.Priority, _ = strconv.Atoi(priority)
			}
		}
	case "webhook":
		d.Token, err = c.credential(ctx, "Bearer Token（可选）", d.Token, false, func(v string) error { return validateToken(v, false) })
		if err == nil {
			err = c.editHeaders(ctx, d)
		}
	default:
		return errors.New("不支持的通知渠道")
	}
	if err != nil {
		return err
	}
	d.Enabled, err = c.prompts.Confirm(ctx, "启用此目的地？", "停用后不会接收通知；以后可以在主菜单重新启用。", d.Enabled)
	if err != nil {
		return err
	}
	if err = config.ValidateDestination(*d); err != nil {
		return fmt.Errorf("目的地配置无效：%w", err)
	}
	return nil
}

func (c *controller) credential(ctx context.Context, title, current string, required bool, validate func(string) error) (string, error) {
	if current != "" {
		choices := []Choice{{"保留现有值", "keep"}, {"替换为新值", "change"}}
		if !required {
			choices = append(choices, Choice{"清除现有值", "clear"})
		}
		action, err := c.prompts.Select(ctx, title, "", choices, "keep")
		if err != nil {
			return "", err
		}
		switch action {
		case "keep":
			return current, nil
		case "clear":
			return "", nil
		case "change":
		default:
			return "", errors.New("无效的凭据操作")
		}
	}
	description := ""
	if !required && current == "" {
		description = "可留空。"
	}
	return c.prompts.Input(ctx, TextInput{Title: title, Description: description, Secret: true, Validate: func(value string) error {
		if (required || current != "") && strings.TrimSpace(value) == "" {
			return errors.New("请输入新值")
		}
		return validate(value)
	}})
}

func (c *controller) editHeaders(ctx context.Context, d *config.Destination) error {
	for {
		keys := sortedHeaderKeys(d.Headers)
		description := "Bearer Token 与 Authorization 请求头只能二选一。"
		if len(keys) > 0 {
			description += "\n已设置：" + strings.Join(keys, "、")
		}
		choices := []Choice{{"添加请求头", "add"}}
		if len(keys) > 0 {
			choices = append(choices, Choice{"编辑请求头", "edit"}, Choice{"删除请求头", "delete"})
		}
		choices = append(choices, Choice{"完成请求头设置", "done"})
		action, err := c.prompts.Select(ctx, "自定义请求头", description, choices, "done")
		if err != nil {
			return err
		}
		if action == "done" {
			if d.Token != "" && hasAuthorization(d.Headers) {
				fmt.Fprintln(c.output, "已有 Bearer Token；请删除 Authorization 请求头，或选择清除 Bearer Token。")
				clear, err := c.prompts.Confirm(ctx, "清除 Bearer Token？", "保留 Authorization 请求头作为认证。", false)
				if err != nil {
					return err
				}
				if !clear {
					continue
				}
				d.Token = ""
			}
			return nil
		}
		oldName := ""
		if action == "edit" || action == "delete" {
			headerChoices := make([]Choice, 0, len(keys)+1)
			for _, key := range keys {
				headerChoices = append(headerChoices, Choice{key, key})
			}
			headerChoices = append(headerChoices, Choice{"返回请求头菜单", ""})
			oldName, err = c.prompts.Select(ctx, "选择请求头", "", headerChoices, "")
			if err != nil {
				return err
			}
			if oldName == "" {
				continue
			}
		}
		if action == "delete" {
			confirm, err := c.prompts.Confirm(ctx, "删除请求头 "+oldName+"？", "", false)
			if err != nil {
				return err
			}
			if confirm {
				delete(d.Headers, oldName)
			}
			continue
		}
		if action != "add" && action != "edit" {
			return errors.New("无效的请求头操作")
		}
		name, err := c.prompts.Input(ctx, TextInput{Title: "请求头名称", Initial: oldName, Description: "例如 X-API-Key。", Validate: func(name string) error {
			if err := config.ValidateHeader(name, ""); err != nil {
				return errors.New("请输入有效的请求头名称，不可覆盖系统管理的请求头")
			}
			if d.Token != "" && strings.EqualFold(name, "Authorization") {
				return errors.New("已设置 Bearer Token，请先清除 Token 才能添加 Authorization")
			}
			for key := range d.Headers {
				if key != oldName && strings.EqualFold(name, key) {
					return errors.New("该请求头已存在（名称不区分大小写）")
				}
			}
			return nil
		}})
		if err != nil {
			return err
		}
		value, err := c.credential(ctx, "请求头值（"+name+"）", d.Headers[oldName], false, func(value string) error {
			if err := config.ValidateHeader(name, value); err != nil {
				return errors.New("请求头值无效")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if d.Headers == nil {
			d.Headers = map[string]string{}
		}
		delete(d.Headers, oldName)
		d.Headers[name] = value
	}
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (c *controller) validateID(id string) error {
	if !idPattern.MatchString(id) {
		return errors.New("ID 须为 1–64 位字母、数字、下划线或短横线，并以字母或数字开头")
	}
	for _, d := range c.working.Destinations {
		if d.ID == id {
			return errors.New("该 ID 已存在，请换一个名称")
		}
	}
	return nil
}

func (c *controller) nextID(kind string) string {
	for i := 1; ; i++ {
		id := fmt.Sprintf("%s-%d", kind, i)
		if c.validateID(id) == nil {
			return id
		}
	}
}

func (c *controller) summary() string {
	lines := []string{"设备：" + safeLabel(c.working.DeviceName), fmt.Sprintf("共 %d 个目的地。", len(c.working.Destinations))}
	for _, d := range c.working.Destinations {
		lines = append(lines, destinationLabel(d))
	}
	return strings.Join(lines, "\n")
}

func destinationLabel(d config.Destination) string {
	status := "已停用"
	if d.Enabled {
		status = "已启用"
	}
	name := map[string]string{"feishu": "飞书", "wecom": "企业微信", "gotify": "Gotify", "webhook": "Webhook"}[d.Kind]
	return safeLabel(d.ID) + " · " + name + " · " + status
}

func safeLabel(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func validateURL(value string) error {
	if err := config.ValidateDestination(config.Destination{ID: "input", Kind: "wecom", URL: value}); err != nil {
		return errors.New("请输入有效的 HTTP(S) URL，不可包含登录信息或片段")
	}
	return nil
}

func validateToken(value string, required bool) error {
	kind := "webhook"
	if required {
		kind = "gotify"
	}
	if err := config.ValidateDestination(config.Destination{ID: "input", Kind: kind, URL: "https://example.invalid", Token: value}); err != nil {
		return errors.New("Token 无效，不能包含空白或控制字符")
	}
	return nil
}

func validateSecret(value string) error {
	if err := config.ValidateDestination(config.Destination{ID: "input", Kind: "feishu", URL: "https://example.invalid", Secret: value}); err != nil {
		return errors.New("Secret 格式无效")
	}
	return nil
}

func validatePriority(value string) error {
	priority, err := strconv.Atoi(value)
	if err != nil || priority < 0 {
		return errors.New("优先级必须是非负整数")
	}
	return nil
}

func cloneDestination(d config.Destination) config.Destination {
	if d.Headers != nil {
		headers := make(map[string]string, len(d.Headers))
		for k, v := range d.Headers {
			headers[k] = v
		}
		d.Headers = headers
	}
	return d
}

func sortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hasAuthorization(headers map[string]string) bool {
	for name := range headers {
		if http.CanonicalHeaderKey(name) == "Authorization" {
			return true
		}
	}
	return false
}
