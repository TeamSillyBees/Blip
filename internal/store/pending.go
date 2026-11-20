package store

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PendingIndex belongs to one worker goroutine. The JSON receipts remain the
// source of truth. Terminal jobs only occupy a file stamp and a retention date.
type PendingIndex struct {
	queue   Queue
	files   map[string]indexedFile
	pending map[string]Job
}

type indexedFile struct {
	modified time.Time
	size     int64
	checkAt  time.Time
}

func (q Queue) PendingIndex() (*PendingIndex, error) {
	index := &PendingIndex{queue: q, files: make(map[string]indexedFile), pending: make(map[string]Job)}
	if err := index.Reconcile(); err != nil {
		return nil, err
	}
	return index, nil
}

func (p *PendingIndex) Jobs() iter.Seq[Job] { return maps.Values(p.pending) }

func (p *PendingIndex) forget(name string) {
	delete(p.pending, name)
	delete(p.files, name)
}

// Reconcile recovers missed notifications, including same-name replacements.
// Only new or changed entries need a shared lock and a JSON read.
func (p *PendingIndex) Reconcile() error {
	entries, err := os.ReadDir(p.queue.JobsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		seen[name] = true
		if prev, ok := p.files[name]; ok && prev.size == info.Size() && prev.modified.Equal(info.ModTime()) {
			continue
		}
		if err := p.load(name, info); err != nil {
			return err
		}
	}
	for name := range p.files {
		if !seen[name] {
			p.forget(name)
		}
	}
	return p.removeBlocked()
}

// Refresh reloads the named publication even on filesystems with coarse
// timestamps. Receipt completion uses the same path if its file event was lost.
func (p *PendingIndex) Refresh(name string) error {
	if name != filepath.Base(name) || filepath.Ext(name) != ".json" {
		return fmt.Errorf("invalid queue record name")
	}
	info, err := os.Stat(filepath.Join(p.queue.JobsDir(), name))
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.IsDir()) {
		p.forget(name)
		return nil
	}
	if err != nil {
		return err
	}
	return p.load(name, info)
}

func (p *PendingIndex) load(name string, info fs.FileInfo) error {
	job, err := p.queue.loadJob(name)
	if err != nil {
		return err
	}
	if job == nil {
		p.forget(name)
		// An isolated file can also claim the identity of an earlier entry.
		return p.removeBlocked()
	}
	blocked, err := p.queue.blocked(job.Event.ID)
	if err != nil {
		return err
	}
	if blocked {
		if _, err := p.queue.quarantine(name); err != nil {
			return err
		}
		p.forget(name)
		return nil
	}
	retention := job.retention()
	p.files[name] = indexedFile{modified: info.ModTime(), size: info.Size(), checkAt: retention.checkAt}
	if retention.pending {
		p.pending[name] = *job
	} else {
		delete(p.pending, name)
	}
	return nil
}

func (p *PendingIndex) removeBlocked() error {
	entries, err := os.ReadDir(filepath.Join(p.queue.Dir, "quarantine", "blocked"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name() + ".json"
		if _, known := p.files[name]; !known {
			continue
		}
		if _, err := p.queue.quarantine(name); err != nil {
			return err
		}
		p.forget(name)
	}
	return nil
}

// CleanupCandidates returns a snapshot so removing expired files can run
// independently of scheduling. PruneJobs checks the current receipts again.
func (p *PendingIndex) CleanupCandidates(now time.Time) []string {
	var ids []string
	for name, record := range p.files {
		if !record.checkAt.IsZero() && !now.Before(record.checkAt) {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	return ids
}
