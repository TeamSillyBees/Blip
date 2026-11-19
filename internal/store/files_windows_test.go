//go:build windows

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotPublicationSurvivesOverlappingReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := WriteJSON(path, "checking"); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = reader.Close()
		close(closed)
	}()
	err = WriteJSON(path, "ready")
	<-closed
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err := ReadJSON(path, &status); err != nil || status != "ready" {
		t.Fatalf("lost status snapshot: %q %v", status, err)
	}
}
