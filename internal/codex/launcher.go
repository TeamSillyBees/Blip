package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// The registered entry stays byte-for-byte stable when the executable moves.
// Only run.cmd/run.sh contains installation-specific executable and arguments.
// Neither script reads stdin before the native hook receives it.
const windowsLauncher = "@echo off\r\nrem Blip managed hook entry\r\nsetlocal DisableDelayedExpansion\r\nchcp 65001 >nul 2>&1\r\nif not exist \"%~dp0run.cmd\" exit /b 0\r\n\"%~dp0run.cmd\"\r\nexit /b 0\r\n"

const posixLauncher = `#!/bin/sh
# Blip managed hook entry
case "$0" in
  */*) blip_hook_dir=${0%/*} ;;
  *) exit 0 ;;
esac
[ -r "$blip_hook_dir/run.sh" ] || exit 0
. "$blip_hook_dir/run.sh"
exit 0
`

type launcherFile struct {
	Name   string   `json:"name"`
	Hashes []string `json:"sha256"`
}

type launcherRecord struct {
	Dir   string         `json:"dir"`
	Files []launcherFile `json:"files"`
}

type hookLauncher struct {
	record   launcherRecord
	commands []string
	content  map[string][]byte
}

func launcherDir(c config.Config) (string, error) {
	state, err := filepath.Abs(c.StateDir)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		state = strings.ToLower(state)
	}
	id := sha256.Sum256([]byte(state))
	// Each source already has a separate state directory. Include that identity
	// so independent Blip configurations can also share the same Codex home.
	return filepath.Abs(filepath.Join(c.CodexHome, ".blip-hooks", hex.EncodeToString(id[:16])))
}

func contentHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func posixQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func newHookLauncher(c config.Config, executable, sourceID string, windows bool) (hookLauncher, error) {
	l := hookLauncher{content: map[string][]byte{}}
	dir, err := launcherDir(c)
	if err != nil {
		return l, err
	}
	l.record.Dir = dir
	values := []string{executable, c.Path}
	for n, value := range values {
		absolute, err := filepath.Abs(value)
		if err != nil {
			return l, err
		}
		if strings.ContainsAny(absolute, "\x00\r\n") || (windows && strings.Contains(absolute, "\"")) {
			return l, fmt.Errorf("hook paths contain unsupported quotes or control characters")
		}
		values[n] = absolute
	}
	if sourceID != "" && !config.ValidSourceID(sourceID) {
		return l, fmt.Errorf("invalid hook source ID")
	}
	suffix := ""
	if sourceID != "" {
		suffix = " --source " + sourceID
	}
	if windows {
		// This path is evaluated by the caller's cmd /C, before our script can
		// disable delayed expansion. Runtime paths below are batch literals.
		if strings.ContainsAny(dir, "\x00\r\n\"%!^") {
			return l, fmt.Errorf("Codex hook entry path cannot contain quotes, percent signs, exclamation marks, carets or control characters")
		}
		quote := func(value string) string { return "\"" + strings.ReplaceAll(value, "%", "%%") + "\"" }
		l.content["hook.cmd"] = []byte(windowsLauncher)
		l.content["run.cmd"] = []byte("@echo off\r\nrem Blip managed hook target\r\nsetlocal DisableDelayedExpansion\r\n" + quote(values[0]) + " --config " + quote(values[1]) + " hook" + suffix + " >nul 2>&1\r\nexit /b 0\r\n")
		command := "\"" + filepath.ToSlash(filepath.Join(dir, "hook.cmd")) + "\""
		// A native default also works with Windows runners that don't implement
		// commandWindows. Keep both fields stable on supported Codex versions.
		l.commands = []string{command, command}
	} else {
		if strings.ContainsAny(dir, "\x00\r\n") {
			return l, fmt.Errorf("Codex hook entry path contains control characters")
		}
		l.content["hook.sh"] = []byte(posixLauncher)
		l.content["run.sh"] = []byte("#!/bin/sh\n# Blip managed hook target\n" + posixQuote(values[0]) + " --config " + posixQuote(values[1]) + " hook" + suffix + " >/dev/null 2>&1\nexit 0\n")
		l.commands = []string{posixQuote(filepath.Join(dir, "hook.sh")), ""}
	}
	for name, data := range l.content {
		l.record.Files = append(l.record.Files, launcherFile{Name: name, Hashes: []string{contentHash(data)}})
	}
	slices.SortFunc(l.record.Files, func(a, b launcherFile) int { return strings.Compare(a.Name, b.Name) })
	return l, nil
}

// mergeLauncher journals old and new hashes before replacement. An interrupted
// update can then retry or uninstall either complete version of each file.
func mergeLauncher(previous *launcherRecord, next *hookLauncher) *launcherRecord {
	if previous == nil && next == nil {
		return nil
	}
	m := &launcherRecord{}
	files := map[string][]string{}
	if previous != nil {
		m.Dir = previous.Dir
		for _, file := range previous.Files {
			files[file.Name] = append([]string{}, file.Hashes...)
		}
	}
	if next != nil {
		m.Dir = next.record.Dir
		for _, file := range next.record.Files {
			files[file.Name] = append(files[file.Name], file.Hashes...)
		}
	}
	for name, hashes := range files {
		slices.Sort(hashes)
		m.Files = append(m.Files, launcherFile{Name: name, Hashes: slices.Compact(hashes)})
	}
	slices.SortFunc(m.Files, func(a, b launcherFile) int { return strings.Compare(a.Name, b.Name) })
	return m
}

func checkLauncher(c config.Config, record *launcherRecord, allowMissing bool) error {
	if record == nil { // Legacy installations have only direct commands.
		return nil
	}
	dir, err := launcherDir(c)
	if err != nil {
		return err
	}
	if record.Dir != dir || len(record.Files) == 0 {
		return fmt.Errorf("invalid Blip hook launcher record")
	}
	for _, path := range []string{filepath.Dir(dir), dir} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && allowMissing {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Blip hook launcher directory is not a regular directory")
		}
	}
	for _, file := range record.Files {
		switch file.Name {
		case "hook.cmd", "run.cmd", "hook.sh", "run.sh":
		default:
			return fmt.Errorf("invalid Blip hook launcher file")
		}
		if len(file.Hashes) == 0 {
			return fmt.Errorf("missing Blip hook launcher hash")
		}
		path := filepath.Join(dir, file.Name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && allowMissing {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Blip hook launcher is not a regular file: %s", path)
		}
		if !allowMissing && runtime.GOOS != "windows" && file.Name == "hook.sh" && info.Mode().Perm()&0100 == 0 {
			return fmt.Errorf("Blip hook entry is not executable: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !slices.Contains(file.Hashes, contentHash(data)) {
			return fmt.Errorf("Blip hook launcher was edited; restore it before updating or uninstalling: %s", path)
		}
	}
	return nil
}

func (l hookLauncher) write() error {
	// Publish the target before exposing a new entry point.
	for _, name := range []string{"run.cmd", "run.sh", "hook.cmd", "hook.sh"} {
		data, exists := l.content[name]
		if !exists {
			continue
		}
		path := filepath.Join(l.record.Dir, name)
		current, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mode := os.FileMode(0600)
		if name == "hook.sh" {
			mode = 0700
		}
		if bytes.Equal(current, data) {
			if err = os.Chmod(path, mode); err != nil {
				return err
			}
			continue
		}
		if err = store.WriteFileMode(path, data, mode); err != nil {
			return err
		}
	}
	return nil
}

func removeLauncher(c config.Config, record *launcherRecord) error {
	if record == nil {
		return nil
	}
	if err := checkLauncher(c, record, true); err != nil {
		return err
	}
	for _, file := range record.Files {
		if err := os.Remove(filepath.Join(record.Dir, file.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Do not recursively remove the directory: unrelated user files survive.
	if entries, err := os.ReadDir(record.Dir); err == nil && len(entries) == 0 {
		return os.Remove(record.Dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
