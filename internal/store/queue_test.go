package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
)

func queuedEvent() event.Event {
	return event.Event{SchemaVersion: 1, ID: event.ID("one"), Agent: "codex", Kind: event.InputRequested, Source: "codex/hooks", OccurredAt: time.Now().UTC(), SessionID: "session", TurnID: "turn", Project: "project"}
}

func TestConcurrentEnqueueAndReceiptsSurviveRestart(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	e := queuedEvent()
	var group sync.WaitGroup
	var inserted atomic.Int32
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			added, err := q.Enqueue(context.Background(), e, []string{"a", "b"})
			if err != nil {
				t.Error(err)
			}
			if added {
				inserted.Add(1)
			}
		}()
	}
	group.Wait()
	if inserted.Load() != 1 {
		t.Fatalf("inserted %d duplicate jobs", inserted.Load())
	}
	for _, id := range []string{"a", "b"} {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			if err := q.UpdateDelivery(context.Background(), e.ID, id, func(d *Delivery) { now := time.Now().UTC(); d.DeliveredAt = &now; d.Attempts++ }); err != nil {
				t.Error(err)
			}
		}(id)
	}
	group.Wait()
	restarted := Queue{Dir: q.Dir}
	jobs, err := restarted.Load()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("read queue after restart: %v", err)
	}
	for _, d := range jobs[0].Deliveries {
		if d.DeliveredAt == nil || d.Attempts != 1 {
			t.Fatal("a simultaneous receipt was lost")
		}
	}
	added, err := restarted.Enqueue(context.Background(), e, []string{"a", "b"})
	if err != nil || added {
		t.Fatal("replayed event was sent again")
	}
}

func TestWorkerLockExcludesSecondWorkerAndReleases(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	first, ok, err := q.WorkerLock()
	if err != nil || !ok {
		t.Fatal(err)
	}
	second, ok, err := q.WorkerLock()
	if err != nil || ok {
		t.Fatal("second worker acquired the same queue")
	}
	_ = second.Close()
	if err = first.Unlock(); err != nil {
		t.Fatal(err)
	}
	third, ok, err := q.WorkerLock()
	if err != nil || !ok {
		t.Fatal("lock was not released")
	}
	defer third.Unlock()
}

func TestPruneRetainsPendingAndExpiresTerminalReceipts(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	e := queuedEvent()
	_, _ = q.Enqueue(context.Background(), e, []string{"a"})
	jobs, _ := q.Load()
	jobs[0].CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
	_ = q.Save(jobs[0])
	if err := q.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	jobs, _ = q.Load()
	if len(jobs) != 1 {
		t.Fatal("pending work was pruned")
	}
	_ = q.UpdateDelivery(context.Background(), e.ID, "a", func(d *Delivery) { d.Failed = true })
	if err := q.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	jobs, _ = q.Load()
	if len(jobs) != 1 || jobs[0].Deliveries[0].FailedAt == nil {
		t.Fatal("newly failed old job was pruned immediately")
	}
	_ = q.UpdateDelivery(context.Background(), e.ID, "a", func(d *Delivery) { old := time.Now().Add(-8 * 24 * time.Hour); d.FailedAt = &old })
	if err := q.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	jobs, _ = q.Load()
	if len(jobs) != 0 {
		t.Fatal("terminal receipts did not expire")
	}
}

func TestCorruptJobIsPreservedWithoutBlockingHealthyDeliveryOrResettingReceipts(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx := context.Background()
	bad := queuedEvent()
	good := bad
	good.ID = event.ID("healthy")
	for _, e := range []event.Event{bad, good} {
		if _, err := q.Enqueue(ctx, e, []string{"one"}); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte(`{"secret-body-marker":"private","unfinished":`)
	if err := os.WriteFile(q.jobPath(bad.ID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.Load()
	if err != nil || len(jobs) != 1 || jobs[0].Event.ID != good.ID {
		t.Fatalf("healthy job blocked: %+v %v", jobs, err)
	}
	if count, err := q.QuarantineCount(); err != nil || count != 1 {
		t.Fatalf("quarantine count: %d %v", count, err)
	}
	dir := filepath.Join(q.Dir, "quarantine", "records", event.ID("queue-record", bad.ID+".json"))
	preserved, err := os.ReadFile(filepath.Join(dir, "record.json"))
	if err != nil || string(preserved) != string(raw) {
		t.Fatal("original corrupt bytes not preserved")
	}
	details, err := os.ReadFile(filepath.Join(dir, "details.json"))
	if err != nil || strings.Contains(string(details), "secret-body-marker") || !strings.Contains(string(details), "invalid JSON") {
		t.Fatal("unsafe or missing quarantine diagnosis")
	}
	restarted := Queue{Dir: q.Dir}
	if added, err := restarted.Enqueue(ctx, bad, []string{"one"}); err != nil || added {
		t.Fatal("replay reset isolated receipt history")
	}
	if err := restarted.UpdateDelivery(ctx, bad.ID, "one", func(*Delivery) {}); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("isolated receipt was writable: %v", err)
	}
	if err := restarted.Prune(time.Now().Add(30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if count, _ := restarted.QuarantineCount(); count != 1 {
		t.Fatal("quarantine evidence was pruned")
	}
}

func TestInvalidReceiptStructureIsNotSilentlyDropped(t *testing.T) {
	for _, change := range []func(*Job){func(j *Job) { j.Deliveries = nil }, func(j *Job) { j.Deliveries = append(j.Deliveries, j.Deliveries[0]) }, func(j *Job) { j.Deliveries[0].Attempts = -1 }, func(j *Job) { j.CreatedAt = time.Time{} }} {
		q := Queue{Dir: t.TempDir()}
		e := queuedEvent()
		_, _ = q.Enqueue(context.Background(), e, []string{"one"})
		jobs, _ := q.Load()
		change(&jobs[0])
		data, _ := json.Marshal(jobs[0])
		_ = os.WriteFile(q.jobPath(e.ID), data, 0600)
		jobs, err := q.Load()
		if err != nil || len(jobs) != 0 {
			t.Fatalf("bad receipts remained dispatchable: %v", err)
		}
		if count, _ := q.QuarantineCount(); count != 1 {
			t.Fatal("bad receipts were lost rather than isolated")
		}
	}
}

func TestConflictingEmbeddedIdentityCannotDispatchOrResetHealthyReceipts(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx := context.Background()
	claimed := queuedEvent()
	claimed.ID = strings.Repeat("0", 64)
	unrelated := queuedEvent()
	unrelated.ID = strings.Repeat("a", 64)
	for _, e := range []event.Event{claimed, unrelated} {
		_, _ = q.Enqueue(ctx, e, []string{"one"})
	}
	// This sorts after the valid job, reproducing a conflict discovered late in Load.
	data, err := os.ReadFile(q.jobPath(claimed.ID))
	if err != nil {
		t.Fatal(err)
	}
	wrongID := strings.Repeat("f", 64)
	if err = os.WriteFile(q.jobPath(wrongID), data, 0600); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.Load()
	if err != nil || len(jobs) != 1 || jobs[0].Event.ID != unrelated.ID {
		t.Fatalf("conflicted identity can dispatch: %+v %v", jobs, err)
	}
	if count, _ := q.QuarantineCount(); count != 2 {
		t.Fatalf("both original records must be preserved, got %d", count)
	}
	if added, err := q.Enqueue(ctx, claimed, []string{"one"}); err != nil || added {
		t.Fatal("claimed identity was reset")
	}
	if jobs, err = q.Load(); err != nil || len(jobs) != 1 {
		t.Fatal("conflict blocks unrelated work on subsequent reads")
	}
}

func TestPruneRetainsRecentlyDeliveredOldJobs(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx := context.Background()
	e := queuedEvent()
	_, _ = q.Enqueue(ctx, e, []string{"one"})
	jobs, _ := q.Load()
	jobs[0].CreatedAt = time.Now().Add(-20 * 24 * time.Hour)
	if err := q.Save(jobs[0]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := q.UpdateDelivery(ctx, e.ID, "one", func(d *Delivery) { d.DeliveredAt = &now; d.Attempts++ }); err != nil {
		t.Fatal(err)
	}
	if err := q.Prune(now); err != nil {
		t.Fatal(err)
	}
	jobs, _ = q.Load()
	if len(jobs) != 1 {
		t.Fatal("new delivery lost its dedupe retention window")
	}
	if added, _ := q.Enqueue(ctx, e, []string{"one"}); added {
		t.Fatal("recent completion replayed")
	}
}

func TestConcurrentPruneAndLoadDoNotFailOnDisappearingTerminalRecords(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	for i := 0; i < 30; i++ {
		e := queuedEvent()
		e.ID = event.ID(string(rune('a' + i)))
		_, _ = q.Enqueue(ctx, e, []string{"one"})
		var job Job
		_ = ReadJSON(q.jobPath(e.ID), &job)
		job.CreatedAt = old
		job.Deliveries[0].DeliveredAt = &old
		_ = q.Save(job)
	}
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 20; j++ {
				if _, err := q.Load(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		if err := q.Prune(now); err != nil {
			t.Error(err)
		}
	}()
	group.Wait()
	jobs, err := q.Load()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("prune incomplete: %d %v", len(jobs), err)
	}
}
