package platform

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestExecutableSyncPreservesVerifiedNewestVersion(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Programs", "Blip", "blip.exe")
	install := func(version string) error {
		path := filepath.Join(dir, "node-"+version, "blip.exe")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(version), 0700); err != nil {
			return err
		}
		return syncExecutable(context.Background(), path, target, version, executableHash([]byte(version)))
	}
	check := func(want string) {
		t.Helper()
		got, err := os.ReadFile(target)
		if err != nil || string(got) != want {
			t.Fatalf("fixed binary=%q, want %q: %v", got, want, err)
		}
	}
	var group sync.WaitGroup
	for _, version := range []string{"0.9.0", "0.10.0", "0.10.0-rc.2", "0.10.0-rc.10"} {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := install(version); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	check("0.10.0")
	if err := install("0.9.0"); err != nil {
		t.Fatal(err)
	}
	check("0.10.0")
	if err := syncExecutable(context.Background(), filepath.Join(dir, "node-0.9.0", "blip.exe"), target, "0.11.0", strings.Repeat("0", 64)); err == nil {
		t.Fatal("installed a binary with a bad digest")
	}
	check("0.10.0")
	if runtime.GOOS == "windows" {
		handle, err := os.Open(target)
		if err != nil {
			t.Fatal(err)
		}
		err = install("1.0.0")
		handle.Close()
		if err == nil {
			t.Fatal("unexpectedly replaced an open Windows executable")
		}
		check("0.10.0")
		if err := install("0.9.0"); err != nil {
			t.Fatal(err)
		}
		check("0.10.0")
	}
	if err := install("1.0.0"); err != nil {
		t.Fatal(err)
	}
	check("1.0.0")
}
