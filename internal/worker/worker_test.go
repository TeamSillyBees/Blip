package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/channels"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type fakeSender struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *fakeSender) Send(ctx context.Context, d config.Destination, e event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[d.ID]++
	if d.ID == "bad" {
		return &channels.SendError{Message: "HTTP 503", Retryable: true}
	}
	return nil
}

func TestSeparateReceiptsRetryAndRestart(t *testing.T) {
	q := store.Queue{Dir: t.TempDir()}
	now := time.Now().UTC()
	e := event.Event{SchemaVersion: 1, ID: event.ID("event"), Agent: "codex", Kind: event.InputRequested, OccurredAt: now, SessionID: "s", TurnID: "t", Project: "p"}
	_, err := q.Enqueue(context.Background(), e, []string{"good", "bad"})
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{calls: map[string]int{}}
	dispatch := Dispatcher{Queue: q, Sender: sender, MaxAttempts: 2, Timeout: time.Second}
	var group sync.WaitGroup
	for _, id := range []string{"good", "bad"} {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			if _, err := dispatch.DeliverOnce(context.Background(), config.Destination{ID: id}, now.Add(time.Second)); err != nil {
				t.Error(err)
			}
		}(id)
	}
	group.Wait()
	jobs, _ := q.Load()
	if jobs[0].Deliveries[0].DeliveredAt == nil || jobs[0].Deliveries[1].Attempts != 1 {
		t.Fatal("one channel overwrote another receipt")
	}
	// Constructing a new dispatcher models a restart: only the failing destination remains due.
	dispatch = Dispatcher{Queue: store.Queue{Dir: q.Dir}, Sender: sender, MaxAttempts: 2, Timeout: time.Second}
	if sent, err := dispatch.DeliverOnce(context.Background(), config.Destination{ID: "good"}, now.Add(time.Minute)); sent || err != nil {
		t.Fatal("successful destination was sent twice")
	}
	if sent, err := dispatch.DeliverOnce(context.Background(), config.Destination{ID: "bad"}, now.Add(2*time.Second)); sent || err != nil {
		t.Fatal("retry backoff ignored")
	}
	_, err = dispatch.DeliverOnce(context.Background(), config.Destination{ID: "bad"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ = q.Load()
	if !jobs[0].Deliveries[1].Failed || jobs[0].Deliveries[1].Attempts != 2 {
		t.Fatal("finite retry budget not enforced")
	}
	if sender.calls["good"] != 1 || sender.calls["bad"] != 2 {
		t.Fatal("unexpected notification count")
	}
}
