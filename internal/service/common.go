// Package service registers and manages Blip's Windows service.
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TeamSillyBees/Blip/internal/config"
)

var ErrUnsupported = errors.New("Windows services are only supported on Windows")

type InstallOptions struct {
	ConfigPath string
	Executable string
	CodexHome  string
	StateDir   string
	Sources    []config.Source
}

type Status struct {
	Name       string `json:"name"`
	Installed  bool   `json:"installed"`
	State      string `json:"state"`
	StartType  string `json:"start_type,omitempty"`
	Account    string `json:"account,omitempty"`
	Executable string `json:"executable,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	// Captured profile paths are used when migrating a service's v1 config.
	Defaults config.RuntimeDefaults `json:"-"`
}

// Name is stable for one configuration path and does not expose that path in the
// SCM service name. Windows paths are case insensitive.
func Name(configPath string) string {
	path, err := filepath.Abs(configPath)
	if err != nil {
		path = filepath.Clean(configPath)
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	digest := sha256.Sum256([]byte(path))
	return "Blip-" + hex.EncodeToString(digest[:8])
}
