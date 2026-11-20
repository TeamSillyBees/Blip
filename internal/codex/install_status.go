package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// InstallationStatus verifies the native target and the complete event definitions.
// Codex owns the separate decision to trust and execute them.
func InstallationStatus(c config.Config, sourceID string, eventVersion int) string {
	var manifest installation
	err := store.ReadJSON(filepath.Join(c.StateDir, "installation.json"), &manifest)
	if errors.Is(err, os.ErrNotExist) {
		return "not_installed"
	}
	if err != nil || len(manifest.Commands) == 0 || manifest.CodexHome != c.CodexHome {
		return "invalid_manifest"
	}
	if manifest.Executable == "" {
		return "missing_configuration" // a pre-fixed-path registration needs reinstall
	}
	if info, err := os.Stat(manifest.Executable); err != nil || !info.Mode().IsRegular() {
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
		Hooks map[string]any `json:"hooks"`
	}
	if json.Unmarshal(data, &root) != nil {
		return "invalid_configuration"
	}
	command, err := platform.HookCommand(manifest.Executable, c.Path, sourceID)
	if err != nil || len(manifest.Commands) != 1 || manifest.Commands[0] != command {
		return "invalid_manifest"
	}
	if !matchesRegistration(root.Hooks, ownedHooks(c, sourceID, command, nil), hookGroups(command, eventVersion)) {
		return "missing_configuration"
	}
	return "configured_unverified"
}
