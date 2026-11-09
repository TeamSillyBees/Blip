package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// InstallationStatus checks local configuration and launcher integrity only;
// Codex remains responsible for hook trust and execution.
func InstallationStatus(c config.Config, eventVersion int) string {
	var manifest installation
	err := store.ReadJSON(filepath.Join(c.StateDir, "installation.json"), &manifest)
	if errors.Is(err, os.ErrNotExist) {
		return "not_installed"
	}
	if err != nil || len(manifest.Commands) == 0 || manifest.CodexHome != c.CodexHome {
		return "invalid_manifest"
	}
	if err = checkLauncher(c, manifest.Launcher, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing_configuration"
		}
		if errors.Is(err, os.ErrPermission) {
			return "unreadable_configuration"
		}
		return "invalid_configuration"
	}
	data, err := os.ReadFile(filepath.Join(c.CodexHome, "hooks.json"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "unreadable_configuration"
		}
		return "missing_configuration"
	}
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Windows string `json:"commandWindows"`
				Marker  string `json:"statusMessage"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &root) != nil {
		return "invalid_configuration"
	}
	owned := map[string]bool{}
	for _, command := range manifest.Commands {
		owned[command] = true
	}
	names := []string{"PreToolUse", "PermissionRequest"}
	if eventVersion == 2 {
		names = append(names, "Stop", "Interrupt", "UserPromptSubmit", "PostToolUse")
	}
	for _, name := range names {
		found := false
		for _, group := range root.Hooks[name] {
			for _, hook := range group.Hooks {
				if hook.Marker == hookMarker && owned[hook.Command] && (hook.Windows == "" || owned[hook.Windows]) {
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
