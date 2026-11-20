package agents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/platform"
)

func (a *antigravityAdapter) installer() (jsonHookInstaller, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return jsonHookInstaller{}, err
	}
	return jsonHookInstaller{config: a.config, source: a.source, path: filepath.Join(home, ".gemini", "config", "hooks.json"), recordName: a.groupName() + ".json"}, nil
}

func (a *antigravityAdapter) groupName() string {
	path := filepath.Clean(a.config.Path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return "blip-" + a.source.ID + "-" + event.ID(path)[:16]
}

func (a *antigravityAdapter) Install(ctx context.Context, executable string) error {
	i, err := a.installer()
	if err != nil {
		return err
	}
	command, err := platform.HookCommand(executable, a.config.Path, a.source.ID)
	if err != nil {
		return err
	}
	handler := func(name string) []any {
		return []any{map[string]any{"type": "command", "command": command + " --antigravity-event " + name, "timeout": 5}}
	}
	group := map[string]any{
		"PreInvocation": handler("PreInvocation"),
		"PreToolUse":    []any{map[string]any{"matcher": "^(ask_question|ask_permission)$", "hooks": handler("PreToolUse")}},
		"Stop":          handler("Stop"),
	}
	return i.editSettings(ctx, []jsonHookEntry{{Event: a.groupName(), Hook: group}}, true, a.updateGroup)
}

func (a *antigravityAdapter) Uninstall(ctx context.Context) error {
	i, err := a.installer()
	if err != nil {
		return err
	}
	return i.editSettings(ctx, nil, false, a.updateGroup)
}

// The enabled toggle belongs to the user, including across reinstall. Other
// edits to a managed group must be resolved before Blip can replace it.
func matchesAntigravityGroup(group map[string]any, name string, entries []jsonHookEntry) bool {
	definition := maps.Clone(group)
	delete(definition, "enabled")
	for _, entry := range entries {
		if entry.Event == name && jsonEqual(entry.Hook, definition) {
			return true
		}
	}
	return false
}

func (a *antigravityAdapter) updateGroup(root map[string]any, previous, entries []jsonHookEntry, install bool) error {
	name := a.groupName()
	current, exists := root[name]
	group, _ := current.(map[string]any)
	if exists && !matchesAntigravityGroup(group, name, previous) && !matchesAntigravityGroup(group, name, entries) {
		return fmt.Errorf("Antigravity hook group %q was edited or already belongs to another registration", name)
	}
	if !install {
		delete(root, name)
		return nil
	}
	next := maps.Clone(entries[0].Hook)
	if enabled, ok := group["enabled"]; ok {
		if _, ok := enabled.(bool); !ok {
			return fmt.Errorf("Antigravity hook group %q has an invalid enabled setting", name)
		}
		next["enabled"] = enabled
	}
	root[name] = next
	return nil
}

func (a *antigravityAdapter) InstallationStatus() string {
	i, err := a.installer()
	if err != nil {
		return "invalid_configuration"
	}
	m, err := i.manifest()
	if err != nil {
		if m.Format == 1 && filepath.IsAbs(m.Path) && m.Path != i.path {
			return "unknown" // the service runs under a different user profile
		}
		return "invalid_manifest"
	}
	if len(m.Entries) == 0 {
		return "not_installed"
	}
	root, _, _, err := readHookSettings(i.path)
	if errors.Is(err, os.ErrPermission) {
		return "unknown"
	}
	if err != nil {
		return "invalid_configuration"
	}
	name := a.groupName()
	group, _ := root[name].(map[string]any)
	if !matchesAntigravityGroup(group, name, m.Entries) {
		return "missing_configuration"
	}
	if setting, exists := group["enabled"]; exists {
		enabled, ok := setting.(bool)
		if !ok {
			return "invalid_configuration"
		}
		if !enabled {
			return "disabled_by_harness"
		}
	}
	return "configured_unverified"
}
