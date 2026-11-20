package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
)

// This helper owns configuration entries, not Claude-family event semantics.
// Ownership includes event, matcher and the entire command handler. A changed
// owned command is a conflict, not permission to delete the user's edit.
type jsonHookEntry struct {
	Event   string         `json:"event"`
	Matcher string         `json:"matcher,omitempty"`
	Hook    map[string]any `json:"hook"`
}
type jsonHookManifest struct {
	Format  int             `json:"format"`
	Path    string          `json:"path"`
	Entries []jsonHookEntry `json:"entries"`
}
type jsonHookInstaller struct {
	config     config.Config
	source     config.Source
	path       string
	recordName string
}

func (i jsonHookInstaller) manifestPath() string {
	if i.recordName != "" {
		return filepath.Join(StateDir(i.config, i.source), i.recordName)
	}
	return filepath.Join(StateDir(i.config, i.source), "installation.json")
}
func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func readHookSettings(path string) (map[string]any, []byte, os.FileMode, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil, 0600, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return nil, nil, 0, fmt.Errorf("hook settings must be a regular file no larger than 4 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, err
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber() // preserve numeric values in unrelated user settings
	if decoder.Decode(&root) != nil || root == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, nil, 0, fmt.Errorf("invalid hook settings; no configuration changed")
	}
	return root, data, info.Mode().Perm(), nil
}

func (i jsonHookInstaller) manifest() (jsonHookManifest, error) {
	var m jsonHookManifest
	err := store.ReadJSON(i.manifestPath(), &m)
	if errors.Is(err, os.ErrNotExist) {
		return jsonHookManifest{Format: 1, Path: i.path}, nil
	}
	if err != nil || m.Format != 1 || m.Path != i.path {
		return m, fmt.Errorf("invalid hook installation record; preserve it for recovery")
	}
	for _, entry := range m.Entries {
		if entry.Event == "" || entry.Hook == nil {
			return m, fmt.Errorf("invalid hook ownership entry")
		}
	}
	return m, nil
}

func (i jsonHookInstaller) edit(ctx context.Context, entries []jsonHookEntry, install bool) error {
	return i.editSettings(ctx, entries, install, updateClaudeHookSettings)
}

func (i jsonHookInstaller) editSettings(ctx context.Context, entries []jsonHookEntry, install bool, update func(map[string]any, []jsonHookEntry, []jsonHookEntry, bool) error) error {
	if !install {
		if _, err := os.Stat(i.manifestPath()); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(i.path), 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(filepath.Dir(i.path), ".blip-hooks.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = lock.Close()
		if err == nil {
			err = fmt.Errorf("hook settings are busy")
		}
		return err
	}
	defer lock.Close()
	previous, err := i.manifest()
	if err != nil || (!install && len(previous.Entries) == 0) {
		return err
	}
	root, original, mode, err := readHookSettings(i.path)
	if err != nil {
		return err
	}
	if err := update(root, previous.Entries, entries, install); err != nil {
		return err
	}
	owned := append(append([]jsonHookEntry{}, previous.Entries...), entries...)
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	if install {
		// Journal both command versions before editing, so interrupted upgrades
		// can recover ownership even if the executable path changed.
		journal := jsonHookManifest{Format: 1, Path: i.path, Entries: uniqueHookEntries(owned)}
		if err := store.WriteJSON(i.manifestPath(), journal); err != nil {
			return err
		}
	}
	if !bytes.Equal(original, updated) && !(len(original) == 0 && !install) {
		if len(original) > 0 {
			backup := i.path + ".blip-backup-" + time.Now().UTC().Format("20060102T150405.000000000")
			if err := store.WriteFile(backup, original); err != nil {
				return fmt.Errorf("back up hook settings: %w", err)
			}
		}
		_, current, _, err := readHookSettings(i.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, original) {
			return fmt.Errorf("hook settings changed concurrently; retry using the newer settings")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := store.WriteFileMode(i.path, updated, mode); err != nil {
			return err
		}
	}
	if install {
		return store.WriteJSON(i.manifestPath(), jsonHookManifest{Format: 1, Path: i.path, Entries: entries})
	}
	if err := os.Remove(i.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func updateClaudeHookSettings(root map[string]any, previous, entries []jsonHookEntry, install bool) error {
	if install && (root["disableAllHooks"] == true || root["allowManagedHooksOnly"] == true) {
		return fmt.Errorf("hooks are explicitly disabled or restricted to managed hooks; no settings changed")
	}
	hooks := map[string]any{}
	if value, exists := root["hooks"]; exists {
		var valid bool
		hooks, valid = value.(map[string]any)
		if !valid {
			return fmt.Errorf("hooks must be an object")
		}
	}
	owned := append(append([]jsonHookEntry{}, previous...), entries...)
	for name, value := range hooks {
		groups, valid := value.([]any)
		if !valid {
			return fmt.Errorf("hook event groups must be arrays")
		}
		keptGroups := []any{}
		for _, value := range groups {
			group, valid := value.(map[string]any)
			if !valid {
				return fmt.Errorf("hook group must be an object")
			}
			handlers, valid := group["hooks"].([]any)
			if !valid {
				return fmt.Errorf("hook handlers must be an array")
			}
			keptHandlers := []any{}
			removed := false
			for _, raw := range handlers {
				handler, valid := raw.(map[string]any)
				if !valid {
					return fmt.Errorf("hook handler must be an object")
				}
				match := false
				for _, entry := range owned {
					matcher, _ := group["matcher"].(string)
					if name == entry.Event && matcher == entry.Matcher && jsonEqual(handler, entry.Hook) {
						match = true
						break
					}
				}
				if !match {
					for _, entry := range previous {
						if sameHookCommand(handler, entry.Hook) {
							return fmt.Errorf("Blip hook entry was edited or moved; restore the recorded entry before updating or uninstalling")
						}
					}
					keptHandlers = append(keptHandlers, handler)
				} else {
					removed = true
				}
			}
			if removed && len(keptHandlers) == 0 {
				continue
			}
			group["hooks"] = keptHandlers
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 && len(groups) > 0 {
			delete(hooks, name)
		} else {
			hooks[name] = keptGroups
		}
	}
	if install {
		for _, entry := range entries {
			group := map[string]any{"hooks": []any{entry.Hook}}
			if entry.Matcher != "" {
				group["matcher"] = entry.Matcher
			}
			groups, _ := hooks[entry.Event].([]any)
			hooks[entry.Event] = append(groups, group)
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	return nil
}

func sameHookCommand(a, b map[string]any) bool {
	left, _ := a["command"].(string)
	right, _ := b["command"].(string)
	return left != "" && left == right && jsonEqual(a["args"], b["args"])
}
func uniqueHookEntries(entries []jsonHookEntry) []jsonHookEntry {
	result := []jsonHookEntry{}
	for _, entry := range entries {
		found := false
		for _, previous := range result {
			if jsonEqual(entry, previous) {
				found = true
				break
			}
		}
		if !found {
			result = append(result, entry)
		}
	}
	return result
}

func (i jsonHookInstaller) status() string {
	m, err := i.manifest()
	if err != nil {
		return "invalid_manifest"
	}
	if len(m.Entries) == 0 {
		return "not_installed"
	}
	root, _, _, err := readHookSettings(i.path)
	if err != nil {
		return "invalid_configuration"
	}
	if root["disableAllHooks"] == true || root["allowManagedHooksOnly"] == true {
		return "disabled_by_harness"
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, entry := range m.Entries {
		found := false
		groups, _ := hooks[entry.Event].([]any)
		for _, raw := range groups {
			group, _ := raw.(map[string]any)
			matcher, _ := group["matcher"].(string)
			if matcher != entry.Matcher {
				continue
			}
			handlers, _ := group["hooks"].([]any)
			for _, handler := range handlers {
				if jsonEqual(handler, entry.Hook) {
					found = true
				}
			}
		}
		if !found {
			return "missing_configuration"
		}
	}
	return "configured_unverified"
}
