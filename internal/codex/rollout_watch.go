package codex

import (
	"path/filepath"
	"runtime"
	"time"

	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/fsnotify/fsnotify"
)

// The long-lived worker opts in. A standalone Poll always checks the
// filesystem synchronously. All watcher state belongs to the polling goroutine.
func (c *Collector) Watch() {
	if c.watch == nil {
		c.watch = &rolloutWatch{}
	}
}

func (c *Collector) Close() error {
	watch := c.watch
	c.watch = nil
	if watch != nil && watch.native != nil {
		return watch.native.Close()
	}
	return nil
}

type rolloutWatch struct {
	native *platform.DirectoryWatcher
	dirs   map[string]bool
	// Windows can defer write notifications until a writer closes its
	// handle. Probe known paths between walks without relisting directories.
	pollFiles []string
	nextWalk  time.Time
	retryAt   time.Time
}

func (w *rolloutWatch) rescan() {
	if w != nil {
		w.nextWalk = time.Time{}
	}
}

func (w *rolloutWatch) disable() {
	_ = w.native.Close()
	w.native = nil
	w.retryAt = time.Now().Add(30 * time.Second)
}

// WalkDir calls this before enumerating the directory, closing the gap
// between discovering a directory and receiving events for its children.
func (w *rolloutWatch) addDir(path string) {
	if w == nil || w.native == nil || w.dirs[path] {
		return
	}
	if err := w.native.Add(path); err != nil {
		w.disable() // Fall back to metadata polling if any part is unwatched.
		return
	}
	w.dirs[path] = true
}

type rolloutChanges struct {
	walk      bool
	verifyAll bool
	paths     map[string]bool
}

func (w *rolloutWatch) changes(now time.Time) rolloutChanges {
	if w == nil {
		return rolloutChanges{walk: true, verifyAll: true}
	}
	if w.native == nil {
		if now.Before(w.retryAt) {
			return rolloutChanges{walk: true}
		}
		native, err := platform.NewDirectoryWatcher()
		if err != nil {
			w.retryAt = now.Add(30 * time.Second)
			return rolloutChanges{walk: true}
		}
		w.native = native
		w.dirs = make(map[string]bool)
		w.nextWalk = now.Add(30 * time.Second)
		return rolloutChanges{walk: true}
	}
	changes := rolloutChanges{walk: len(w.dirs) == 0 || !now.Before(w.nextWalk)}
	// Bound the batch so a busy producer cannot starve reading or checkpointing.
drain:
	for range 256 {
		select {
		case <-w.native.Errors:
			w.disable()
			return rolloutChanges{walk: true, verifyAll: true}
		case e, ok := <-w.native.Events:
			if !ok {
				w.disable()
				return rolloutChanges{walk: true, verifyAll: true}
			}
			if w.dirs[e.Name] && (e.Has(fsnotify.Remove) || e.Has(fsnotify.Rename)) {
				w.disable()
				return rolloutChanges{walk: true, verifyAll: true}
			}
			if e.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if isRollout(filepath.Base(e.Name)) {
				if changes.paths == nil {
					changes.paths = make(map[string]bool)
				}
				changes.paths[e.Name] = true
				if runtime.GOOS == "windows" && e.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
					changes.walk = true
				}
			} else if e.Has(fsnotify.Create) {
				changes.walk = true // A new date directory may already contain a rollout.
			}
		default:
			break drain
		}
	}
	if changes.walk {
		w.nextWalk = now.Add(30 * time.Second)
		clear(w.dirs)
		for _, path := range w.native.WatchList() {
			w.dirs[path] = true
		}
	}
	return changes
}
