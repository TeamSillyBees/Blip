package antigravity

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyServiceWAL(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), SummaryFilename)
	client, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec("PRAGMA journal_mode=WAL; CREATE TABLE messages (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PrepareServiceWAL(ctx, path); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("missing file for the service read grant: %v", err)
		}
	}
	// A separate writer stays connected while the service attaches. The
	// read-only attributes reproduce its lack of write access without UAC.
	client, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Exec("INSERT INTO messages VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		file := path + suffix
		if err := os.Chmod(file, 0400); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(file, 0600)
	}
	reader, err := OpenSummaryDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for want := 1; want <= 2; want++ {
		var n int
		if err := reader.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&n); err != nil || n != want {
			t.Fatalf("read live WAL: count=%d, want %d: %v", n, want, err)
		}
		if want == 1 {
			if _, err := client.Exec("INSERT INTO messages VALUES (2)"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := reader.ExecContext(ctx, "DELETE FROM messages"); err == nil {
		t.Fatal("reader was able to modify a client database")
	}
}
