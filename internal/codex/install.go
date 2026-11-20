package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
	"github.com/pelletier/go-toml/v2"
)

const hookMarker = "Blip: capture notification (v1)"

type installation struct {
	CodexHome  string   `json:"codex_home"`
	Commands   []string `json:"commands"`
	Executable string   `json:"executable,omitempty"`
}

func posixQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func Install(ctx context.Context, c config.Config, executable string) error {
	return InstallSource(ctx, c, executable, config.DefaultSourceID)
}

func InstallSource(ctx context.Context, c config.Config, executable, sourceID string) error {
	return InstallSourceVersion(ctx, c, executable, sourceID, 1)
}

func InstallSourceVersion(ctx context.Context, c config.Config, executable, sourceID string, eventVersion int) error {
	if err := hooksEnabled(c.CodexHome); err != nil {
		return err
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	return editHooks(ctx, c, executable, sourceID, eventVersion, true)
}

func Uninstall(ctx context.Context, c config.Config, executable, sourceID string) error {
	return editHooks(ctx, c, executable, sourceID, 0, false)
}

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

func editHooks(ctx context.Context, c config.Config, executable, sourceID string, eventVersion int, install bool) error {
	command, err := platform.HookCommand(executable, c.Path, sourceID)
	if err != nil {
		return err
	}
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
	owned := ownedHooks(c, sourceID, command, previous.Commands)
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
		desired = hookGroups(command, eventVersion)
	}
	// Keep the original bytes and group positions when registration is already
	// correct. Reinstalling one configuration must not reorder it after others.
	registrationMatches := matchesRegistration(hooks, owned, desired)
	if !registrationMatches {
		for name, groups := range hooks {
			cleaned, cleanErr := stripManaged(groups, owned)
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
		if err = os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if previous.Executable != executable || previous.CodexHome != c.CodexHome || len(previous.Commands) != 1 || previous.Commands[0] != command {
		// Publishing hooks first is retryable: the new command is known even
		// without a manifest, and the old record identifies previous entries.
		return store.WriteJSON(manifestPath, installation{CodexHome: c.CodexHome, Commands: []string{command}, Executable: executable})
	}
	return nil
}

func hookGroups(command string, eventVersion int) map[string]map[string]any {
	names := []string{"PreToolUse", "PermissionRequest"}
	if eventVersion == 2 {
		names = append(names, "Stop", "UserPromptSubmit", "Interrupt", "PostToolUse")
	}
	groups := map[string]map[string]any{}
	for _, name := range names {
		handler := map[string]any{"type": "command", "command": command, "timeout": 10, "statusMessage": hookMarker}
		if runtime.GOOS == "windows" {
			handler["commandWindows"] = command
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

func ownedHooks(c config.Config, sourceID, command string, previous []string) func(map[string]any) bool {
	known := map[string]bool{command: true}
	for _, old := range previous {
		if old != "" {
			known[old] = true
		}
	}
	// Reconstruct the previous registration even if its manifest or scripts
	// are gone. The state-directory identity separates configs sharing a home.
	state := filepath.Clean(c.StateDir)
	if runtime.GOOS == "windows" {
		state = strings.ToLower(state)
	}
	sum := sha256.Sum256([]byte(state))
	dir := filepath.Join(c.CodexHome, ".blip-hooks", fmt.Sprintf("%x", sum[:16]))
	known[`"`+filepath.ToSlash(filepath.Join(dir, "hook.cmd"))+`"`] = true
	known[posixQuote(filepath.Join(dir, "hook.sh"))] = true
	// Pre-launcher releases registered the native binary directly with the
	// config first. Match its basename, config and source, never a marker alone.
	suffix := ""
	if sourceID != config.DefaultSourceID {
		suffix = " --source " + sourceID
	}
	legacyWindows := `" --config "` + c.Path + `" hook` + suffix
	legacyPosix := "' --config " + posixQuote(c.Path) + " hook" + suffix
	matches := func(line string) bool {
		if known[line] {
			return true
		}
		if exe, ok := strings.CutSuffix(line, legacyWindows); ok && strings.HasPrefix(exe, `"`) {
			return strings.EqualFold(filepath.Base(exe[1:]), "blip.exe")
		}
		if exe, ok := strings.CutSuffix(line, legacyPosix); ok && strings.HasPrefix(exe, "'") {
			base := filepath.Base(exe[1:])
			return base == "blip" || base == "blip.exe"
		}
		return false
	}
	return func(handler map[string]any) bool {
		cmd, _ := handler["command"].(string)
		win, _ := handler["commandWindows"].(string)
		return handler["type"] == "command" && handler["statusMessage"] == hookMarker && (matches(cmd) || matches(win))
	}
}

func matchesRegistration(hooks map[string]any, owned func(map[string]any) bool, desired map[string]map[string]any) bool {
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
				if !owned(handler) {
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

func stripManaged(value any, owned func(map[string]any) bool) ([]any, error) {
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
			if owned(object) {
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
