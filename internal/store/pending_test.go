package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
)

func TestPendingIndexReconcilesWithoutOpeningUnchangedReceipts(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx := context.Background()
	e := queuedEvent()
	if _, err := q.Enqueue(ctx, e, []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateDelivery(ctx, e.ID, "one", func(d *Delivery) { d.Failed = true }); err != nil {
		t.Fatal(err)
	}
	p, err := q.PendingIndex()
	if err != nil || len(p.pending) != 1 {
		t.Fatalf("partially finished job missing on startup: %v", err)
	}
	now := time.Now().UTC()
	if err := q.UpdateDelivery(ctx, e.ID, "two", func(d *Delivery) { d.DeliveredAt = &now }); err != nil {
		t.Fatal(err)
	}
	// No file notification: the safety scan must discover the new receipt.
	if err := p.Reconcile(); err != nil || len(p.pending) != 0 || len(p.files) != 1 {
		t.Fatalf("terminal job still pending or no longer retained: %v", err)
	}
	// A JSON read would have to acquire a shared lock and fail here.
	lock, err := q.mutationLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if err := p.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if ids := p.CleanupCandidates(now); len(ids) != 0 {
		t.Fatalf("unexpired receipts selected for pruning: %v", ids)
	}
}

func TestPendingIndexRefreshIsTargetedAndQuarantinesConflicts(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	ctx := context.Background()
	old := queuedEvent()
	if _, err := q.Enqueue(ctx, old, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	p, err := q.PendingIndex()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(q.jobPath(old.ID))
	if err != nil {
		t.Fatal(err)
	}
	conflict := strings.Repeat("f", 64) + ".json"
	if err := os.WriteFile(filepath.Join(q.JobsDir(), conflict), raw, 0600); err != nil {
		t.Fatal(err)
	}
	other := queuedEvent()
	other.ID = event.ID("unrelated")
	if _, err := q.Enqueue(ctx, other, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(other.ID + ".json"); err != nil || len(p.pending) != 2 {
		t.Fatalf("named wake unexpectedly scanned another file: %v", err)
	}
	if err := p.Refresh(conflict); err != nil || len(p.pending) != 1 || p.pending[other.ID+".json"].Event.ID != other.ID {
		t.Fatalf("cached conflict remained dispatchable: %v", err)
	}
	if count, err := q.QuarantineCount(); err != nil || count != 2 {
		t.Fatalf("quarantined %d records: %v", count, err)
	}
}

func TestPendingIndexPruneRechecksReceiptAndMigratesLegacyFailure(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	e := queuedEvent()
	legacy := Job{Event: e, CreatedAt: old, Deliveries: []Delivery{{DestinationID: "a", Failed: true}, {DestinationID: "b", NextAttempt: old}}}
	if err := WriteJSON(q.jobPath(e.ID), legacy); err != nil {
		t.Fatal(err)
	}
	p, err := q.PendingIndex()
	if err != nil {
		t.Fatal(err)
	}
	prune := func(at time.Time) {
		t.Helper()
		if err := q.PruneJobs(context.Background(), p.CleanupCandidates(at), at); err != nil {
			t.Fatal(err)
		}
		if err := p.Reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	prune(now)
	name := e.ID + ".json"
	if job := p.pending[name]; len(job.Deliveries) != 2 || job.Deliveries[0].FailedAt == nil {
		t.Fatal("legacy failure did not acquire a timestamp or pending receipt was dropped")
	}
	legacy.Deliveries[0].FailedAt = &old
	legacy.Deliveries[1].DeliveredAt = &old
	if err := q.Save(legacy); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(name); err != nil {
		t.Fatal(err)
	}
	// Make the disk receipt newer than the cached expiry, without a wake.
	if err := q.UpdateDelivery(context.Background(), e.ID, "b", func(d *Delivery) { d.DeliveredAt = &now }); err != nil {
		t.Fatal(err)
	}
	prune(now)
	if len(p.files) != 1 {
		t.Fatal("pruned a recently delivered job")
	}
	prune(now.Add(7 * 24 * time.Hour))
	if len(p.files) != 0 {
		t.Fatal("expired record retained")
	}
}

func BenchmarkRetainedQueue(b *testing.B) {
	q := Queue{Dir: b.TempDir()}
	if err := os.MkdirAll(q.JobsDir(), 0700); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	job := Job{Event: queuedEvent(), CreatedAt: now, Deliveries: []Delivery{{DestinationID: "out", DeliveredAt: &now}}}
	for i := range 1000 {
		job.Event.ID = event.ID(strconv.Itoa(i))
		raw, err := json.Marshal(job)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(q.jobPath(job.Event.ID), raw, 0600); err != nil {
			b.Fatal(err)
		}
	}
	p, err := q.PendingIndex()
	if err != nil {
		b.Fatal(err)
	}
	b.Run("FullLoad", func(b *testing.B) {
		for b.Loop() {
			if _, err := q.Load(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Reconcile", func(b *testing.B) {
		for b.Loop() {
			if err := p.Reconcile(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("NamedPublication", func(b *testing.B) {
		e := queuedEvent()
		if _, err := q.Enqueue(context.Background(), e, []string{"out"}); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if err := p.Refresh(e.ID + ".json"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
