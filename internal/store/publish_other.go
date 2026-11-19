//go:build !windows

package store

import "os"

func publishSnapshot(from, to string) error {
	return os.Rename(from, to)
}
