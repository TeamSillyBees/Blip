// Package worker coordinates the collector and independent destination senders.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	for _, job := range jobs {
		for _, receipt := range job.Deliveries {
			if receipt.DestinationID != destination.ID || receipt.DeliveredAt != nil || receipt.Failed || receipt.NextAttempt.After(now) {
				continue
			}
			notification := job.Event
			// Records created before device names were supported have no label.
			// Keep names already captured at enqueue time unchanged across retries.
			if notification.DeviceName == "" {
				notification.DeviceName = d.DeviceName
			}
			// Older jobs and chats named after enqueueing can acquire their title
			// at delivery time. Preserve titles already captured by the event.
			if notification.SessionTitle == "" {
				titleCtx, stop := context.WithTimeout(ctx, 50*time.Millisecond)
				if d.ResolveTitle != nil {
					notification.SessionTitle = d.ResolveTitle(titleCtx, notification)
				} else if notification.SchemaVersion == 1 && notification.Agent == "codex" {
					notification.SessionTitle = agents.ResolveTitle(titleCtx, config.Config{CodexHome: d.CodexHome}, notification)
				}
				stop()
			}
			if receipt.FirstAttemptAt == nil && receipt.Attempts == 0 {
				// Persist the start of dispatch before calling the sender. This is
				// a local attempt marker, not a receipt of network delivery.
				startedAt := time.Now().UTC()
				writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				err = d.Queue.UpdateDelivery(writeCtx, job.Event.ID, destination.ID, func(current *store.Delivery) {
					if current.FirstAttemptAt == nil && current.Attempts == 0 {
						current.FirstAttemptAt = &startedAt
					}
				})
				stop()
				if err != nil {
					return false, err
				}
			}
			sendCtx, cancel := context.WithTimeout(ctx, d.Timeout)
			sendErr := d.Sender.Send(sendCtx, destination, notification)
			cancel()
			finishedAt := time.Now().UTC()
			if ctx.Err() != nil {
				return true, ctx.Err()
			} // Shutdown leaves the job pending.
			writeCtx, writeCancel := context.WithTimeout(ctx, 3*time.Second)
			err = d.Queue.UpdateDelivery(writeCtx, job.Event.ID, destination.ID, func(current *store.Delivery) {
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
			return true, err
		}
	}
	return false, nil
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
	if _, err = queue.Load(); err != nil {
		return err
	}
	stopPath := filepath.Join(c.StateDir, "stop")
	if err = os.Remove(stopPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopDone := make(chan struct{})
	go func() { defer close(stopDone); watchStop(ctx, stopPath, cancel, logger) }()
	defer func() { cancel(); <-stopDone }()
	status := Status{PID: os.Getpid(), StartedAt: time.Now().UTC(), Heartbeat: time.Now().UTC(), Sources: []agents.SourceStatus{}}
	sources := c.EffectiveSources()
	for _, source := range sources {
		status.Sources = append(status.Sources, agents.Inspect(c, source))
		status.Sources[len(status.Sources)-1].State = "checking"
	}
	if err = store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), status); err != nil {
		return err
	}
	dispatcher := Dispatcher{Queue: queue, Sender: channels.New(c.TimeoutDuration()), MaxAttempts: c.MaxAttempts, Timeout: c.TimeoutDuration(), DeviceName: c.DeviceName, ResolveTitle: func(ctx context.Context, e event.Event) string { return agents.ResolveTitle(ctx, c, e) }}
	fatal := make(chan error, 1)
	var group sync.WaitGroup
	for _, destination := range c.Enabled() {
		group.Add(1)
		go func(destination config.Destination) {
			defer group.Done()
			for {
				attempted, sendErr := dispatcher.DeliverOnce(ctx, destination, time.Now().UTC())
				if sendErr != nil && ctx.Err() == nil {
					select {
					case fatal <- fmt.Errorf("delivery state: %w", sendErr):
					default:
					}
					return
				}
				interval := c.PollDuration()
				if interval <= 0 || interval > 250*time.Millisecond {
					interval = 250 * time.Millisecond
				}
				if attempted && destination.Kind == "wecom" {
					interval = 3100 * time.Millisecond
				}
				if attempted && destination.Kind == "feishu" {
					interval = 650 * time.Millisecond
				}
				timer := time.NewTimer(interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}(destination)
	}
	defer func() { cancel(); group.Wait() }()
	updates := make(chan sourceUpdate, len(sources)*2+1)
	for index, source := range sources {
		group.Add(1)
		go func() { defer group.Done(); collectSource(ctx, c, source, index, registry, queue, updates) }()
	}
	checked := make([]bool, len(sources))
	collection := make([]sourceUpdate, len(sources))
	for i := range collection {
		collection[i].state = "checking"
	}
	interval := c.PollDuration()
	if interval <= 0 || interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastPrune := time.Time{}
	lastQuarantined := 0
	for {
		status.CollectorError = ""
		status.Degraded = false
		status.Ready = true
		for i := range status.Sources {
			s := &status.Sources[i]
			s.State, s.ErrorCode = collection[i].state, collection[i].code
			agents.RefreshHookDiagnostic(c, s)
			agents.ApplyHookHealth(s)
			if !checked[i] {
				status.Ready = false
			}
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
		status.Heartbeat = time.Now().UTC()
		if err = store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), status); err != nil {
			return err
		}
		if ready != nil && status.Ready {
			ready()
			ready = nil
		}
		if time.Since(lastPrune) >= time.Hour {
			if err = queue.Prune(time.Now().UTC()); err != nil {
				logger.Printf("queue cleanup: %v", err)
			}
			lastPrune = time.Now()
		}
		if count, countErr := queue.QuarantineCount(); countErr != nil {
			logger.Printf("queue quarantine status: %v", countErr)
		} else {
			if count > lastQuarantined {
				logger.Printf("queue has %d quarantined records; inspect the quarantine directory before recovering them", count)
			}
			lastQuarantined = count
		}
		if _, err = os.Stat(stopPath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case err = <-fatal:
			return err
		case update := <-updates:
			checked[update.index] = true
			collection[update.index] = update
			s := &status.Sources[update.index]
			s.State, s.ErrorCode = update.state, update.code
			if update.eventAt != nil {
				s.LastEventAt = update.eventAt
			}
			if update.err != nil && ctx.Err() == nil {
				logger.Printf("source %q: %v", s.ID, update.err)
			}
			if update.fatal != nil {
				return update.fatal
			}
		case <-ticker.C:
		}
	}
}

// Stop must remain responsive while the collector sleeps or a sender awaits HTTP.
func watchStop(ctx context.Context, path string, cancel context.CancelFunc, logger *log.Logger) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := os.Stat(path)
			if err == nil {
				cancel()
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				logger.Printf("cannot check stop request: %v", err)
				cancel()
				return
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
