package worker

import (
	"context"
	"errors"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type sourceUpdate struct {
	index       int
	state, code string
	err, fatal  error
	eventAt     *time.Time
}

func collectSource(ctx context.Context, c config.Config, source config.Source, index int, registry agents.Registry, queue store.Queue, updates chan<- sourceUpdate) {
	report := func(update sourceUpdate) {
		update.index = index
		select {
		case updates <- update:
		case <-ctx.Done():
		}
	}
	if !source.Enabled {
		report(sourceUpdate{state: "disabled"})
		return
	}
	bindCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := agents.Bind(bindCtx, c, source)
	cancel()
	if err != nil {
		report(sourceUpdate{state: "paused", code: "source_binding", err: err})
		return
	}
	adapter, err := registry.Open(c, source)
	if err != nil {
		report(sourceUpdate{state: "unavailable", code: "adapter_unavailable", err: err})
		return
	}
	collector, ok := adapter.(agents.Collector)
	if !ok {
		report(sourceUpdate{state: "ready"})
		return
	}
	for {
		update := sourceUpdate{state: "ready"}
		pollCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		err = collector.Poll(pollCtx, func(e event.Event) error {
			e.DeviceName = c.DeviceName
			if resolver, ok := adapter.(agents.TitleResolver); ok {
				titleCtx, cancel := context.WithTimeout(pollCtx, 50*time.Millisecond)
				e.SessionTitle = resolver.SessionTitle(titleCtx, e.SessionID)
				cancel()
			}
			_, err := queue.Enqueue(pollCtx, e, DestinationIDs(c))
			if err != nil && pollCtx.Err() == nil {
				update.fatal = err
			}
			if err == nil {
				now := time.Now().UTC()
				update.eventAt = &now
			}
			return err
		})
		stop()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			update.state = "degraded"
			update.code = "collector_error"
			update.err = err
		}
		pause := errors.Is(err, agents.ErrCheckpoint)
		if pause {
			update.state = "paused"
			update.code = "checkpoint_invalid"
		}
		report(update)
		if pause || update.fatal != nil {
			return
		}
		interval := c.PollDuration()
		if interval <= 0 {
			interval = time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
