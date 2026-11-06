// Package store persists notification jobs and per-destination delivery receipts.
package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
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
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	Failed        bool       `json:"failed,omitempty"`
	FailedAt      *time.Time `json:"failed_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type Job struct {
	Event      event.Event `json:"event"`
	CreatedAt  time.Time   `json:"created_at"`
	Deliveries []Delivery  `json:"deliveries"`
}

type Queue struct{ Dir string }

func (q Queue) jobPath(id string) string { return filepath.Join(q.Dir, "jobs", id+".json") }

var ErrQuarantined = errors.New("notification record is quarantined; inspect the preserved record before recovery")

// Snapshot reads the first durable notification under the same lock used by
// enqueue. Adapters use it after replay/upgrade to retain the original outcome.
func (q Queue) Snapshot(ctx context.Context, id string) (*event.Event, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid notification ID")
	}
	lock, err := q.mutationLock(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if blocked, err := q.blocked(id); blocked || err != nil {
		if err != nil {
			return nil, err
		}
		return nil, ErrQuarantined
	}
	data, err := os.ReadFile(q.jobPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var job Job
	if decodeJob(data, id+".json", &job) != nil {
		return nil, ErrQuarantined
	}
	return &job.Event, nil
}

func (q Queue) mutationLock(ctx context.Context) (*flock.Flock, error) {
	if err := os.MkdirAll(q.Dir, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(q.Dir, "enqueue.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !ok {
		_ = lock.Close()
		return nil, fmt.Errorf("notification queue is busy")
	}
	return lock, nil
}

func validID(id string) bool { _, err := hex.DecodeString(id); return len(id) == 64 && err == nil }

func (q Queue) blocked(id string) (bool, error) {
	_, err := os.Stat(filepath.Join(q.Dir, "quarantine", "blocked", id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Enqueue is safe across concurrent hook processes. Existing receipts are never overwritten.
func (q Queue) Enqueue(ctx context.Context, e event.Event, destinations []string) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Join(q.Dir, "jobs"), 0700); err != nil {
		return false, err
	}
	lock, err := q.mutationLock(ctx)
	if err != nil {
		return false, err
	}
	defer lock.Unlock()
	if blocked, err := q.blocked(e.ID); blocked || err != nil {
		return false, err
	}
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
		data, readErr := q.readJobFile(entry.Name())
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		} // A concurrent prune may remove a terminal record.
		if readErr != nil {
			return nil, fmt.Errorf("unreadable queue record %s", entry.Name())
		}
		if decodeJob(data, entry.Name(), &job) != nil {
			// Re-read under the mutation lock before isolating the file: a receipt
			// update or another reader may have changed it since the first read.
			recovered, quarantineErr := q.quarantine(entry.Name())
			if quarantineErr != nil {
				return nil, quarantineErr
			}
			if recovered != nil {
				jobs = append(jobs, *recovered)
			}
			continue
		}
		jobs = append(jobs, job)
	}
	// A later invalid record can claim an ID read earlier in this scan. Check
	// every collected identity again, so such a conflict cannot be dispatched.
	ready := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		blocked, err := q.blocked(job.Event.ID)
		if err != nil {
			return nil, err
		}
		if blocked {
			if _, err = q.quarantine(job.Event.ID + ".json"); err != nil {
				return nil, err
			}
			continue
		}
		ready = append(ready, job)
	}
	return ready, nil
}

func (q Queue) readJobFile(name string) ([]byte, error) {
	// Windows does not allow replacing/removing a record while ReadFile holds
	// its handle. Shared queue locks let readers coexist and exclude mutation
	// only for the short file read, not for parsing or sending notifications.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lock := flock.New(filepath.Join(q.Dir, "enqueue.lock"))
	ok, err := lock.TryRLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !ok {
		_ = lock.Close()
		return nil, fmt.Errorf("notification queue is busy")
	}
	defer lock.Unlock()
	return os.ReadFile(filepath.Join(q.Dir, "jobs", name))
}

// Save serializes local state changes with enqueue, receipt updates and pruning.
func (q Queue) Save(job Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lock, err := q.mutationLock(ctx)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return q.saveLocked(job)
}

func (q Queue) saveLocked(job Job) error {
	for i := range job.Deliveries {
		if job.Deliveries[i].Failed && job.Deliveries[i].FailedAt == nil {
			now := time.Now().UTC()
			job.Deliveries[i].FailedAt = &now
		}
	}
	if err := validateJob(job, job.Event.ID+".json"); err != nil {
		return err
	}
	if blocked, err := q.blocked(job.Event.ID); err != nil {
		return err
	} else if blocked {
		return ErrQuarantined
	}
	return WriteJSON(q.jobPath(job.Event.ID), job)
}

// UpdateDelivery merges one receipt with the latest file, avoiding lost updates
// when two independent channel workers finish the same event concurrently.
func (q Queue) UpdateDelivery(ctx context.Context, id, destination string, update func(*Delivery)) error {
	if !validID(id) {
		return fmt.Errorf("invalid notification ID")
	}
	lock, err := q.mutationLock(ctx)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if blocked, err := q.blocked(id); err != nil {
		return err
	} else if blocked {
		return ErrQuarantined
	}
	var job Job
	if err = ReadJSON(q.jobPath(id), &job); err != nil {
		return err
	}
	if err = validateJob(job, id+".json"); err != nil {
		return err
	}
	for index := range job.Deliveries {
		if job.Deliveries[index].DestinationID == destination {
			update(&job.Deliveries[index])
			return q.saveLocked(job)
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
		if err := q.pruneJob(job.Event.ID, now); err != nil {
			return err
		}
	}
	return nil
}

func (q Queue) pruneJob(id string, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lock, err := q.mutationLock(ctx)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if blocked, err := q.blocked(id); err != nil {
		return err
	} else if blocked {
		return nil
	}
	var job Job
	if err = ReadJSON(q.jobPath(id), &job); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("unreadable queue record %s", id)
	}
	if err = validateJob(job, id+".json"); err != nil {
		return err
	}
	latest := job.CreatedAt
	for _, d := range job.Deliveries {
		if d.DeliveredAt == nil && !d.Failed {
			return nil
		}
		finished := d.DeliveredAt
		if d.Failed {
			finished = d.FailedAt
		}
		if finished == nil {
			return q.saveLocked(job)
		} // Old failures get a full retention window after migration.
		if finished.After(latest) {
			latest = *finished
		}
	}
	if now.Sub(latest) < 7*24*time.Hour {
		return nil
	}
	if err = os.Remove(q.jobPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func decodeJob(data []byte, name string, job *Job) error {
	if json.Unmarshal(data, job) != nil {
		return fmt.Errorf("invalid JSON")
	}
	return validateJob(*job, name)
}

func validateJob(job Job, name string) error {
	if job.Event.Validate() != nil || name != job.Event.ID+".json" {
		return fmt.Errorf("invalid event identity")
	}
	if job.CreatedAt.IsZero() || len(job.Deliveries) == 0 {
		return fmt.Errorf("missing creation time or delivery receipts")
	}
	seen := map[string]bool{}
	for _, d := range job.Deliveries {
		if d.DestinationID == "" || seen[d.DestinationID] || d.Attempts < 0 {
			return fmt.Errorf("invalid delivery identity or attempt count")
		}
		if d.LastAttemptAt != nil && d.LastAttemptAt.IsZero() {
			return fmt.Errorf("invalid last attempt time")
		}
		seen[d.DestinationID] = true
		if d.DeliveredAt != nil && (d.DeliveredAt.IsZero() || d.Failed) {
			return fmt.Errorf("invalid delivered receipt")
		}
		if d.FailedAt != nil && (!d.Failed || d.FailedAt.IsZero()) {
			return fmt.Errorf("invalid failed receipt")
		}
		if d.DeliveredAt == nil && !d.Failed && d.NextAttempt.IsZero() {
			return fmt.Errorf("missing retry time")
		}
	}
	return nil
}

type quarantineInfo struct {
	OriginalFile  string    `json:"original_file"`
	Reason        string    `json:"reason"`
	QuarantinedAt time.Time `json:"quarantined_at"`
}

func (q Queue) quarantine(name string) (*Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lock, err := q.mutationLock(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	path := filepath.Join(q.Dir, "jobs", name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unreadable queue record %s", name)
	}
	var job Job
	reason := decodeJob(data, name, &job)
	if reason == nil {
		blocked, err := q.blocked(job.Event.ID)
		if err != nil {
			return nil, err
		}
		if !blocked {
			return &job, nil
		}
		reason = fmt.Errorf("event identity conflicts with an isolated record")
	}
	dir := filepath.Join(q.Dir, "quarantine", "records", event.ID("queue-record", name))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Block both the filename's identity and a readable embedded identity before
	// moving anything. A crash cannot then make a replay recreate empty receipts.
	for _, id := range []string{strings.TrimSuffix(name, ".json"), job.Event.ID} {
		if validID(id) {
			if err = WriteJSON(filepath.Join(q.Dir, "quarantine", "blocked", id), map[string]string{"record": filepath.Base(dir)}); err != nil {
				return nil, err
			}
		}
	}
	if err = WriteJSON(filepath.Join(dir, "details.json"), quarantineInfo{name, reason.Error(), time.Now().UTC()}); err != nil {
		return nil, err
	}
	recordPath := filepath.Join(dir, "record.json")
	// Existing evidence must never be overwritten, even if someone reintroduced
	// another invalid file under the same event ID.
	if _, statErr := os.Stat(recordPath); statErr == nil {
		recordPath = filepath.Join(dir, "record-"+time.Now().UTC().Format("20060102T150405.000000000")+".json")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if err = os.Rename(path, recordPath); err != nil {
		return nil, fmt.Errorf("cannot quarantine queue record %s: %w", name, err)
	}
	return nil, nil
}

// QuarantineCount reports retained incident directories; raw evidence is never
// pruned automatically. It is safe to call without loading or changing the queue.
func (q Queue) QuarantineCount() (int, error) {
	entries, err := os.ReadDir(filepath.Join(q.Dir, "quarantine", "records"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count, nil
}
