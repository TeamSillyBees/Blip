package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/TeamSillyBees/Blip/internal/setup"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/TeamSillyBees/Blip/internal/worker"
	"github.com/spf13/cobra"
)

var version = "0.2.0"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Blip:", err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	defaultPath, defaultPathErr := config.DefaultPath()
	path := defaultPath
	var accessible bool
	root := &cobra.Command{
		Use:           "blip",
		Short:         "在编程助手完成一轮响应或需要输入时发送通知",
		Long:          "Blip 在编程助手完成一轮响应或需要你输入时发送通知。\n首次使用：直接运行 blip，填写通知渠道后选择“保存并启动”。",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !setup.CanPrompt(command.InOrStdin(), command.OutOrStdout()) {
				return command.Help()
			}
			if path == "" {
				return fmt.Errorf("无法确定默认配置路径，请用 --config 指定配置文件")
			}
			return configure(command.Context(), path, command.InOrStdin(), command.OutOrStdout(), accessible)
		},
	}
	root.SetIn(input)
	root.SetOut(output)
	root.SetErr(output)
	root.SetArgs(args)
	root.SetFlagErrorFunc(flagError)
	root.PersistentFlags().StringVar(&path, "config", defaultPath, "配置文件路径")
	root.PersistentFlags().BoolVar(&accessible, "accessible", false, "使用适合屏幕阅读器的文本提示")
	root.PersistentPreRunE = func(command *cobra.Command, _ []string) error {
		if err := requiredFlagsError(command); err != nil {
			return err
		}
		if path != "" || command == root || command.Name() == "help" || command.Name() == "version" || command.HasSubCommands() {
			return nil
		}
		for parent := command; parent != nil; parent = parent.Parent() {
			if parent.Name() == "completion" {
				return nil
			}
		}
		if defaultPathErr != nil {
			return fmt.Errorf("无法确定默认配置路径，请用 --config 指定配置文件：%w", defaultPathErr)
		}
		return fmt.Errorf("配置文件路径不能为空")
	}
	root.AddCommand(newServiceCommand(&path))
	root.AddCommand(newSourcesCommand(&path))
	root.SetVersionTemplate("{{.Version}}\n")
	for _, entry := range []struct{ name, description string }{
		{"init", "生成配置示例，供脚本或手动编辑"},
		{"start", "启动普通后台通知进程"},
		{"stop", "请求平稳停止通知进程"},
		{"restart", "重启并加载最新配置（自动识别 Windows 服务）"},
		{"run", "在当前终端运行通知进程"},
		{"status", "查看运行状态和投递结果"},
		{"test", "测试通知；终端检查投递，脚本默认仅入队"},
		{"doctor", "检查来源接入、能力和最近回调"},
		{"hook", "从标准输入接收编程助手的钩子事件"},
	} {
		var sourceID string
		var jsonOutput bool
		var wait bool
		command := &cobra.Command{
			Use: entry.name, Short: entry.description, Args: noArgs, Hidden: entry.name == "hook",
			RunE: func(command *cobra.Command, _ []string) error {
				if command.Name() == "test" && (wait || (!command.Flags().Changed("wait") && setup.CanPrompt(command.InOrStdin(), command.OutOrStdout()))) {
					c, err := config.Read(path)
					if err != nil {
						return err
					}
					return sendTest(command.Context(), c, command.OutOrStdout())
				}
				if command.Name() == "status" && !jsonOutput && setup.CanPrompt(command.InOrStdin(), command.OutOrStdout()) {
					c, err := config.ReadLocal(path)
					if err != nil {
						return err
					}
					return printStatusText(c, command.OutOrStdout())
				}
				args := []string{command.Name()}
				if sourceID != "" {
					args = append(args, sourceID)
				}
				return execute(command.Context(), path, args, command.InOrStdin(), command.OutOrStdout())
			},
		}
		if entry.name == "hook" || entry.name == "doctor" {
			description := "要检查的来源 ID；默认检查所有来源"
			if entry.name == "hook" {
				description = "来源 ID；默认使用 codex-default"
			}
			command.Flags().StringVar(&sourceID, "source", "", description)
		}
		if entry.name == "status" {
			command.Flags().BoolVar(&jsonOutput, "json", false, "输出 JSON，适合脚本处理；重定向输出时默认启用")
		}
		if entry.name == "test" {
			command.Flags().BoolVar(&wait, "wait", false, "自动启动并等待投递结果；交互终端默认启用，--wait=false 仅入队")
		}
		root.AddCommand(command)
	}
	for _, action := range []string{"install", "uninstall"} {
		verb := map[string]string{"install": "安装", "uninstall": "卸载"}[action]
		parent := &cobra.Command{Use: action, Short: verb + "编程助手接入", Args: noArgs,
			RunE: func(command *cobra.Command, _ []string) error { return command.Help() },
		}
		for harness := range agents.Builtins() {
			var sourceID string
			child := &cobra.Command{
				Use: harness, Short: verb + " " + harness + " 的 Blip 通知钩子", Args: noArgs,
				RunE: func(command *cobra.Command, _ []string) error {
					if sourceID == "" {
						c, err := config.ReadLocal(path)
						if err != nil {
							return err
						}
						sourceID, err = inferSource(c, harness)
						if err != nil {
							return err
						}
					}
					return execute(command.Context(), path, []string{command.Parent().Name(), harness, sourceID}, command.InOrStdin(), command.OutOrStdout())
				},
			}
			child.Flags().StringVar(&sourceID, "source", "", "已配置的来源 ID")
			parent.AddCommand(child)
		}
		root.AddCommand(parent)
	}
	configureCommand := &cobra.Command{
		Use: "configure", Aliases: []string{"config", "setup"},
		Short: "配置渠道，一次完成接入、启动和测试",
		Long:  "配置飞书、企业微信、Gotify 和自定义 Webhook。\n设备名称和目的地 ID 自动生成。选择保存并启动即可应用配置并发送测试通知；也可仅保存。\n保存前按 Ctrl+C 取消，不写入修改。",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return configure(command.Context(), path, command.InOrStdin(), command.OutOrStdout(), accessible)
		},
	}
	root.AddCommand(configureCommand)
	root.AddCommand(&cobra.Command{Use: "version", Short: "显示版本号", Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(command.OutOrStdout(), version)
			return err
		},
	})
	root.AddGroup(&cobra.Group{ID: "common", Title: "常用操作："}, &cobra.Group{ID: "advanced", Title: "接入与维护："})
	for _, command := range root.Commands() {
		switch command.Name() {
		case "configure", "start", "stop", "restart", "status", "test":
			command.GroupID = "common"
		default:
			command.GroupID = "advanced"
		}
	}
	root.SetHelpCommandGroupID("advanced")
	root.SetCompletionCommandGroupID("advanced")
	localizeHelp(root)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	err := root.ExecuteContext(ctx)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("尚未创建配置。请先运行 %s", cliCommand(path, "configure"))
		}
	}
	return err
}

func execute(ctx context.Context, path string, command []string, input io.Reader, output io.Writer) error {
	var err error
	if command[0] == "init" {
		if _, err = os.Stat(path); err == nil {
			return fmt.Errorf("配置已存在；修改配置请运行 %s", cliCommand(path, "configure"))
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data, err := config.ExampleBytes()
		if err != nil {
			return err
		}
		if err = store.WriteFile(path, data); err != nil {
			return err
		}
		absolute, _ := filepath.Abs(path)
		fmt.Fprintln(output, "配置已创建：", absolute)
		return nil
	}
	var c config.Config
	if command[0] == "start" || command[0] == "run" || command[0] == "restart" {
		c, err = config.Read(path)
	} else {
		c, err = config.ReadLocal(path)
	}
	if err != nil {
		if command[0] == "hook" {
			return nil
		}
		return err
	}
	switch command[0] {
	case "hook":
		id := config.DefaultSourceID
		if len(command) > 1 {
			id = command[1]
		}
		_ = agents.Capture(ctx, c, id, input)
		return nil // Notification failures cannot approve, deny, or block an agent operation.
	case "install", "uninstall":
		if len(command) < 2 {
			return fmt.Errorf("请指定编程助手类型")
		}
		id := config.DefaultSourceID
		if len(command) > 2 {
			id = command[2]
		}
		var source config.Source
		if command[0] == "uninstall" {
			source, err = agents.InstalledSource(c, id)
		} else {
			var ok bool
			source, ok = c.Source(id)
			if !ok {
				err = fmt.Errorf("尚未配置来源 %q", id)
			}
		}
		if err != nil {
			return err
		}
		if source.Harness != command[1] {
			return fmt.Errorf("来源 %q 属于 %s", id, source.Harness)
		}
		adapter, err := agents.Builtins().Open(c, source)
		if err != nil {
			return err
		}
		installer, ok := adapter.(agents.Installer)
		if !ok {
			return fmt.Errorf("此来源不支持自动安装接入")
		}
		if command[0] == "install" {
			if err = agents.Bind(ctx, c, source); err != nil {
				return err
			}
			executable, exeErr := os.Executable()
			if exeErr != nil {
				return exeErr
			}
			err = installer.Install(ctx, executable)
		} else {
			err = installer.Uninstall(ctx)
		}
		if err != nil {
			return err
		}
		if command[0] == "install" {
			fmt.Fprintf(output, "%s 接入已安装（%s）。请在 %s 中审阅并信任通知钩子，必要时重开已有会话。\n", source.Harness, source.ID, source.Harness)
		} else {
			fmt.Fprintf(output, "%s 接入已移除（%s）。配置与通知记录保留。\n", source.Harness, source.ID)
		}
		return nil
	case "run":
		if len(c.Enabled()) == 0 {
			return fmt.Errorf("请先启用至少一个通知目的地：%s", cliCommand(path, "configure"))
		}
		return worker.Run(ctx, c, log.New(os.Stderr, "Blip ", log.LstdFlags))
	case "start":
		return start(ctx, c, output)
	case "stop":
		if err := stop(ctx, c, output); errors.Is(err, errStopPending) {
			// Keep the original request-only exit contract. Restart uses stop
			// directly and must not launch another worker until it has exited.
			fmt.Fprintln(output, "已请求停止，通知进程正在完成当前操作。")
			return nil
		} else {
			return err
		}
	case "restart":
		if len(c.Enabled()) == 0 {
			return fmt.Errorf("请先启用至少一个通知目的地：%s", cliCommand(path, "configure"))
		}
		return manageLifecycle(ctx, c, "restart", output)
	case "status":
		return printStatus(c, output)
	case "doctor":
		id := ""
		if len(command) > 1 {
			id = command[1]
		}
		return printDoctor(c, id, output)
	case "test":
		if _, err := enqueueTest(ctx, c); err != nil {
			return err
		}
		fmt.Fprintf(output, "测试通知已入队。运行 %s 查看投递结果。\n", cliCommand(c.Path, "status"))
		return nil
	default:
		return fmt.Errorf("未知命令 %q；运行 blip help 查看帮助", command[0])
	}
}

func isRunning(c config.Config) (bool, error) {
	lock, available, err := (store.Queue{Dir: c.StateDir}).WorkerLock()
	if err != nil {
		return false, err
	}
	if available {
		_ = lock.Unlock()
	}
	return !available, nil
}

func start(ctx context.Context, c config.Config, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(c.Enabled()) == 0 {
		return fmt.Errorf("请先启用至少一个通知目的地：%s", cliCommand(c.Path, "configure"))
	}
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	if running {
		var status worker.Status
		if store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.Ready {
			fmt.Fprintln(output, "Blip 已在运行。")
		} else {
			fmt.Fprintf(output, "Blip 正在初始化。运行 %s 查看是否就绪。\n", cliCommand(c.Path, "status"))
		}
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(c.StateDir, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	child := exec.Command(executable, "--config", c.Path, "run")
	child.Stdout, child.Stderr = file, file
	platform.HideProcess(child)
	if err = child.Start(); err != nil {
		return fmt.Errorf("启动通知进程失败：%w", err)
	}
	childPID := child.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	for i := 0; i < 50; i++ {
		running, err = isRunning(c)
		if err != nil {
			return err
		}
		var status worker.Status
		if running && store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.PID == childPID && status.Ready {
			fmt.Fprintln(output, "Blip 已启动。")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			// A simultaneous start may have won the worker lock.
			active, _ := isRunning(c)
			if active && store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.Ready {
				fmt.Fprintln(output, "Blip 已在运行。")
				return nil
			}
			return fmt.Errorf("通知进程在启动期间退出；请检查 %s 和日志 %s", cliCommand(c.Path, "status"), filepath.Join(c.StateDir, "worker.log"))
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("通知进程尚未就绪；再次启动前请检查 %s 和日志 %s", cliCommand(c.Path, "status"), filepath.Join(c.StateDir, "worker.log"))
}
