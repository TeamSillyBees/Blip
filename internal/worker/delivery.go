package worker

import (
	"context"
	"iter"
	"log"
	"sync"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// Prefer the oldest due job, and keep the earliest future retry for the timer.
func nextDelivery(jobs iter.Seq[store.Job], destination string, now time.Time) (id string, retry time.Time) {
	var oldest time.Time
	for job := range jobs {
		for _, r := range job.Deliveries {
			if r.DestinationID != destination || r.DeliveredAt != nil || r.Failed {
				continue
			}
			if r.NextAttempt.After(now) {
				retry = earliest(retry, r.NextAttempt)
			} else if id == "" || job.CreatedAt.Before(oldest) || (job.CreatedAt.Equal(oldest) && job.Event.ID < id) {
				oldest, id = job.CreatedAt, job.Event.ID
			}
		}
	}
	return id, retry
}

func earliest(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func channelInterval(kind string) time.Duration {
	switch kind {
	case "wecom":
		return 3100 * time.Millisecond
	case "feishu":
		return 650 * time.Millisecond
	default:
		return 0
	}
}

type deliveryResult struct {
	index     int
	id        string
	attempted bool
	finished  time.Time
	err       error
}

// Dispatch at most one job per destination at a time. Publications and
// completed attempts wake the scheduler; timers cover retries and channel pacing.
func (d Dispatcher) runDeliveries(ctx context.Context, destinations []config.Destination, pending *store.PendingIndex, changes <-chan string, logger *log.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	var group sync.WaitGroup
	defer func() { cancel(); group.Wait() }()

	done := make(chan deliveryResult, len(destinations))
	type slot struct {
		busy        bool
		nextAllowed time.Time
	}
	slots := make([]slot, len(destinations))
	var pruning <-chan struct{}
	prune := func() {
		if pruning != nil {
			return
		}
		now := time.Now()
		ids := pending.CleanupCandidates(now)
		if len(ids) == 0 {
			return
		}
		finished := make(chan struct{})
		pruning = finished
		group.Go(func() {
			defer close(finished)
			if err := d.Queue.PruneJobs(ctx, ids, now); err != nil && ctx.Err() == nil {
				logger.Printf("queue cleanup: %v", err)
			}
		})
	}
	prune()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for ctx.Err() == nil {
		now := time.Now()
		var deadline time.Time
		for index, destination := range destinations {
			s := &slots[index]
			if s.busy {
				continue
			}
			id, nextAttempt := nextDelivery(pending.Jobs(), destination.ID, now)
			if id != "" {
				nextAttempt = now
			} else if nextAttempt.IsZero() {
				continue
			}
			if s.nextAllowed.After(nextAttempt) {
				nextAttempt = s.nextAllowed
			}
			if nextAttempt.After(now) {
				deadline = earliest(deadline, nextAttempt)
				continue
			}
			s.busy = true
			group.Go(func() {
				attempted, err := d.deliver(ctx, destination, id, now)
				done <- deliveryResult{index: index, id: id, attempted: attempted, finished: time.Now(), err: err}
			})
		}
		var timer *time.Timer
		var due <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			due = timer.C
		}
		var err error
		select {
		case <-ctx.Done():
		case name := <-changes:
			err = refreshPublications(pending, name, changes)
		case <-cleanup.C:
			if len(destinations) == 0 {
				// Without senders there is no watcher. Only retention needs a scan.
				if err := pending.Reconcile(); err != nil {
					logger.Printf("queue cleanup: %v", err)
					break
				}
			}
			prune()
		case <-pruning:
			pruning = nil
		case <-due:
		case result := <-done:
			s := &slots[result.index]
			s.busy = false
			if result.attempted {
				s.nextAllowed = result.finished.Add(channelInterval(destinations[result.index].Kind))
			}
			err = result.err
			if err == nil && ctx.Err() == nil {
				err = pending.Refresh(result.id + ".json")
			}
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func refreshPublications(pending *store.PendingIndex, first string, changes <-chan string) error {
	names := map[string]bool{first: true}
	for range len(changes) {
		names[<-changes] = true
	}
	reconcile := names[""]
	delete(names, "")
	for name := range names {
		if err := pending.Refresh(name); err != nil {
			return err
		}
	}
	if reconcile {
		return pending.Reconcile()
	}
	return nil
}
