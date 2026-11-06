//go:build !windows

package config

import "path/filepath"

func resolveHomePath(path string) (string, error) { return filepath.EvalSymlinks(path) }
