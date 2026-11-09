package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
	"github.com/pelletier/go-toml/v2"
)

const hookMarker = "Blip: capture notification (v1)"

type installation struct {
	CodexHome string          `json:"codex_home"`
	Commands  []string        `json:"commands"`
	Launcher  *launcherRecord `json:"launcher,omitempty"`
}

// Commands formats the legacy direct invocation. Install now registers a
// stable launcher; this format remains useful for compatibility and migration.
func Commands(executable, configPath string) (string, string, error) {
	for _, value := range []string{executable, configPath} {
		if strings.ContainsAny(value, "\x00\r\n\"%!^") {
			return "", "", fmt.Errorf("hook paths cannot contain quotes, percent signs, exclamation marks, carets or control characters")
		}
	}
	posix := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	windows := func(value string) string { return "\"" + value + "\"" }
	return posix(executable) + " --config " + posix(configPath) + " hook", windows(executable) + " --config " + windows(configPath) + " hook", nil
}

func Install(ctx context.Context, c config.Config, executable string) error {
	return InstallSource(ctx, c, executable, "")
}

// InstallSource keeps the legacy default source implicit; new sources carry an
// explicit ID in the launcher target and use the adapter's state directory.
func InstallSource(ctx context.Context, c config.Config, executable, sourceID string) error {
	return InstallSourceVersion(ctx, c, executable, sourceID, 1)
}

func InstallSourceVersion(ctx context.Context, c config.Config, executable, sourceID string, eventVersion int) error {
	if err := hooksEnabled(c.CodexHome); err != nil {
		return err
	}
	launcher, err := newHookLauncher(c, executable, sourceID, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	return editHooksVersion(ctx, c, launcher.commands, true, eventVersion, &launcher)
}

func Uninstall(ctx context.Context, c config.Config) error { return editHooks(ctx, c, nil, false) }

func hooksEnabled(home string) error {
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings map[string]any
	if toml.Unmarshal(data, &settings) != nil {
		return fmt.Errorf("Codex config.toml is invalid; no hook files changed")
	}
	if features, ok := settings["features"].(map[string]any); ok {
		if value, exists := features["hooks"]; exists && value == false {
			return fmt.Errorf("Codex hooks are explicitly disabled; enable hooks in Codex before installing Blip")
		}
	}
	return nil
}

func editHooks(ctx context.Context, c config.Config, commands []string, install bool) error {
	return editHooksVersion(ctx, c, commands, install, 1, nil)
}

func editHooksVersion(ctx context.Context, c config.Config, commands []string, install bool, eventVersion int, launcher *hookLauncher) error {
	if err := os.MkdirAll(c.CodexHome, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(c.CodexHome, ".blip-hooks.lock"))
	defer lock.Close()
	locked, err := lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("Codex hook configuration is busy")
	}
	manifestPath := filepath.Join(c.StateDir, "installation.json")
	var previous installation
	if err = store.ReadJSON(manifestPath, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("invalid Blip installation record")
	}
	if previous.CodexHome != "" && previous.CodexHome != c.CodexHome {
		return fmt.Errorf("configuration now points to another Codex home; uninstall using the original configuration first")
	}
	if err = checkLauncher(c, previous.Launcher, true); err != nil {
		return err
	}
	journalLauncher := mergeLauncher(previous.Launcher, launcher)
	if err = checkLauncher(c, journalLauncher, true); err != nil {
		return err
	}
	managed := map[string]bool{}
	for _, command := range append(append([]string{}, previous.Commands...), commands...) {
		if command != "" {
			managed[command] = true
		}
	}
	path := filepath.Join(c.CodexHome, "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root := map[string]any{}
	if len(data) > 0 && json.Unmarshal(data, &root) != nil {
		return fmt.Errorf("Codex hooks.json is invalid; no files changed")
	}
	if root == nil {
		return fmt.Errorf("Codex hooks.json must be an object")
	}
	hooks := map[string]any{}
	if existing, ok := root["hooks"]; ok {
		var valid bool
		hooks, valid = existing.(map[string]any)
		if !valid {
			return fmt.Errorf("Codex hooks must be an object")
		}
	}
	desired := map[string]map[string]any{}
	if install {
		desired = hookGroups(commands, eventVersion)
	}
	// Keep the original bytes and group positions when registration is already
	// correct. Reinstalling one configuration must not reorder it after others.
	registrationMatches := install && matchesRegistration(hooks, managed, desired)
	if !registrationMatches {
		for name, groups := range hooks {
			cleaned, cleanErr := stripManaged(groups, managed)
			if cleanErr != nil {
				return cleanErr
			}
			if len(cleaned) == 0 && len(groups.([]any)) > 0 {
				delete(hooks, name)
			} else {
				hooks[name] = cleaned
			}
		}
		for name, group := range desired {
			groups, _ := hooks[name].([]any)
			hooks[name] = append(groups, group)
		}
	}
	root["hooks"] = hooks
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	if registrationMatches {
		updated = data
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	changed := string(updated) != string(data) && !(len(data) == 0 && !install)
	if changed && len(data) > 0 {
		backup := path + ".blip-backup-" + time.Now().UTC().Format("20060102T150405.000000000")
		if err = store.WriteFile(backup, data); err != nil {
			return fmt.Errorf("back up Codex hooks: %w", err)
		}
	}
	if install {
		owned := []string{}
		for command := range managed {
			owned = append(owned, command)
		}
		slices.Sort(owned)
		// Keep old and new ownership through a failed update, so uninstall can recover either state.
		if err = store.WriteJSON(manifestPath, installation{CodexHome: c.CodexHome, Commands: owned, Launcher: journalLauncher}); err != nil {
			return err
		}
		if launcher != nil {
			if err = launcher.write(); err != nil {
				return err
			}
		}
	}
	if changed {
		current, readErr := os.ReadFile(path)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if string(current) != string(data) {
			return fmt.Errorf("Codex hooks changed concurrently; retry using the newer configuration")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = store.WriteFile(path, updated); err != nil {
			return err
		}
	}
	if !install {
		if err = removeLauncher(c, previous.Launcher); err != nil {
			return err
		}
		if err = os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		current := []string{commands[0]}
		if commands[1] != "" && commands[1] != commands[0] {
			current = append(current, commands[1])
		}
		var record *launcherRecord
		if launcher != nil {
			record = &launcher.record
		}
		return store.WriteJSON(manifestPath, installation{CodexHome: c.CodexHome, Commands: current, Launcher: record})
	}
	return nil
}

func hookGroups(commands []string, eventVersion int) map[string]map[string]any {
	names := []string{"PreToolUse", "PermissionRequest"}
	if eventVersion == 2 {
		names = append(names, "Stop", "UserPromptSubmit", "Interrupt", "PostToolUse")
	}
	groups := map[string]map[string]any{}
	for _, name := range names {
		handler := map[string]any{"type": "command", "command": commands[0], "timeout": 10, "statusMessage": hookMarker}
		if commands[1] != "" {
			handler["commandWindows"] = commands[1]
		}
		group := map[string]any{"hooks": []any{handler}}
		if name == "PreToolUse" && eventVersion != 2 {
			group["matcher"] = `(^|[./_:])request_user_input(_async)?$`
		}
		if name == "Interrupt" {
			handler["timeout"] = 3
		}
		groups[name] = group
	}
	return groups
}

func matchesRegistration(hooks map[string]any, managed map[string]bool, desired map[string]map[string]any) bool {
	seen := map[string]bool{}
	for name, value := range hooks {
		groups, ok := value.([]any)
		if !ok {
			return false
		}
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				return false
			}
			for _, raw := range handlers {
				handler, ok := raw.(map[string]any)
				if !ok {
					return false
				}
				command, _ := handler["command"].(string)
				windows, _ := handler["commandWindows"].(string)
				if handler["statusMessage"] != hookMarker || (!managed[command] && !managed[windows]) {
					continue
				}
				candidate := map[string]any{}
				for key, value := range group {
					candidate[key] = value
				}
				candidate["hooks"] = []any{handler}
				actual, _ := json.Marshal(candidate)
				expected, _ := json.Marshal(desired[name])
				if seen[name] || !bytes.Equal(actual, expected) {
					return false
				}
				seen[name] = true
			}
		}
	}
	return len(seen) == len(desired)
}

func stripManaged(value any, commands map[string]bool) ([]any, error) {
	groups, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("Codex hook event entries must be arrays")
	}
	result := []any{}
	for _, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Codex hook group")
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			return nil, fmt.Errorf("invalid Codex hook handlers")
		}
		kept := []any{}
		removed := false
		for _, handler := range handlers {
			object, ok := handler.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid Codex hook handler")
			}
			command, _ := object["command"].(string)
			windows, _ := object["commandWindows"].(string)
			if object["statusMessage"] == hookMarker && (commands[command] || commands[windows]) {
				removed = true
				continue
			}
			kept = append(kept, handler)
		}
		if removed && len(kept) == 0 {
			continue
		}
		group["hooks"] = kept
		result = append(result, group)
	}
	return result, nil
}
