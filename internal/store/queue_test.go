package store

import (
	"context"
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
	if len(jobs) != 0 {
		t.Fatal("terminal receipts did not expire")
	}
}
