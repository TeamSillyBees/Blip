//go:build !windows

package config

import "path/filepath"

// ResolveHomePath resolves the path of an existing source directory.
func ResolveHomePath(path string) (string, error) { return filepath.EvalSymlinks(path) }
