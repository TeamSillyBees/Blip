package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
)

type promptStep struct {
	op    string
	value string
	err   error
}

type scriptedPrompter struct {
	t      *testing.T
	steps  []promptStep
	inputs []TextInput
	shown  strings.Builder
	result Result
}

func (p *scriptedPrompter) next(op, title, description string) promptStep {
	p.t.Helper()
	p.shown.WriteString(title + "\n" + description + "\n")
	if len(p.steps) == 0 {
		p.t.Fatalf("unexpected %s prompt %q", op, title)
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	if step.op != op {
		p.t.Fatalf("prompt %q: wanted %s, got %s", title, step.op, op)
	}
	return step
}

func (p *scriptedPrompter) Select(_ context.Context, title, description string, choices []Choice, initial string) (string, error) {
	step := p.next("select", title, description)
	if step.err != nil {
		return "", step.err
	}
	if step.value == "<default>" {
		step.value = initial
	}
	found := false
	for _, choice := range choices {
		p.shown.WriteString(choice.Label + "\n")
		found = found || choice.Value == step.value
	}
	if !found {
		p.t.Fatalf("prompt %q: choice %q is not offered", title, step.value)
	}
	return step.value, nil
}

func (p *scriptedPrompter) Input(_ context.Context, spec TextInput) (string, error) {
	step := p.next("input", spec.Title, spec.Description)
	p.inputs = append(p.inputs, spec)
	if !spec.Secret {
		p.shown.WriteString(spec.Initial + "\n")
	}
	if step.err != nil {
		return "", step.err
	}
	if step.value == "<default>" {
		step.value = spec.Initial
	}
	if spec.Validate != nil {
		if err := spec.Validate(step.value); err != nil {
			p.t.Fatalf("prompt %q rejected scripted input: %v", spec.Title, err)
		}
	}
	return step.value, nil
}

func (p *scriptedPrompter) Confirm(_ context.Context, title, description string, initial bool) (bool, error) {
	step := p.next("confirm", title, description)
	if step.err != nil {
		return false, step.err
	}
	if step.value == "<default>" {
		return initial, nil
	}
	return step.value == "yes", nil
}

func selectStep(value string) promptStep { return promptStep{op: "select", value: value} }
func inputStep(value string) promptStep  { return promptStep{op: "input", value: value} }
func confirmStep(value bool) promptStep {
	if value {
		return promptStep{op: "confirm", value: "yes"}
	}
	return promptStep{op: "confirm", value: "no"}
}

func runScript(t *testing.T, path string, steps ...promptStep) (*scriptedPrompter, string) {
	t.Helper()
	p := &scriptedPrompter{t: t, steps: steps}
	var output bytes.Buffer
	result, err := RunWithPrompter(context.Background(), path, p, &output)
	if err != nil {
		t.Fatal(err)
	}
	p.result = result
	if len(p.steps) > 0 {
		t.Fatalf("%d unused prompts", len(p.steps))
	}
	return p, output.String()
}

func TestAddAllChannelsAndMultipleDestinations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	p, output := runScript(t, path,
		selectStep("wecom"), inputStep("https://wecom.invalid/hook?key=private-wecom"),
		selectStep("add"), selectStep("feishu"), inputStep("https://feishu.invalid/private-feishu"), inputStep("private-signature"),
		selectStep("add"), selectStep("gotify"), inputStep("https://gotify.invalid"), inputStep("private-gotify-token"),
		selectStep("add"), selectStep("webhook"), inputStep("https://webhook.invalid/notify"), inputStep(""),
		selectStep("add"), inputStep("X-API-Key"), inputStep("private-header"), selectStep("done"),
		selectStep("toggle"), selectStep("3"), confirmStep(true),
		selectStep("add"), selectStep("wecom"), inputStep("https://wecom.invalid/another"),
		selectStep("save"),
	)
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Config.Destinations) != 5 {
		t.Fatalf("saved %d destinations", len(doc.Config.Destinations))
	}
	if doc.Config.DeviceName != config.DefaultDeviceName() {
		t.Fatal("first configuration did not preserve the default device name")
	}
	ids := map[string]bool{}
	for _, d := range doc.Config.Destinations {
		if ids[d.ID] || !strings.HasPrefix(d.ID, d.Kind+"-") {
			t.Fatalf("automatic destination identity is invalid: %q", d.ID)
		}
		ids[d.ID] = true
	}
	gotify := doc.Config.Destinations[2]
	if gotify.Priority != 5 || gotify.Token != "private-gotify-token" {
		t.Fatalf("gotify defaults or token changed: %#v", gotify)
	}
	if doc.Config.Destinations[3].Headers["X-API-Key"] != "private-header" || doc.Config.Destinations[3].Enabled {
		t.Fatal("custom headers or enabled state not persisted")
	}
	for _, spec := range p.inputs {
		visible := spec.Title == "设备名称" || spec.Title == "目的地 ID" || spec.Title == "消息优先级" || spec.Title == "请求头名称" || strings.Contains(spec.Title, "URL")
		if spec.Secret == visible {
			t.Errorf("input %q has incorrect visibility", spec.Title)
		}
	}
	for _, secret := range []string{"private-signature", "private-gotify-token", "private-header"} {
		if strings.Contains(output+p.shown.String(), secret) {
			t.Errorf("display leaked %q", secret)
		}
	}
	for _, hint := range []string{"输入隐藏", "环境变量", "${", "联网验证", "所有凭据", "值始终隐藏"} {
		if strings.Contains(p.shown.String(), hint) {
			t.Errorf("verbose hint remains: %q", hint)
		}
	}
	for _, spec := range p.inputs {
		if spec.Title == "设备名称" || spec.Title == "目的地 ID" || spec.Title == "消息优先级" {
			t.Errorf("new destination asked for an automatic default: %q", spec.Title)
		}
	}
	if !p.result.Saved || p.result.Apply {
		t.Fatal("save-only choice requested runtime changes")
	}
}

func TestFirstDestinationNeedsOnlyChannelURLAndSaveChoice(t *testing.T) {
	for _, action := range []string{"apply", "save"} {
		path := filepath.Join(t.TempDir(), "配置 with spaces.json")
		p, _ := runScript(t, path, selectStep("wecom"), inputStep("https://example.invalid/hook"), selectStep(action))
		if !p.result.Saved || !p.result.Changed || p.result.Apply != (action == "apply") || len(p.inputs) != 1 {
			t.Fatalf("incorrect quick setup result: %+v, %d inputs", p.result, len(p.inputs))
		}
		doc, err := config.OpenDocument(path)
		if err != nil || !doc.Config.Destinations[0].Enabled {
			t.Fatalf("new destination was not enabled: %v", err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "data")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("configuration editor performed runtime actions before returning")
		}
	}
}

func TestReaddingDestinationDoesNotReuseDeletedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	runScript(t, path, selectStep("wecom"), inputStep("https://example.invalid/first"), selectStep("save"))
	before, _ := config.OpenDocument(path)
	runScript(t, path, selectStep("delete"), selectStep("0"), confirmStep(true), selectStep("save"))
	runScript(t, path, selectStep("wecom"), inputStep("https://example.invalid/second"), selectStep("save"))
	after, _ := config.OpenDocument(path)
	if before.Config.Destinations[0].ID == after.Config.Destinations[0].ID {
		t.Fatal("new destination reused an ID whose receipts may still exist")
	}
}

func TestEditingGotifyPriorityPreservesDisabledState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	runScript(t, path, selectStep("gotify"), inputStep("https://example.invalid"), inputStep("test-token"), selectStep("toggle"), selectStep("0"), confirmStep(true), selectStep("save"))
	p, _ := runScript(t, path, selectStep("edit"), selectStep("0"), inputStep("<default>"), selectStep("keep"), inputStep("8"), selectStep("save"))
	doc, err := config.OpenDocument(path)
	if err != nil || doc.Config.Destinations[0].Priority != 8 || doc.Config.Destinations[0].Enabled || p.result.Apply {
		t.Fatalf("editing priority altered enabled state or was not saved: %v", err)
	}
}

const existingConfig = `{
  "schema_version": 1,
  "device_name": "Office-PC",
  "state_dir": "relative-state",
  "codex_home": "../codex-home",
  "poll_interval": "3s",
  "request_timeout": "9s",
  "max_attempts": 7,
  "destinations": [
    {"id":"stable","kind":"webhook","enabled":true,"url":"https://example.invalid/webhook?key=visible-url-key","token":"private-token","headers":{"X-API-Key":"private-header","X-Remove":"remove-me"}},
    {"id":"untouched","kind":"feishu","enabled":false,"url":"https://example.invalid/private-url","secret":"private-secret"}
  ]
}
`

func writeExisting(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(existingConfig), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditShowsURLAndPreservesStableIDOtherSettingsAndCredentials(t *testing.T) {
	path := writeExisting(t)
	before, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	p, output := runScript(t, path,
		selectStep("edit"), selectStep("0"), inputStep("<default>"), selectStep("keep"),
		selectStep("edit"), selectStep("X-API-Key"), inputStep("<default>"), selectStep("keep"),
		selectStep("delete"), selectStep("X-Remove"), confirmStep(true),
		selectStep("add"), inputStep("X-New"), inputStep("new-private-value"), selectStep("done"),
		selectStep("save"),
	)
	after, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	want := before.Config.Clone()
	delete(want.Destinations[0].Headers, "X-Remove")
	want.Destinations[0].Headers["X-New"] = "new-private-value"
	if !reflect.DeepEqual(after.Config, want) {
		t.Fatalf("editing changed unrelated settings\nwant: %#v\ngot: %#v", want, after.Config)
	}
	for _, spec := range p.inputs {
		if spec.Title == "目的地 ID" {
			t.Fatal("existing destination ID may not be renamed")
		}
		if spec.Secret && spec.Initial != "" {
			t.Fatal("existing credential was supplied as a form value")
		}
	}
	if p.inputs[0].Title != "Webhook URL" || p.inputs[0].Secret || p.inputs[0].Initial != before.Config.Destinations[0].URL || !strings.Contains(p.shown.String(), "visible-url-key") {
		t.Fatal("existing URL was not shown for direct editing")
	}
	for _, private := range []string{"private-token", "private-header", "remove-me", "new-private-value", "private-secret"} {
		if strings.Contains(output+p.shown.String(), private) {
			t.Errorf("display leaked %q", private)
		}
	}
}

func TestCancelAfterHeaderMutationLeavesOriginalBytesAndNoBackup(t *testing.T) {
	for _, cancel := range []promptStep{selectStep("cancel"), {op: "select", err: ErrCancelled}, {op: "select", err: context.Canceled}} {
		t.Run(cancel.value+cancel.op+errorName(cancel.err), func(t *testing.T) {
			path := writeExisting(t)
			_, output := runScript(t, path,
				selectStep("edit"), selectStep("0"), inputStep("<default>"), selectStep("keep"),
				selectStep("delete"), selectStep("X-Remove"), confirmStep(true), selectStep("done"), cancel,
			)
			content, err := os.ReadFile(path)
			if err != nil || string(content) != existingConfig {
				t.Fatalf("cancel altered original bytes: %v", err)
			}
			if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cancellation unexpectedly wrote a backup")
			}
			if !strings.Contains(output, "已取消") || strings.Contains(output, "配置已保存") {
				t.Fatalf("wrong cancellation outcome: %s", output)
			}
		})
	}
}

func errorName(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestNewConfigCancelDoesNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	p, _ := runScript(t, path, selectStep("back"), selectStep("device"), inputStep("My Device"), selectStep("cancel"))
	if p.result.Saved || p.result.Apply {
		t.Fatal("cancel requested runtime changes")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancel created configuration")
	}
}

func TestFirstDeviceNameCanBeCustomizedAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	p, _ := runScript(t, path, selectStep("back"), selectStep("device"), inputStep("家里的工作电脑"), selectStep("save"))
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Config.DeviceName != "家里的工作电脑" {
		t.Fatalf("custom device name was not saved: %q", doc.Config.DeviceName)
	}
	spec := p.inputs[0]
	hostname, err := os.Hostname()
	if err == nil && config.ValidateDeviceName(hostname) == nil && spec.Initial != hostname {
		t.Fatalf("device name default %q differs from hostname %q", spec.Initial, hostname)
	}
	if spec.Secret {
		t.Fatal("device name must be visible")
	}
	for _, invalid := range []string{"", "   ", " leading", "trailing ", "line\nbreak", strings.Repeat("名", 129)} {
		if spec.Validate(invalid) == nil {
			t.Errorf("invalid device name accepted: %q", invalid)
		}
	}
}

func TestDeviceRenamePreservesDestinationsAndOtherSettings(t *testing.T) {
	path := writeExisting(t)
	before, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := runScript(t, path, selectStep("device"), inputStep("旅行电脑"), selectStep("save"))
	after, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	want := before.Config.Clone()
	want.DeviceName = "旅行电脑"
	if !reflect.DeepEqual(after.Config, want) {
		t.Fatal("device rename changed destinations or other settings")
	}
	if len(p.inputs) != 1 || p.inputs[0].Initial != "Office-PC" || p.inputs[0].Secret {
		t.Fatal("rename did not display the existing device name")
	}
	if !strings.Contains(p.shown.String(), "设备：旅行电脑") {
		t.Fatal("configuration summary does not show the new device name")
	}
}

func TestCancelledDeviceRenameLeavesOriginalBytes(t *testing.T) {
	path := writeExisting(t)
	runScript(t, path, selectStep("device"), inputStep("Unsaved Device"), selectStep("cancel"))
	content, err := os.ReadFile(path)
	if err != nil || string(content) != existingConfig {
		t.Fatalf("cancelled device rename altered configuration: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled device rename wrote a backup")
	}
}

func TestExistingConfigWithoutDeviceNameUsesDefaultWithoutPrompt(t *testing.T) {
	path := writeExisting(t)
	withoutDevice := strings.Replace(existingConfig, "  \"device_name\": \"Office-PC\",\n", "", 1)
	if err := os.WriteFile(path, []byte(withoutDevice), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Config.DeviceName != "" {
		t.Fatal("fixture unexpectedly contains a device name")
	}
	p, _ := runScript(t, path, selectStep("save"))
	after, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	want := before.Config.Clone()
	want.DeviceName = config.DefaultDeviceName()
	if len(p.inputs) != 0 || !reflect.DeepEqual(after.Config, want) {
		t.Fatal("existing config did not use the default device name without prompting")
	}
}

func TestCancelledFirstPromptDoesNotCreateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	runScript(t, path, promptStep{op: "select", err: ErrCancelled})
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled device prompt created a configuration")
	}
}

func TestToggleDeleteAndSecretClearRequireExplicitChoices(t *testing.T) {
	path := writeExisting(t)
	runScript(t, path,
		selectStep("toggle"), selectStep("0"), confirmStep(true),
		selectStep("delete"), selectStep("0"), confirmStep(false),
		selectStep("edit"), selectStep("1"), inputStep("<default>"), selectStep("clear"),
		selectStep("delete"), selectStep("0"), confirmStep(true),
		selectStep("save"),
	)
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Config.Destinations) != 1 || doc.Config.Destinations[0].ID != "untouched" || doc.Config.Destinations[0].Secret != "" {
		t.Fatalf("incorrect staged changes: %#v", doc.Config.Destinations)
	}
}

func TestChangingBearerTokenCanResolveExistingAuthorization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"schema_version":1,"device_name":"Office-PC","destinations":[{"id":"hook","kind":"webhook","enabled":true,"url":"https://example.invalid/hook","headers":{"Authorization":"custom-secret"}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	runScript(t, path, selectStep("edit"), selectStep("0"), inputStep("<default>"), inputStep("new-bearer"), selectStep("done"), confirmStep(true), selectStep("save"))
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	d := doc.Config.Destinations[0]
	if d.Token != "" || d.Headers["Authorization"] != "custom-secret" {
		t.Fatal("auth conflict was not resolved explicitly")
	}
}

func TestValidationAndBlankSecretReplacement(t *testing.T) {
	c := controller{working: config.Config{Destinations: []config.Destination{{ID: "same"}}}}
	for _, invalid := range []string{"", "same", " space", "with space", "\x1b[31m"} {
		if c.validateID(invalid) == nil {
			t.Errorf("invalid ID accepted: %q", invalid)
		}
	}
	if c.validateID("another_1") != nil {
		t.Fatal("valid ID rejected")
	}
	if validateURL("https://x.invalid:99999/private-secret") == nil || validateURL("${MISSING_URL}") == nil {
		t.Fatal("URL validation accepted an invalid port or placeholder")
	}
	if validateToken("${LEGACY_TOKEN}", true) == nil || validateSecret("${LEGACY_SECRET}") == nil {
		t.Fatal("credential validation still accepts environment placeholders")
	}
	if validateToken("contains space", true) == nil || validatePriority("-1") == nil {
		t.Fatal("invalid scalar accepted")
	}
	p := &scriptedPrompter{t: t, steps: []promptStep{selectStep("change"), inputStep("replacement")}}
	c.prompts = p
	if _, err := c.credential(context.Background(), "Secret", "existing", false, validateSecret); err != nil {
		t.Fatal(err)
	}
	spec := p.inputs[0]
	if spec.Initial != "" || !spec.Secret || spec.Validate("") == nil {
		t.Fatal("blank replacement could silently wipe an existing secret")
	}
}

func TestRunRejectsNonTTYBeforeCreatingDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := Run(context.Background(), path, strings.NewReader(""), io.Discard, true); err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("non-TTY input was not clearly rejected: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected non-TTY call wrote a configuration")
	}
}

func TestAccessibleEOFAbortsInsteadOfAcceptingDefault(t *testing.T) {
	for _, input := range []string{"", "invalid\n", "\x03"} {
		p := huhPrompter{input: strings.NewReader(input), output: io.Discard, accessible: true}
		_, err := p.Select(context.Background(), "save?", "", []Choice{{"save", "save"}, {"cancel", "cancel"}}, "save")
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("input %q: got %v", input, err)
		}
	}
}

func TestAccessibleVisibleInputShowsAndKeepsCurrentURL(t *testing.T) {
	const address = "https://example.invalid/hook?key=visible-url-key"
	var output bytes.Buffer
	p := huhPrompter{input: strings.NewReader("\n"), output: &output, accessible: true}
	value, err := p.Input(context.Background(), TextInput{Title: "Webhook URL", Initial: address, Validate: validateURL})
	if err != nil || value != address || !strings.Contains(output.String(), address) {
		t.Fatalf("accessible input did not show or retain current URL: value=%q, err=%v, output=%q", value, err, output.String())
	}
}

func TestAccessibleModeResolvesHuhTerminalFallback(t *testing.T) {
	for _, test := range []struct {
		term       string
		explicit   bool
		accessible bool
	}{
		{term: "dumb", accessible: true},
		{term: "xterm", explicit: true, accessible: true},
		{term: "xterm"},
		{},
	} {
		t.Run(test.term+strconv.FormatBool(test.explicit), func(t *testing.T) {
			t.Setenv("TERM", test.term)
			p := newHuhPrompter(strings.NewReader(""), io.Discard, test.explicit)
			if p.accessible != test.accessible {
				t.Fatalf("resolved accessible=%v, want %v", p.accessible, test.accessible)
			}
		})
	}
	// An implicit fallback must retain descriptions and treat EOF as cancellation,
	// rather than letting Huh's Form.runAccessible silently accept a default.
	t.Setenv("TERM", "dumb")
	var output bytes.Buffer
	p := newHuhPrompter(strings.NewReader(""), &output, false)
	_, err := p.Select(context.Background(), "Save?", "staged configuration summary", []Choice{{"Save", "save"}, {"Cancel", "cancel"}}, "save")
	if !errors.Is(err, ErrCancelled) || !strings.Contains(output.String(), "staged configuration summary") {
		t.Fatalf("implicit accessibility lost cancellation or summary: err=%v, output=%q", err, output.String())
	}
}

func TestAccessibleContextCancellationUnblocksPrompt(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := huhPrompter{input: reader, output: io.Discard, accessible: true}
	done := make(chan error, 1)
	go func() {
		_, err := p.Confirm(ctx, "Save?", "", true)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not release accessible prompt")
	}
}
