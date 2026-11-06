//go:build windows

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TeamSillyBees/Blip/internal/store"
	"golang.org/x/sys/windows"
)

const metadataFilename = "install.json"

type installMetadata struct {
	Format     int         `json:"format"`
	Name       string      `json:"name"`
	ConfigPath string      `json:"config_path"`
	Executable string      `json:"executable"`
	CodexHome  string      `json:"codex_home"`
	StateDir   string      `json:"state_dir"`
	Grants     []aclChange `json:"grants,omitempty"`
}

func localAbsolute(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("service paths must be nonempty local paths")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, `\\`) || filepath.VolumeName(abs) == "" {
		return "", fmt.Errorf("Windows services require local drive paths, not network or device paths")
	}
	return abs, nil
}

func managedDirectory(configPath string) (string, error) {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return "", fmt.Errorf("locate Program Files: %w", err)
	}
	return filepath.Join(programFiles, "Blip", Name(configPath)), nil
}

func makeInstallMetadata(opts InstallOptions) (installMetadata, string, error) {
	var m installMetadata
	paths := []*string{&opts.ConfigPath, &opts.Executable, &opts.CodexHome, &opts.StateDir}
	for _, path := range paths {
		value, err := localAbsolute(*path)
		if err != nil {
			return m, "", err
		}
		*path = value
	}
	for _, path := range []string{opts.ConfigPath, opts.Executable} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return m, "", fmt.Errorf("service configuration and source executable must be existing regular files")
		}
	}
	for _, directory := range []string{filepath.Dir(opts.ConfigPath), opts.CodexHome, opts.StateDir} {
		if err := rejectBroadDirectory(directory); err != nil {
			return m, "", err
		}
	}
	directory, err := managedDirectory(opts.ConfigPath)
	if err != nil {
		return m, "", err
	}
	if err := noReparsePoints(directory); err != nil {
		return m, "", err
	}
	m = installMetadata{Format: 1, Name: Name(opts.ConfigPath), ConfigPath: opts.ConfigPath,
		Executable: filepath.Join(directory, "blip.exe"), CodexHome: opts.CodexHome, StateDir: opts.StateDir}
	if samePath(m.Executable, opts.Executable) {
		return m, "", fmt.Errorf("install from the regular Blip CLI, not the managed service copy")
	}
	return m, opts.Executable, nil
}

func serviceArguments(m installMetadata) []string {
	return []string{"--config", m.ConfigPath, "service", "run", "--name", m.Name,
		"--default-codex-home", m.CodexHome, "--default-state-dir", m.StateDir}
}

func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

func rejectBroadDirectory(path string) error {
	if samePath(path, filepath.VolumeName(path)+string(filepath.Separator)) {
		return fmt.Errorf("service data and configuration must use dedicated directories, not a drive root")
	}
	for _, folder := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Profile, windows.FOLDERID_ProgramFiles, windows.FOLDERID_ProgramData, windows.FOLDERID_Windows, windows.FOLDERID_UserProfiles} {
		known, err := windows.KnownFolderPath(folder, 0)
		if err == nil && samePath(path, known) {
			return fmt.Errorf("service data and configuration must use dedicated subdirectories, not a profile or system directory")
		}
	}
	return nil
}

// Installation and cleanup must never follow junctions/symlinks out of the
// protected service directory or grant permissions through redirected paths.
func noReparsePoints(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(pointer)
		if err == nil && attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("service installation does not support reparse points in managed or permission paths")
		}
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return err
		}
		if parent := filepath.Dir(current); parent == current {
			return nil
		}
	}
}

func writeMetadata(m installMetadata) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(m.Executable), metadataFilename)
	if err = store.WriteFile(path, append(data, '\n')); err != nil {
		return err
	}
	return protectInstallFile(path)
}

func readMetadata(configPath string) (installMetadata, error) {
	var m installMetadata
	abs, err := localAbsolute(configPath)
	if err != nil {
		return m, err
	}
	directory, err := managedDirectory(abs)
	if err != nil {
		return m, err
	}
	if err = noReparsePoints(directory); err != nil {
		return m, err
	}
	metadataPath := filepath.Join(directory, metadataFilename)
	if err = noReparsePoints(metadataPath); err != nil {
		return m, err
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return m, fmt.Errorf("read managed service installation record: %w", err)
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("invalid service installation record")
	}
	if m.Format != 1 || m.Name != Name(abs) || !samePath(m.ConfigPath, abs) || !samePath(m.Executable, filepath.Join(directory, "blip.exe")) {
		return m, fmt.Errorf("service installation record does not belong to this configuration")
	}
	for _, path := range []string{m.CodexHome, m.StateDir} {
		value, pathErr := localAbsolute(path)
		if pathErr != nil || !samePath(value, path) {
			return m, fmt.Errorf("invalid paths in service installation record")
		}
	}
	if err = validateRecordedGrants(m); err != nil {
		return m, err
	}
	return m, nil
}

func validateRecordedGrants(m installMetadata) error {
	// Keep the index path valid after removal so uninstall can finish cleaning
	// an installation whose optional title index no longer exists.
	allowed := []string{filepath.Dir(m.ConfigPath), m.ConfigPath, filepath.Join(m.CodexHome, "sessions"), filepath.Join(m.CodexHome, "session_index.jsonl"), m.StateDir}
	for _, grant := range m.Grants {
		valid := false
		for _, path := range allowed {
			valid = valid || samePath(grant.Path, path)
		}
		if !valid {
			return fmt.Errorf("unexpected permission path in service installation record")
		}
	}
	return nil
}
