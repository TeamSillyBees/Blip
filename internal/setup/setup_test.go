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
	if err := RunWithPrompter(context.Background(), path, p, &output); err != nil {
		t.Fatal(err)
	}
	if len(p.steps) > 0 {
		t.Fatalf("%d unused prompts", len(p.steps))
	}
	return p, output.String()
}

func TestAddAllChannelsAndMultipleDestinations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	p, output := runScript(t, path,
		selectStep("wecom"), inputStep("work"), inputStep("https://wecom.invalid/hook?key=private-wecom"), confirmStep(true),
		selectStep("add"), selectStep("feishu"), inputStep("team"), inputStep("https://feishu.invalid/private-feishu"), inputStep("private-signature"), confirmStep(true),
		selectStep("add"), selectStep("gotify"), inputStep("phone"), inputStep("https://gotify.invalid"), inputStep("${GOTIFY_TOKEN}"), inputStep("<default>"), confirmStep(true),
		selectStep("add"), selectStep("webhook"), inputStep("automation"), inputStep("${CUSTOM_WEBHOOK_URL}"), inputStep(""),
		selectStep("add"), inputStep("X-API-Key"), inputStep("private-header"), selectStep("done"), confirmStep(false),
		selectStep("add"), selectStep("wecom"), inputStep("other-work"), inputStep("https://wecom.invalid/another"), confirmStep(true),
		selectStep("save"), confirmStep(true),
	)
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Config.Destinations) != 5 {
		t.Fatalf("saved %d destinations", len(doc.Config.Destinations))
	}
	gotify := doc.Config.Destinations[2]
	if gotify.Priority != 5 || gotify.Token != "${GOTIFY_TOKEN}" {
		t.Fatalf("gotify defaults or env reference changed: %#v", gotify)
	}
	if doc.Config.Destinations[3].Headers["X-API-Key"] != "private-header" || doc.Config.Destinations[3].Enabled {
		t.Fatal("custom headers or enabled state not persisted")
	}
	for _, spec := range p.inputs {
		if spec.Title != "目的地 ID" && spec.Title != "消息优先级" && spec.Title != "请求头名称" && !spec.Secret {
			t.Errorf("credential input %q is not masked", spec.Title)
		}
	}
	for _, secret := range []string{"private-wecom", "private-feishu", "private-signature", "${GOTIFY_TOKEN}", "${CUSTOM_WEBHOOK_URL}", "private-header"} {
		if strings.Contains(output+p.shown.String(), secret) {
			t.Errorf("display leaked %q", secret)
		}
	}
	for _, command := range []string{"blip install codex", "blip start", "blip test", "blip stop"} {
		if !strings.Contains(output, command) {
			t.Errorf("missing follow-up command %q", command)
		}
	}
}

const existingConfig = `{
  "schema_version": 1,
  "state_dir": "relative-state",
  "codex_home": "../codex-home",
  "poll_interval": "3s",
  "request_timeout": "9s",
  "max_attempts": 7,
  "destinations": [
    {"id":"stable","kind":"webhook","enabled":true,"url":"${PRIVATE_URL}","token":"${PRIVATE_TOKEN}","headers":{"X-API-Key":"${PRIVATE_HEADER}","X-Remove":"remove-me"}},
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

func TestEditPreservesStableIDOtherSettingsAndReferences(t *testing.T) {
	path := writeExisting(t)
	before, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	p, output := runScript(t, path,
		selectStep("edit"), selectStep("0"), selectStep("keep"), selectStep("keep"),
		selectStep("edit"), selectStep("X-API-Key"), inputStep("<default>"), selectStep("keep"),
		selectStep("delete"), selectStep("X-Remove"), confirmStep(true),
		selectStep("add"), inputStep("X-New"), inputStep("new-private-value"), selectStep("done"), confirmStep(true),
		selectStep("save"), confirmStep(true),
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
	for _, private := range []string{"${PRIVATE_URL}", "${PRIVATE_TOKEN}", "${PRIVATE_HEADER}", "remove-me", "new-private-value", "private-secret"} {
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
				selectStep("edit"), selectStep("0"), selectStep("keep"), selectStep("keep"),
				selectStep("delete"), selectStep("X-Remove"), confirmStep(true), selectStep("done"), confirmStep(false), cancel,
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

func TestNewConfigCancelAndDeclinedSaveDoNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	runScript(t, path, selectStep("back"), selectStep("save"), confirmStep(false), selectStep("cancel"))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancel created configuration")
	}
}

func TestToggleDeleteAndSecretClearRequireExplicitChoices(t *testing.T) {
	path := writeExisting(t)
	runScript(t, path,
		selectStep("toggle"), selectStep("0"), confirmStep(true),
		selectStep("delete"), selectStep("0"), confirmStep(false),
		selectStep("edit"), selectStep("1"), selectStep("keep"), selectStep("clear"), confirmStep(false),
		selectStep("delete"), selectStep("0"), confirmStep(true),
		selectStep("save"), confirmStep(true),
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
	data := `{"schema_version":1,"destinations":[{"id":"hook","kind":"webhook","enabled":true,"url":"https://example.invalid/hook","headers":{"Authorization":"custom-secret"}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	runScript(t, path, selectStep("edit"), selectStep("0"), selectStep("keep"), inputStep("new-bearer"), selectStep("done"), confirmStep(true), confirmStep(true), selectStep("save"), confirmStep(true))
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
	if validateURL("https://x.invalid:99999/private-secret") == nil || validateURL("${MISSING_URL}") != nil {
		t.Fatal("URL validation does not handle invalid ports or unresolved references")
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
	if err := Run(context.Background(), path, strings.NewReader(""), io.Discard, true); err == nil || !strings.Contains(err.Error(), "TTY") {
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
