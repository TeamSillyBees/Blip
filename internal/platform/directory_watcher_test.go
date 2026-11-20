package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWatchRegistrationDuringEventOverflow(t *testing.T) {
	w, err := NewDirectoryWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	dir := t.TempDir()
	if err := w.Add(dir); err != nil {
		t.Fatal(err)
	}
	// Leave Events unread, as during a long scan that discovers another
	// directory. Each file creation has a distinct name.
	for i := range cap(w.Events) + 16 {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-w.Errors:
		if !errors.Is(err, fsnotify.ErrEventOverflow) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing reconciliation signal after overflow")
	}
	if err := w.Add(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
