package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func testLedger(t *testing.T) (handoffs, func(event.Event) error) {
	t.Helper()
	c := sourceConfig(t)
	h := handoffs{config: c, source: c.Sources[0]}
	return h, func(e event.Event) error {
		_, err := (store.Queue{Dir: c.StateDir}).Enqueue(context.Background(), e, []string{"test"})
		return err
	}
}
func ledgerEvents(t *testing.T, h handoffs) []event.Event {
	t.Helper()
	jobs, err := (store.Queue{Dir: h.config.StateDir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	var result []event.Event
	for _, job := range jobs {
		result = append(result, job.Event)
	}
	return result
}
func completed(h handoffs, at time.Time) event.Event {
	return event.Event{SchemaVersion: 2, Agent: h.source.Harness, SourceID: h.source.ID, Source: h.source.Harness + "/rollout", Kind: event.TurnCompleted, SessionID: "s", TurnID: "t", OccurredAt: at, Project: "p"}
}

func TestStopDeadlineSurvivesDuplicateAndRestart(t *testing.T) {
	h, emit := testLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.stop(ctx, "s", "t", "t", "/project", now); err != nil {
		t.Fatal(err)
	}
	if err := h.stop(ctx, "s", "t", "t", "/project", now.Add(900*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := h.flush(ctx, now.Add(999*time.Millisecond), emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 0 {
		t.Fatal("candidate notified before its deadline")
	}
	restarted := handoffs{config: h.config, source: h.source}
	if err := restarted.flush(ctx, now.Add(time.Second), emit); err != nil {
		t.Fatal(err)
	}
	events := ledgerEvents(t, h)
	if len(events) != 1 || events[0].Kind != event.AttentionRequired || events[0].Reason != "stop_signal" || !events[0].OccurredAt.Equal(now) {
		t.Fatalf("bad stop reminder: %+v", events)
	}
	if err := restarted.finish(ctx, completed(h, now.Add(2*time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 1 {
		t.Fatal("late completion duplicated stop reminder")
	}
}

func TestParallelRequestsAndLateReplayStayResolved(t *testing.T) {
	h, emit := testLedger(t)
	ctx, now := context.Background(), time.Now()
	makeRequest := func(id string) event.Event {
		e := completed(h, now)
		e.Kind = event.InputRequested
		e.RequestID = id
		e.ID = event.SourceEventID(h.source.ID, h.source.Harness, "request", id)
		return e
	}
	for _, id := range []string{"q1", "q2"} {
		if err := h.request(ctx, makeRequest(id), "t", emit); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.resolveRequest(ctx, "s", "t", "t", "q2", true); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(ctx, completed(h, now.Add(time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 2 {
		t.Fatal("one resolved request cleared another outstanding wait")
	}
	if err := h.resolveRequest(ctx, "s", "t", "t", "q1", true); err != nil {
		t.Fatal(err)
	}
	if err := h.request(ctx, makeRequest("q1"), "t", emit); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(ctx, completed(h, now.Add(2*time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 2 {
		t.Fatal("late completion duplicated the stop already covered by a request")
	}
	if err := h.resume(ctx, "s", "t", "t", "continued-tool", false, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(ctx, completed(h, now.Add(4*time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 3 {
		t.Fatal("replayed answered request hid completion")
	}
}

func TestExplicitResumeOpensOneNewHandoff(t *testing.T) {
	h, emit := testLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.stop(ctx, "s", "t", "t", "/p", now); err != nil {
		t.Fatal(err)
	}
	if err := h.flush(ctx, now.Add(time.Second), emit); err != nil {
		t.Fatal(err)
	}
	// A start record whose timestamp predates Stop must not count as resume.
	if err := h.resume(ctx, "s", "t", "t", "late-old-start", true, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(ctx, completed(h, now.Add(time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 1 {
		t.Fatal("delayed start record fabricated a resumed turn")
	}
	if err := h.resume(ctx, "s", "t", "t", "new-tool", false, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// A late final record from before resume does not end the new episode.
	if err := h.finish(ctx, completed(h, now.Add(time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 1 {
		t.Fatal("late prior completion ended the new episode")
	}
	if err := h.finish(ctx, completed(h, now.Add(3*time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	if err := h.resume(ctx, "s", "t", "t", "new-tool", false, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.resume(ctx, "s", "t", "t", "maintenance-tool", false, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(ctx, completed(h, now.Add(6*time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	events := ledgerEvents(t, h)
	if len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("wrong resumed handoffs: %+v", events)
	}
}

func TestCancellationAndSpecificRequestsSuppressGenericStop(t *testing.T) {
	for _, kind := range []event.Kind{"cancel", event.InputRequested, event.ApprovalRequested} {
		t.Run(string(kind), func(t *testing.T) {
			h, emit := testLedger(t)
			ctx := context.Background()
			now := time.Now().UTC()
			if err := h.stop(ctx, "s", "t", "t", "/p", now); err != nil {
				t.Fatal(err)
			}
			want := 1
			if kind == "cancel" {
				want = 0
				if err := h.cancel(ctx, "s", "t", "t", now.Add(time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			} else {
				e := completed(h, now.Add(time.Millisecond))
				e.Kind = kind
				e.RequestID = "req"
				e.ID = event.SourceEventID(h.source.ID, h.source.Harness, "request", "req")
				if err := h.request(ctx, e, "t", emit); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.flush(ctx, now.Add(time.Second), emit); err != nil {
				t.Fatal(err)
			}
			if err := h.finish(ctx, completed(h, now.Add(2*time.Second)), "t", emit); err != nil {
				t.Fatal(err)
			}
			if len(ledgerEvents(t, h)) != want {
				t.Fatal("request/cancel produced an extra final reminder")
			}
			if kind == event.InputRequested {
				if err := h.resolveApproval(ctx, "s", "t", "t", "req"); err != nil {
					t.Fatal(err)
				}
				if err := h.stop(ctx, "s", "t", "t", "/p", now.Add(3*time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := h.flush(ctx, now.Add(5*time.Second), emit); err != nil {
					t.Fatal(err)
				}
				if len(ledgerEvents(t, h)) != 1 {
					t.Fatal("async question tool return treated as user answer")
				}
			}
		})
	}
}

func TestEnqueueCrashRecoveryAndStalePendingRevision(t *testing.T) {
	h, emit := testLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.stop(ctx, "s", "t", "t", "/p", now); err != nil {
		t.Fatal(err)
	}
	key := h.key("s", "t")
	pendingPath := h.path(key, true)
	old, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	simulatedCrash := errors.New("exit after enqueue before ledger save")
	err = h.flush(ctx, now.Add(time.Second), func(e event.Event) error {
		if err := emit(e); err != nil {
			return err
		}
		return simulatedCrash
	})
	if !errors.Is(err, simulatedCrash) {
		t.Fatal(err)
	}
	if err = h.flush(ctx, now.Add(2*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 1 {
		t.Fatal("recovery duplicated queue notification")
	}
	if err = store.WriteFile(pendingPath, old); err != nil {
		t.Fatal(err)
	}
	if err = h.flush(ctx, now.Add(3*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, h)) != 1 {
		t.Fatal("older pending revision resurrected")
	}
	if _, err = os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatal("stale pending revision was not cleaned")
	}
}

func TestConcurrentStopsAndFinalShareOneSnapshot(t *testing.T) {
	h, emit := testLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.stop(ctx, "s", "t", "t", "/p", now); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if i%2 == 0 {
				failures <- h.flush(ctx, now.Add(time.Second), emit)
			} else {
				failures <- h.finish(ctx, completed(h, now.Add(time.Second)), "t", emit)
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Error(err)
		}
	}
	if len(ledgerEvents(t, h)) != 1 {
		t.Fatal("concurrent paths created multiple snapshots")
	}
}

func TestLegacySnapshotControlsOutcomeAfterUpgrade(t *testing.T) {
	h, emit := testLedger(t)
	h.source.ID = config.DefaultSourceID
	ctx := context.Background()
	now := time.Now().UTC()
	e := completed(h, now)
	e.SchemaVersion = 1
	e.SourceID = ""
	e.ID = event.ID("codex", "s", "t", string(event.TurnCompleted))
	if err := emit(e); err != nil {
		t.Fatal(err)
	}
	if err := h.stop(ctx, "s", "t", "t", "/p", now); err != nil {
		t.Fatal(err)
	}
	if err := h.flush(ctx, now.Add(time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := h.resume(ctx, "s", "t", "t", "maintenance", false, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(ctx, completed(h, now.Add(3*time.Second)), "t", emit); err != nil {
		t.Fatal(err)
	}
	events := ledgerEvents(t, h)
	if len(events) != 1 || events[0].SchemaVersion != 1 {
		t.Fatal("upgrade replaced first snapshot or fabricated continuation")
	}
}

func TestCorruptHandoffStateIsPreserved(t *testing.T) {
	h, emit := testLedger(t)
	now := time.Now().UTC()
	ctx := context.Background()
	if err := h.stop(ctx, "s", "t", "t", "/p", now); err != nil {
		t.Fatal(err)
	}
	path := h.path(h.key("s", "t"), true)
	if err := os.WriteFile(path, []byte(`{"format":1,"revision":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.flush(ctx, now.Add(time.Second), emit); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("corrupt state accepted: %v", err)
	}
	data, _ := os.ReadFile(path)
	var state map[string]any
	if json.Unmarshal(data, &state) != nil || len(state) != 2 {
		t.Fatal("corrupt state reset")
	}
	if _, err := os.Stat(filepath.Join(h.root(), "settled", h.key("s", "t")+".json")); !os.IsNotExist(err) {
		t.Fatal("corrupt candidate became settled")
	}
}
