//go:build windows

package store

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Windows can reject a replacement while a status reader has the previous
// snapshot open. Retry the already-synced temp file for a bounded interval.
func publishSnapshot(from, to string) error {
	err := os.Rename(from, to)
	for delay := 5 * time.Millisecond; delay <= 80*time.Millisecond; delay *= 2 {
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			break
		}
		time.Sleep(delay)
		err = os.Rename(from, to)
	}
	return err
}
