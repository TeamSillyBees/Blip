package worker

import (
	"context"
	"log"
	"testing"
	"testing/synctest"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func enqueueTurn(t *testing.T, q store.Queue, turn string, destinations ...string) {
	t.Helper()
	e := event.Event{SchemaVersion: 1, ID: event.ID(turn), Agent: "codex", Kind: event.TurnCompleted, OccurredAt: time.Now(), SessionID: "session", SessionTitle: "title", TurnID: turn}
	if added, err := q.Enqueue(context.Background(), e, destinations); err != nil || !added {
		t.Fatalf("enqueue %s: added=%v err=%v", turn, added, err)
	}
}

func startTestDeliveries(t *testing.T, d Dispatcher, kind string) (chan<- string, func()) {
	t.Helper()
	pending, err := d.Queue.PendingIndex()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- d.runDeliveries(ctx, []config.Destination{{ID: "out", Kind: kind}}, pending, changes, log.Default())
	}()
	synctest.Wait()
	return changes, func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestIdleDeliveryWaitsForPublicationAndRetryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := store.Queue{Dir: t.TempDir()}
		sender := &recordingSender{} // The first attempt fails, the second succeeds.
		changes, stop := startTestDeliveries(t, Dispatcher{Queue: q, Sender: sender, Timeout: time.Second, MaxAttempts: 2}, "webhook")
		defer stop()
		enqueueTurn(t, q, "new", "out")
		time.Sleep(time.Second)
		synctest.Wait()
		if sender.notificationCount() != 0 {
			t.Fatal("idle dispatcher scanned the queue before a wake")
		}
		changes <- event.ID("new") + ".json"
		synctest.Wait()
		if sender.notificationCount() != 1 {
			t.Fatal("publication did not trigger the first attempt")
		}
		time.Sleep(5*time.Second - time.Millisecond)
		changes <- ""
		synctest.Wait()
		if sender.notificationCount() != 1 {
			t.Fatal("a second wake bypassed retry backoff")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if sender.notificationCount() != 2 {
			t.Fatal("retry required another publication")
		}
	})
}

func TestBacklogAndNewPublicationsRespectChannelPacing(t *testing.T) {
	for _, tc := range []struct {
		kind string
		gap  time.Duration
	}{{"wecom", 3100 * time.Millisecond}, {"feishu", 650 * time.Millisecond}} {
		t.Run(tc.kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := store.Queue{Dir: t.TempDir()}
				enqueueTurn(t, q, "offline", "out")
				sender := &fakeSender{calls: map[string]int{}}
				changes, stop := startTestDeliveries(t, Dispatcher{Queue: q, Sender: sender, Timeout: time.Second, MaxAttempts: 2}, tc.kind)
				defer stop()
				if sender.callCount("out") != 1 {
					t.Fatal("startup did not drain the offline job")
				}
				enqueueTurn(t, q, "second", "out")
				enqueueTurn(t, q, "third", "out")
				changes <- "" // Recover both publications if their native events were lost.
				time.Sleep(tc.gap - time.Millisecond)
				synctest.Wait()
				if sender.callCount("out") != 1 {
					t.Fatal("publication bypassed channel pacing")
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if sender.callCount("out") != 2 {
					t.Fatal("first queued job missed its deadline")
				}
				time.Sleep(tc.gap)
				synctest.Wait()
				if sender.callCount("out") != 3 {
					t.Fatal("coalesced wake did not drain the backlog")
				}
			})
		})
	}
}

func TestStalePendingReceiptsAreCheckedBeforeSending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := store.Queue{Dir: t.TempDir()}
		for _, turn := range []string{"sent", "failed", "retry", "new"} {
			enqueueTurn(t, q, turn, "out")
			time.Sleep(time.Millisecond)
		}
		pending, err := q.PendingIndex()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		for turn, update := range map[string]func(*store.Delivery){
			"sent":   func(r *store.Delivery) { r.DeliveredAt = &now },
			"failed": func(r *store.Delivery) { r.Failed = true },
			"retry":  func(r *store.Delivery) { r.NextAttempt = now.Add(time.Minute) },
		} {
			if err := q.UpdateDelivery(context.Background(), event.ID(turn), "out", update); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		sender := &fakeSender{calls: map[string]int{}}
		d := Dispatcher{Queue: q, Sender: sender, Timeout: time.Second, MaxAttempts: 2}
		done := make(chan error, 1)
		go func() {
			done <- d.runDeliveries(ctx, []config.Destination{{ID: "out", Kind: "wecom"}}, pending, nil, log.Default())
		}()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		fresh, err := q.ReadJob(event.ID("new"))
		if err != nil || sender.calls["out"] != 1 || fresh.Deliveries[0].DeliveredAt == nil {
			t.Fatalf("stale receipt sent or delayed new job: calls=%v, err=%v", sender.calls, err)
		}
	})
}

type blockingDestinationSender chan string

func (s blockingDestinationSender) Send(ctx context.Context, d config.Destination, e event.Event) error {
	s <- d.ID + "/" + e.TurnID
	if d.ID == "slow" {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestSlowDestinationDoesNotBlockFollowingJobs(t *testing.T) {
	q := store.Queue{Dir: t.TempDir()}
	enqueueTurn(t, q, "first", "slow", "fast")
	pending, err := q.PendingIndex()
	if err != nil {
		t.Fatal(err)
	}
	sent := make(blockingDestinationSender, 4)
	d := Dispatcher{Queue: q, Sender: sent, Timeout: time.Minute, MaxAttempts: 2}
	done := make(chan error, 1)
	changes := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done <- d.runDeliveries(ctx, []config.Destination{{ID: "slow"}, {ID: "fast"}}, pending, changes, log.Default())
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	seen := map[string]bool{}
	for len(seen) < 3 {
		select {
		case attempt := <-sent:
			if seen[attempt] || attempt == "slow/second" {
				t.Fatalf("unexpected delivery %q after %v", attempt, seen)
			}
			seen[attempt] = true
			if len(seen) == 2 {
				enqueueTurn(t, q, "second", "slow", "fast")
				changes <- event.ID("second") + ".json"
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("fast destination could not drain the backlog: %v", seen)
		}
	}
}
