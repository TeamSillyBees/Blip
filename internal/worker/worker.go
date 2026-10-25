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

	"github.com/TeamSillyBees/Blip/internal/channels"
	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type Sender interface {
	Send(context.Context, config.Destination, event.Event) error
}
type Dispatcher struct {
	Queue       store.Queue
	Sender      Sender
	MaxAttempts int
	Timeout     time.Duration
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
			sendCtx, cancel := context.WithTimeout(ctx, d.Timeout)
			sendErr := d.Sender.Send(sendCtx, destination, job.Event)
			cancel()
			finishedAt := time.Now().UTC()
			if ctx.Err() != nil {
				return true, ctx.Err()
			} // Shutdown leaves the job pending.
			writeCtx, writeCancel := context.WithTimeout(ctx, 3*time.Second)
			err = d.Queue.UpdateDelivery(writeCtx, job.Event.ID, destination.ID, func(current *store.Delivery) {
				current.Attempts++
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
	Ready     bool      `json:"ready"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Heartbeat time.Time `json:"heartbeat"`
}

func Run(ctx context.Context, c config.Config, logger *log.Logger) error {
	queue := store.Queue{Dir: c.StateDir}
	lock, locked, err := queue.WorkerLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("a Blip worker is already running for this configuration")
	}
	defer lock.Unlock()
	stopPath := filepath.Join(c.StateDir, "stop")
	if err = os.Remove(stopPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	status := Status{PID: os.Getpid(), StartedAt: time.Now().UTC(), Heartbeat: time.Now().UTC()}
	if err = store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), status); err != nil {
		return err
	}
	dispatcher := Dispatcher{Queue: queue, Sender: channels.New(c.TimeoutDuration()), MaxAttempts: c.MaxAttempts, Timeout: c.TimeoutDuration()}
	var group sync.WaitGroup
	for _, destination := range c.Enabled() {
		group.Add(1)
		go func(destination config.Destination) {
			defer group.Done()
			interval := c.PollDuration()
			if destination.Kind == "wecom" && interval < 3100*time.Millisecond {
				interval = 3100 * time.Millisecond
			}
			if destination.Kind == "feishu" && interval < 650*time.Millisecond {
				interval = 650 * time.Millisecond
			}
			for {
				if _, sendErr := dispatcher.DeliverOnce(ctx, destination, time.Now().UTC()); sendErr != nil && ctx.Err() == nil {
					logger.Printf("delivery state error for destination %q: %v", destination.ID, sendErr)
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
	collector := codex.Collector{SessionsDir: filepath.Join(c.CodexHome, "sessions"), StatePath: filepath.Join(c.StateDir, "rollout-cursors.json")}
	destinations := DestinationIDs(c)
	ticker := time.NewTicker(c.PollDuration())
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		if err = collector.Poll(ctx, func(e event.Event) error { _, err := queue.Enqueue(ctx, e, destinations); return err }); err != nil && ctx.Err() == nil {
			if errors.Is(err, codex.ErrCheckpoint) {
				return err
			}
			logger.Printf("collector: %v", err)
		}
		status.Heartbeat = time.Now().UTC()
		status.Ready = true
		if err = store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), status); err != nil {
			return err
		}
		if time.Since(lastPrune) >= time.Hour {
			if err = queue.Prune(time.Now().UTC()); err != nil {
				logger.Printf("queue cleanup: %v", err)
			}
			lastPrune = time.Now()
		}
		if _, err = os.Stat(stopPath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
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
