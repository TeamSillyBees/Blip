package worker

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const queueReconcileInterval = 30 * time.Second

// The atomic publication of a job is the cross-process wakeup. This also
// works with older hook binaries and with events enqueued by a collector.
func watchQueue(ctx context.Context, dir string, changes chan<- string, logger *log.Logger) {
	wake := func(name string) {
		select {
		case changes <- name:
		case <-ctx.Done():
		}
	}
	reported := false
	for ctx.Err() == nil {
		err := watchPublications(ctx, dir, func(name string) {
			reported = false
			wake(name)
		})
		if ctx.Err() != nil {
			return
		}
		if !reported {
			logger.Printf("queue watcher unavailable, retrying: %v", err)
			reported = true
		}
		// Keep delivering on filesystems that cannot install a native watch.
		wake("")
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// An empty name asks the worker to reconcile the directory after a gap.
func watchPublications(ctx context.Context, dir string, wake func(string)) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err = w.Add(dir); err != nil {
		return err
	}
	wake("") // Cover jobs published between the initial scan and registration.
	reconcile := time.NewTicker(queueReconcileInterval)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reconcile.C:
			wake("")
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("queue watch closed")
			}
			if !errors.Is(err, fsnotify.ErrEventOverflow) {
				return err
			}
			wake("")
		case change, ok := <-w.Events:
			if !ok {
				return errors.New("queue watch closed")
			}
			if change.Name == dir && (change.Has(fsnotify.Remove) || change.Has(fsnotify.Rename)) {
				return errors.New("watched queue directory was removed or renamed")
			}
			if filepath.Ext(change.Name) == ".json" && change.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				wake(filepath.Base(change.Name))
			}
		}
	}
}
