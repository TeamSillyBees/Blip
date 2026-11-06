//go:build windows

package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Run connects the worker to the Windows Service Control Manager. The runner
// reports readiness after initialization and must return when its context is
// cancelled. Its errors become service-specific failures reported to Windows.
func Run(name string, runner func(context.Context, func()) error) error {
	if name == "" || runner == nil {
		return fmt.Errorf("service runtime requires a name and a worker")
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service context: %w", err)
	}
	if !isService {
		return fmt.Errorf("service runtime must be started by Windows; use blip service start")
	}
	handler := &runtimeHandler{runner: runner, finished: make(chan error, 1)}
	if err := svc.Run(name, handler); err != nil {
		return fmt.Errorf("run Windows service: %w", err)
	}
	select {
	case err := <-handler.finished:
		return err
	default:
		return nil
	}
}

type runtimeHandler struct {
	runner       func(context.Context, func()) error
	finished     chan error
	tickInterval time.Duration
}

func (h *runtimeHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failure error
	defer func() {
		if h.finished != nil {
			h.finished <- failure
		}
	}()

	current := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 10000}
	changes <- current
	ready := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() { done <- h.runner(ctx, func() { once.Do(func() { close(ready) }) }) }()
	readySignal := (<-chan struct{})(ready)
	interval := h.tickInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	stopping := false
	stop := func() {
		if stopping {
			return
		}
		stopping = true
		readySignal = nil
		cancel()
		current = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 10000}
		changes <- current
	}
	for {
		select {
		case err := <-done:
			if err != nil && !stopping {
				failure = err
				return true, 1
			}
			return false, 0
		case <-readySignal:
			readySignal = nil
			current = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			changes <- current
		case request, ok := <-requests:
			if !ok {
				requests = nil
				stop()
				continue
			}
			switch request.Cmd {
			case svc.Interrogate:
				changes <- current
			case svc.Stop, svc.Shutdown:
				stop()
			}
		case <-ticker.C:
			if current.State == svc.StartPending || current.State == svc.StopPending {
				current.CheckPoint++
				changes <- current
			}
		}
	}
}
