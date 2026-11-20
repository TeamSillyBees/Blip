package worker

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/fsnotify/fsnotify"
)

// Watch the parent directories because stop requests and diagnostics are
// published by atomic rename. A stop cancels outstanding HTTP and collection.
func watchWorkerState(ctx context.Context, c config.Config, sources []config.Source, cancel context.CancelFunc, diagnostics chan<- int, logger *log.Logger) {
	stopPath := filepath.Join(c.StateDir, "stop")
	checkStop := func() {
		if _, err := os.Stat(stopPath); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				logger.Printf("cannot check stop request: %v", err)
			}
			cancel()
		}
	}
	refresh := func(index int) {
		if len(sources) > 0 {
			select {
			case diagnostics <- index:
			case <-ctx.Done():
			}
		}
	}
	reported := false
	for ctx.Err() == nil {
		err := watchStateDirectory(ctx, c, sources, stopPath, checkStop, refresh)
		if ctx.Err() != nil {
			return
		}
		if !reported {
			logger.Printf("worker state watcher unavailable, retrying: %v", err)
			reported = true
		}
		checkStop()
		refresh(-1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func watchStateDirectory(ctx context.Context, c config.Config, sources []config.Source, stopPath string, checkStop func(), refresh func(int)) error {
	w, err := platform.NewDirectoryWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dirs := map[string]bool{filepath.Dir(stopPath): true}
	if err := w.Add(filepath.Dir(stopPath)); err != nil {
		return err
	}
	files := make(map[string]int, len(sources)*2)
	for i, s := range sources {
		path := agents.DiagnosticPath(c, s.ID)
		dir := filepath.Dir(path)
		if s.Enabled {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
		}
		if err := w.Add(dir); err != nil {
			if !s.Enabled && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		dirs[dir] = true
		files[path] = i
		files[agents.CheckpointFaultPath(c, s.ID)] = i
	}
	checkStop()
	refresh(-1) // Cover changes made before watch registration.
	// The CLI waits up to ten seconds for a stop. Also honor a durable stop
	// on mounts that silently lose their native notifications.
	safety := time.NewTicker(5 * time.Second)
	defer safety.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-safety.C:
			checkStop()
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("worker state watch closed")
			}
			// Re-register the directories and refresh durable state after a gap.
			return err
		case change, ok := <-w.Events:
			if !ok {
				return errors.New("worker state watch closed")
			}
			if dirs[change.Name] && (change.Has(fsnotify.Remove) || change.Has(fsnotify.Rename)) {
				return errors.New("worker state directory removed or renamed")
			}
			if change.Name == stopPath {
				checkStop()
			} else if index, ok := files[change.Name]; ok && change.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				refresh(index)
			}
		}
	}
}
