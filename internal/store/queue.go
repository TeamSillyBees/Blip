// Package store persists notification jobs and per-destination delivery receipts.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/gofrs/flock"
)

type Delivery struct {
	DestinationID string     `json:"destination_id"`
	Attempts      int        `json:"attempts"`
	NextAttempt   time.Time  `json:"next_attempt"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	Failed        bool       `json:"failed,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type Job struct {
	Event      event.Event `json:"event"`
	CreatedAt  time.Time   `json:"created_at"`
	Deliveries []Delivery  `json:"deliveries"`
}

type Queue struct{ Dir string }

func (q Queue) jobPath(id string) string { return filepath.Join(q.Dir, "jobs", id+".json") }

// Enqueue is safe across concurrent hook processes. Existing receipts are never overwritten.
func (q Queue) Enqueue(ctx context.Context, e event.Event, destinations []string) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Join(q.Dir, "jobs"), 0700); err != nil {
		return false, err
	}
	lock := flock.New(filepath.Join(q.Dir, "enqueue.lock"))
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("notification queue is busy")
	}
	defer lock.Unlock()
	path := q.jobPath(e.ID)
	if _, err = os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	job := Job{Event: e, CreatedAt: time.Now().UTC(), Deliveries: []Delivery{}}
	seen := map[string]bool{}
	for _, id := range destinations {
		if id != "" && !seen[id] {
			job.Deliveries = append(job.Deliveries, Delivery{DestinationID: id, NextAttempt: job.CreatedAt})
			seen[id] = true
		}
	}
	if len(job.Deliveries) == 0 {
		return false, nil
	}
	if err = WriteJSON(path, job); err != nil {
		return false, err
	}
	return true, nil
}

func (q Queue) Load() ([]Job, error) {
	entries, err := os.ReadDir(filepath.Join(q.Dir, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := []Job{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var job Job
		if err = ReadJSON(filepath.Join(q.Dir, "jobs", entry.Name()), &job); err != nil {
			return nil, fmt.Errorf("unreadable queue record %s", entry.Name())
		}
		if err = job.Event.Validate(); err != nil || entry.Name() != job.Event.ID+".json" {
			return nil, fmt.Errorf("invalid queue record %s", entry.Name())
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// Save is used by the single worker while it holds worker.lock.
func (q Queue) Save(job Job) error {
	if err := job.Event.Validate(); err != nil {
		return err
	}
	return WriteJSON(q.jobPath(job.Event.ID), job)
}

// UpdateDelivery merges one receipt with the latest file, avoiding lost updates
// when two independent channel workers finish the same event concurrently.
func (q Queue) UpdateDelivery(ctx context.Context, id, destination string, update func(*Delivery)) error {
	lock := flock.New(filepath.Join(q.Dir, "enqueue.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("notification queue is busy")
	}
	defer lock.Unlock()
	var job Job
	if err = ReadJSON(q.jobPath(id), &job); err != nil {
		return err
	}
	for index := range job.Deliveries {
		if job.Deliveries[index].DestinationID == destination {
			update(&job.Deliveries[index])
			return q.Save(job)
		}
	}
	return fmt.Errorf("destination receipt not found")
}

func (q Queue) WorkerLock() (*flock.Flock, bool, error) {
	if err := os.MkdirAll(q.Dir, 0700); err != nil {
		return nil, false, err
	}
	lock := flock.New(filepath.Join(q.Dir, "worker.lock"))
	ok, err := lock.TryLock()
	return lock, ok, err
}

func (q Queue) Prune(now time.Time) error {
	jobs, err := q.Load()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if now.Sub(job.CreatedAt) < 7*24*time.Hour {
			continue
		}
		terminal := true
		for _, delivery := range job.Deliveries {
			if delivery.DeliveredAt == nil && !delivery.Failed {
				terminal = false
			}
		}
		if terminal {
			if err := os.Remove(q.jobPath(job.Event.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
