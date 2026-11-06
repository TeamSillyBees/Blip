//go:build windows

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

type runtimeExit struct {
	specific bool
	code     uint32
}

type runtimeFixture struct {
	requests chan svc.ChangeRequest
	changes  chan svc.Status
	exit     chan runtimeExit
	finished chan error
	close    func()
}

func startRuntime(t *testing.T, interval time.Duration, runner func(context.Context, func()) error) *runtimeFixture {
	t.Helper()
	f := &runtimeFixture{
		requests: make(chan svc.ChangeRequest, 10), changes: make(chan svc.Status, 100),
		exit: make(chan runtimeExit, 1), finished: make(chan error, 1),
	}
	var once sync.Once
	f.close = func() { once.Do(func() { close(f.requests) }) }
	terminated := make(chan struct{})
	h := &runtimeHandler{runner: runner, finished: f.finished, tickInterval: interval}
	go func() {
		specific, code := h.Execute(nil, f.requests, f.changes)
		f.exit <- runtimeExit{specific, code}
		close(terminated)
	}()
	t.Cleanup(func() {
		f.close()
		select {
		case <-terminated:
		case <-time.After(2 * time.Second):
			t.Error("service handler did not terminate")
		}
	})
	return f
}

func (f *runtimeFixture) status(t *testing.T, expected svc.State) svc.Status {
	t.Helper()
	select {
	case status := <-f.changes:
		if status.State != expected {
			t.Fatalf("service status = %v, want %v", status.State, expected)
		}
		return status
	case <-time.After(2 * time.Second):
		t.Fatalf("service did not report %v", expected)
		return svc.Status{}
	}
}

func (f *runtimeFixture) result(t *testing.T, specific bool, code uint32) error {
	t.Helper()
	select {
	case result := <-f.exit:
		if result.specific != specific || result.code != code {
			t.Fatalf("service exit = %+v, want specific=%v code=%v", result, specific, code)
		}
		return <-f.finished
	case <-time.After(2 * time.Second):
		t.Fatal("service did not exit")
		return nil
	}
}

func TestRuntimeWaitsForReadyAndStopsGracefully(t *testing.T) {
	for _, control := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(map[svc.Cmd]string{svc.Stop: "stop", svc.Shutdown: "shutdown"}[control], func(t *testing.T) {
			initialize := make(chan struct{})
			f := startRuntime(t, time.Hour, func(ctx context.Context, ready func()) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-initialize:
				}
				ready()
				ready() // A duplicate report must not panic or emit a second Running.
				<-ctx.Done()
				ready() // A late report must not reactivate a stopping service.
				return ctx.Err()
			})
			pending := f.status(t, svc.StartPending)
			if pending.Accepts != 0 || pending.CheckPoint == 0 || pending.WaitHint == 0 {
				t.Fatalf("invalid startup status: %+v", pending)
			}
			f.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Stopped}}
			if got := f.status(t, svc.StartPending); got != pending {
				t.Fatal("interrogate did not return the handler's startup status")
			}
			close(initialize)
			running := f.status(t, svc.Running)
			if running.Accepts != svc.AcceptStop|svc.AcceptShutdown || running.CheckPoint != 0 || running.WaitHint != 0 {
				t.Fatalf("invalid running status: %+v", running)
			}
			f.requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			if got := f.status(t, svc.Running); got != running {
				t.Fatal("interrogate did not return running status")
			}
			f.requests <- svc.ChangeRequest{Cmd: control}
			f.status(t, svc.StopPending)
			if err := f.result(t, false, 0); err != nil {
				t.Fatalf("requested stop returned an error: %v", err)
			}
			if len(f.changes) != 0 {
				t.Fatal("late readiness emitted a new service state")
			}
		})
	}
}

func TestRuntimeStartupFailureDoesNotReportReady(t *testing.T) {
	want := errors.New("configuration could not be loaded")
	f := startRuntime(t, time.Hour, func(context.Context, func()) error { return want })
	f.status(t, svc.StartPending)
	if err := f.result(t, true, 1); !errors.Is(err, want) {
		t.Fatalf("service failure was not preserved: %v", err)
	}
	if len(f.changes) != 0 {
		t.Fatal("failed initialization reported a running service")
	}
}

func TestRuntimeWorkerExitUsesSCMFailureCode(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "local stop", true: "worker failure"}[failed], func(t *testing.T) {
			finish := make(chan struct{})
			want := errors.New("worker state could not be saved")
			f := startRuntime(t, time.Hour, func(ctx context.Context, ready func()) error {
				ready()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-finish:
				}
				if failed {
					return want
				}
				return nil
			})
			f.status(t, svc.StartPending)
			f.status(t, svc.Running)
			close(finish)
			code := uint32(0)
			if failed {
				code = 1
			}
			err := f.result(t, failed, code)
			if failed && !errors.Is(err, want) || !failed && err != nil {
				t.Fatalf("wrong worker exit error: %v", err)
			}
		})
	}
}

func TestRuntimePendingCheckpointsAndCancellationBeforeReady(t *testing.T) {
	for _, closeControls := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop request", true: "closed controls"}[closeControls], func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			f := startRuntime(t, 10*time.Millisecond, func(ctx context.Context, ready func()) error {
				<-ctx.Done()
				ready()
				<-release
				return ctx.Err()
			})
			initial := f.status(t, svc.StartPending)
			progress := f.status(t, svc.StartPending)
			if progress.CheckPoint <= initial.CheckPoint || progress.WaitHint == 0 {
				t.Fatal("startup did not update its checkpoint")
			}
			if closeControls {
				f.close()
			} else {
				f.requests <- svc.ChangeRequest{Cmd: svc.Stop}
			}
			var stopped svc.Status
			deadline := time.After(2 * time.Second)
		waitForStop:
			for {
				select {
				case stopped = <-f.changes:
					if stopped.State == svc.StopPending {
						break waitForStop
					}
					if stopped.State != svc.StartPending {
						t.Fatalf("unexpected state during startup cancellation: %+v", stopped)
					}
				case <-deadline:
					t.Fatal("service did not begin stopping")
				}
			}
			if next := f.status(t, svc.StopPending); next.CheckPoint <= stopped.CheckPoint || next.Accepts != 0 {
				t.Fatalf("invalid shutdown progress: %+v", next)
			}
			finish()
			if err := f.result(t, false, 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}
