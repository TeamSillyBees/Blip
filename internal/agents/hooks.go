package agents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

const MaxHookBytes = 1024 * 1024
const HookBudget = 3 * time.Second

type HookDiagnostic struct {
	ReceivedAt  time.Time  `json:"received_at"`
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`
}

func DiagnosticPath(c config.Config, id string) string {
	return filepath.Join(c.StateDir, "sources", id, "last-hook.json")
}

// ReadHook bounds both payload size and the wait for EOF. The CLI supplies a
// closable stdin pipe; cancellation closes it to release a blocked OS read.
func ReadHook(ctx context.Context, reader io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() { data, err := io.ReadAll(io.LimitReader(reader, MaxHookBytes+1)); done <- result{data, err} }()
	select {
	case <-ctx.Done():
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, ctx.Err()
	case read := <-done:
		if read.err != nil || len(read.data) > MaxHookBytes {
			return nil, fmt.Errorf("hook payload is unreadable or larger than 1 MiB")
		}
		return read.data, nil
	}
}

// Capture never sends over the network. Errors are returned for diagnostics;
// command adapters keep their neutral stdout and exit status separately.
func Capture(ctx context.Context, c config.Config, id string, reader io.Reader) (result error) {
	ctx, cancel := context.WithTimeout(ctx, HookBudget)
	defer cancel()
	received := time.Now().UTC()
	diagnostic := HookDiagnostic{ReceivedAt: received}
	if config.ValidSourceID(id) {
		defer func() {
			if result != nil {
				diagnostic.ErrorCode = "hook_failed"
				if errors.Is(result, context.DeadlineExceeded) {
					diagnostic.ErrorCode = "hook_timeout"
				}
			}
			// A small diagnostic write is best effort and contains no raw payload.
			_ = store.WriteJSON(DiagnosticPath(c, id), diagnostic)
		}()
	}
	data, err := ReadHook(ctx, reader)
	if err != nil {
		return err
	}
	source, exists := c.Source(id)
	if !exists || !source.Enabled {
		return nil
	}
	if err = Bind(ctx, c, source); err != nil {
		return err
	}
	adapter, err := Builtins().Open(c, source)
	if err != nil {
		return err
	}
	destinations := []string{}
	for _, d := range c.Enabled() {
		destinations = append(destinations, d.ID)
	}
	emit := func(e event.Event) error {
		e.DeviceName = c.DeviceName
		if resolver, ok := adapter.(TitleResolver); ok {
			titleCtx, stop := context.WithTimeout(ctx, 50*time.Millisecond)
			e.SessionTitle = resolver.SessionTitle(titleCtx, e.SessionID)
			stop()
		}
		if _, err := (store.Queue{Dir: c.StateDir}).Enqueue(ctx, e, destinations); err != nil {
			return err
		}
		now := received
		diagnostic.LastEventAt = &now
		return nil
	}
	if consumer, ok := adapter.(HookConsumer); ok {
		return consumer.ConsumeHook(ctx, data, received, emit)
	}
	events, err := adapter.ParseHook(ctx, data, received)
	if err != nil {
		return err
	}
	for _, e := range events {
		if err = emit(e); err != nil {
			return err
		}
	}
	return nil
}
