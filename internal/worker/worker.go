// Package worker coordinates the collector and independent destination senders.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/channels"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type Sender interface {
	Send(context.Context, config.Destination, event.Event) error
}
type Dispatcher struct {
	Queue        store.Queue
	Sender       Sender
	MaxAttempts  int
	Timeout      time.Duration
	DeviceName   string
	CodexHome    string
	ResolveTitle func(context.Context, event.Event) string
}

// DeliverOnce attempts the oldest due job for just one destination.
func (d Dispatcher) DeliverOnce(ctx context.Context, destination config.Destination, now time.Time) (bool, error) {
	jobs, err := d.Queue.Load()
	if err != nil {
		return false, err
	}
	id, _ := nextDelivery(slices.Values(jobs), destination.ID, now)
	if id == "" {
		return false, nil
	}
	return d.deliver(ctx, destination, id, now)
}

func (d Dispatcher) deliver(ctx context.Context, destination config.Destination, id string, due time.Time) (bool, error) {
	startCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	notification, err := d.Queue.BeginAttempt(startCtx, id, destination.ID, due)
	stop()
	if err != nil || notification == nil {
		return false, err
	}
	// Records created before device names were supported have no label.
	// Keep names already captured at enqueue time unchanged across retries.
	if notification.DeviceName == "" {
		notification.DeviceName = d.DeviceName
	}
	// Older jobs and chats named after enqueueing can acquire their title
	// at delivery time. Preserve titles already captured by the event.
	if notification.SessionTitle == "" {
		titleCtx, stop := context.WithTimeout(ctx, agents.TitleLookupTimeout(notification.Agent))
		if d.ResolveTitle != nil {
			notification.SessionTitle = d.ResolveTitle(titleCtx, *notification)
		} else if notification.SchemaVersion == 1 && notification.Agent == "codex" {
			notification.SessionTitle = agents.ResolveTitle(titleCtx, config.Config{CodexHome: d.CodexHome}, *notification)
		}
		stop()
	}
	sendCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	sendErr := d.Sender.Send(sendCtx, destination, *notification)
	cancel()
	finishedAt := time.Now().UTC()
	if ctx.Err() != nil {
		return true, ctx.Err()
	} // Shutdown leaves the job pending.
	writeCtx, writeCancel := context.WithTimeout(ctx, 3*time.Second)
	err = d.Queue.UpdateDelivery(writeCtx, notification.ID, destination.ID, func(current *store.Delivery) {
		current.Attempts++
		current.LastAttemptAt = &finishedAt
		if sendErr == nil {
			current.DeliveredAt = &finishedAt
			current.LastError = ""
			return
		}
		current.LastError = sendErr.Error() // Sender errors intentionally exclude endpoints, bodies and credentials.
		current.Failed = current.Attempts >= d.MaxAttempts || !channels.Retryable(sendErr)
		current.NextAttempt = finishedAt.Add(Backoff(current.Attempts))
	})
	writeCancel()
	if errors.Is(err, store.ErrQuarantined) {
		// A conflicting record was isolated while this send was in flight.
		// The blocked marker prevents another attempt for this event.
		return true, nil
	}
	return true, err
}

func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 {
		return time.Minute
	}
	delay := time.Duration(1<<(attempt-1)) * 5 * time.Second
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

type Status struct {
	Ready          bool                  `json:"ready"`
	PID            int                   `json:"pid"`
	StartedAt      time.Time             `json:"started_at"`
	Heartbeat      time.Time             `json:"heartbeat"`
	CollectorError string                `json:"collector_error,omitempty"`
	Degraded       bool                  `json:"degraded"`
	Sources        []agents.SourceStatus `json:"sources"`
}

func Run(ctx context.Context, c config.Config, logger *log.Logger) error {
	return RunWithReady(ctx, c, logger, nil)
}

// RunWithReady reports initialization once, after the first completed collection
// pass and its ready status have been published. The callback must not block.
func RunWithReady(ctx context.Context, c config.Config, logger *log.Logger, ready func()) error {
	return runWithRegistry(ctx, c, logger, ready, agents.Builtins())
}

func runWithRegistry(ctx context.Context, c config.Config, logger *log.Logger, ready func(), registry agents.Registry) error {
	if c.DeviceName == "" {
		c.DeviceName = config.DefaultDeviceName()
	}
	queue := store.Queue{Dir: c.StateDir}
	lock, locked, err := queue.WorkerLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("a Blip worker is already running for this configuration")
	}
	defer lock.Unlock()
	pending, err := queue.PendingIndex()
	if err != nil {
		return err
	}
	stopPath := filepath.Join(c.StateDir, "stop")
	if err = os.Remove(stopPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	status := Status{PID: os.Getpid(), StartedAt: time.Now().UTC(), Sources: []agents.SourceStatus{}}
	sources := c.EffectiveSources()
	for _, source := range sources {
		status.Sources = append(status.Sources, agents.Inspect(c, source))
		status.Sources[len(status.Sources)-1].State = "checking"
	}
	dispatcher := Dispatcher{Queue: queue, Sender: channels.New(c.TimeoutDuration()), MaxAttempts: c.MaxAttempts, Timeout: c.TimeoutDuration(), DeviceName: c.DeviceName, ResolveTitle: func(ctx context.Context, e event.Event) string { return agents.ResolveTitle(ctx, c, e) }}
	fatal := make(chan error, 1)
	var group sync.WaitGroup
	diagnostics := make(chan int, len(sources)+1)
	group.Go(func() { watchWorkerState(ctx, c, sources, cancel, diagnostics, logger) })
	destinations := c.Enabled()
	var changes chan string
	if len(destinations) > 0 {
		changes = make(chan string, 128)
		group.Go(func() { watchQueue(ctx, queue.JobsDir(), changes, logger) })
	}
	group.Go(func() {
		if err := dispatcher.runDeliveries(ctx, destinations, pending, changes, logger); err != nil && ctx.Err() == nil {
			fatal <- fmt.Errorf("delivery state: %w", err)
		}
	})
	defer func() { cancel(); group.Wait() }()
	updates := make(chan sourceUpdate, len(sources)*2+1)
	for index, source := range sources {
		group.Add(1)
		go func() { defer group.Done(); collectSource(ctx, c, source, index, registry, queue, updates) }()
	}
	collection := make([]sourceUpdate, len(sources))
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var published Status
	lastQuarantined := 0
	reconcile := true
	for {
		status.CollectorError = ""
		status.Degraded = false
		status.Ready = true
		for i := range status.Sources {
			s := &status.Sources[i]
			s.State, s.ErrorCode = collection[i].state, collection[i].code
			if s.State == "" {
				s.State = "checking"
				status.Ready = false
			}
			agents.ApplyHookHealth(s)
			if s.ErrorCode != "" {
				status.Degraded = true
				if s.ID == config.DefaultSourceID {
					status.CollectorError = s.ErrorCode
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if reconcile || !reflect.DeepEqual(published, status) {
			status.Heartbeat = time.Now().UTC()
			if err = store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), status); err != nil {
				return err
			}
			published = status
			published.Sources = slices.Clone(status.Sources)
			if ready != nil && status.Ready {
				ready()
				ready = nil
			}
		}
		if reconcile {
			if count, err := queue.QuarantineCount(); err != nil {
				logger.Printf("queue quarantine status: %v", err)
			} else {
				if count > lastQuarantined {
					logger.Printf("queue has %d quarantined records; inspect the quarantine directory before recovering them", count)
				}
				lastQuarantined = count
			}
			reconcile = false
		}
		select {
		case <-ctx.Done():
			return nil
		case err = <-fatal:
			return err
		case update := <-updates:
			collection[update.index] = update
			s := &status.Sources[update.index]
			if update.eventAt != nil && (s.LastEventAt == nil || update.eventAt.After(*s.LastEventAt)) {
				s.LastEventAt = update.eventAt
			}
			if update.err != nil && ctx.Err() == nil {
				logger.Printf("source %q: %v", s.ID, update.err)
			}
			if update.fatal != nil {
				return update.fatal
			}
		case <-ticker.C:
			reconcile = true
			for i := range status.Sources {
				agents.RefreshHookDiagnostic(c, &status.Sources[i])
			}
		case index := <-diagnostics:
			// A single atomic replacement can generate several notifications.
			changed := map[int]bool{index: true}
			for range len(diagnostics) {
				changed[<-diagnostics] = true
			}
			for i := range status.Sources {
				if changed[-1] || changed[i] {
					agents.RefreshHookDiagnostic(c, &status.Sources[i])
				}
			}
		}
	}
}

func DestinationIDs(c config.Config) []string {
	ids := []string{}
	for _, destination := range c.Enabled() {
		ids = append(ids, destination.ID)
	}
	return ids
}
