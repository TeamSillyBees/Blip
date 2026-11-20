package platform

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TeamSillyBees/Blip/internal/config"
)

// HookCommand builds a direct shell command for a client callback.
func HookCommand(executable, path, id string) (string, error) {
	if !config.ValidSourceID(id) || !filepath.IsAbs(executable) || !filepath.IsAbs(path) || strings.ContainsAny(executable+path, "\x00\r\n") {
		return "", fmt.Errorf("invalid hook source ID or absolute path")
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(executable+path, "\"") {
			return "", fmt.Errorf("invalid quote in Windows hook path")
		}
		quote = func(value string) string { return `"` + value + `"` }
		if strings.ContainsAny(executable, "%!^") {
			fixed := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Blip", "blip.exe")
			if !strings.EqualFold(filepath.Clean(executable), fixed) {
				return "", fmt.Errorf("Windows hook executable must use the fixed Blip path")
			}
			executable = `%LOCALAPPDATA%\Programs\Blip\blip.exe`
		}
	}
	command := quote(executable) + " hook --source " + id
	def, err := config.DefaultPath()
	if err == nil && (def == path || (runtime.GOOS == "windows" && strings.EqualFold(def, path))) {
		return command, nil
	}
	// cmd.exe expands percent variables even inside quoted paths. The native
	// hook accepts a lossless literal for these custom config locations.
	if runtime.GOOS == "windows" && strings.ContainsAny(path, "%!^") {
		return command + " --config-encoded " + base64.RawURLEncoding.EncodeToString([]byte(path)), nil
	}
	return command + " --config " + quote(path), nil
}
