package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// Keep lifecycle selection in one place: a registered service must never be
// replaced by a second, unmanaged worker when applying configuration.
type lifecycle struct {
	query        func(string) (service.Status, error)
	startService func(context.Context, string) error
	stopService  func(context.Context, string) error
	startWorker  func(context.Context, config.Config, io.Writer) error
	stopWorker   func(context.Context, config.Config, io.Writer) error
}

var errStopPending = errors.New("worker is still stopping")

func manageLifecycle(ctx context.Context, c config.Config, action string, output io.Writer) error {
	m := lifecycle{query: service.Query, startService: service.Start, stopService: service.Stop, startWorker: start, stopWorker: stop}
	return m.run(ctx, c, action, output)
}

func (m lifecycle) run(ctx context.Context, c config.Config, action string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("unknown lifecycle action %q", action)
	}
	status, err := m.query(c.Path)
	if err != nil && !errors.Is(err, service.ErrUnsupported) {
		return err
	}
	if status.Installed {
		if action == "stop" || action == "restart" {
			if err := m.stopService(ctx, c.Path); err != nil {
				return err
			}
		}
		if action == "start" || action == "restart" {
			if err := m.startService(ctx, c.Path); err != nil {
				return err
			}
		}
		fmt.Fprintf(output, "Windows service %s.\n", map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}[action])
		return nil
	}
	if action == "stop" || action == "restart" {
		stopOutput := output
		if action == "restart" {
			stopOutput = io.Discard
		}
		if err := m.stopWorker(ctx, c, stopOutput); err != nil {
			return err
		}
	}
	if action == "start" || action == "restart" {
		return m.startWorker(ctx, c, output)
	}
	return nil
}

func stop(ctx context.Context, c config.Config, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	if !running {
		fmt.Fprintln(output, "Blip is stopped.")
		return nil
	}
	if err := store.WriteFile(filepath.Join(c.StateDir, "stop"), []byte("stop\n")); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, 10*time.Second, errStopPending)
	defer cancel()
	for {
		running, err = isRunning(c)
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintln(output, "Blip stopped.")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stop requested, but the worker has not stopped yet; inspect %s: %w", cliCommand(c.Path, "status"), context.Cause(ctx))
		case <-time.After(100 * time.Millisecond):
		}
	}
}
