package worker

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func (s *fakeSender) callCount(destinationID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[destinationID]
}

type recordingSender struct {
	mu            sync.Mutex
	notifications []event.Event
}

func (s *recordingSender) notificationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.notifications)
}

func (s *recordingSender) Send(_ context.Context, _ config.Destination, notification event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, notification)
	if len(s.notifications) == 1 {
		return &channels.SendError{Message: "HTTP 503", Retryable: true}
	}
	return nil
}

func TestDeviceNamesSurviveQueuedRetriesAndLegacyRecords(t *testing.T) {
	for _, entry := range []struct {
		name, stored, configured, expected string
	}{
		{"captured name", "原始工作站", "重命名后的工作站", "原始工作站"},
		{"legacy record", "", "配置中的设备", "配置中的设备"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			ctx := context.Background()
			q := store.Queue{Dir: t.TempDir()}
			now := time.Now().UTC()
			notification := event.Event{SchemaVersion: 1, ID: event.ID(entry.name), Agent: "codex", DeviceName: entry.stored, Kind: event.TurnCompleted, OccurredAt: now, SessionID: "session", TurnID: "turn"}
			if _, err := q.Enqueue(ctx, notification, []string{"destination"}); err != nil {
				t.Fatal(err)
			}
			sender := &recordingSender{}
			dispatcher := Dispatcher{Queue: q, Sender: sender, DeviceName: entry.configured, MaxAttempts: 2, Timeout: time.Second}
			for _, due := range []time.Time{now.Add(time.Second), now.Add(time.Minute)} {
				if sent, err := dispatcher.DeliverOnce(ctx, config.Destination{ID: "destination"}, due); !sent || err != nil {
					t.Fatalf("send failed: sent=%v, error=%v", sent, err)
				}
			}
			if len(sender.notifications) != 2 {
				t.Fatalf("wrong retry count: %d", len(sender.notifications))
			}
			for _, delivered := range sender.notifications {
				if delivered.DeviceName != entry.expected || delivered.ID != notification.ID {
					t.Fatalf("device name or identity changed: %+v", delivered)
				}
			}
			jobs, err := q.Load()
			if err != nil || len(jobs) != 1 {
				t.Fatalf("load persisted job: %v", err)
			}
			if jobs[0].Event.DeviceName != entry.stored || jobs[0].Deliveries[0].DeliveredAt == nil {
				t.Fatal("delivery rewrote captured metadata or failed to record successful retry")
			}
		})
	}
}

func TestSessionTitlesPreserveCapturedNamesAndFillLegacyJobs(t *testing.T) {
	for _, stored := range []string{"入队时的对话名", ""} {
		t.Run(stored, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte("{\"id\":\"session\",\"thread_name\":\"当前对话名\"}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			q := store.Queue{Dir: t.TempDir()}
			now := time.Now().UTC()
			notification := event.Event{SchemaVersion: 1, ID: event.ID("title", stored), Agent: "codex", Kind: event.TurnCompleted, OccurredAt: now, SessionID: "session", SessionTitle: stored, TurnID: "turn"}
			if _, err := q.Enqueue(ctx, notification, []string{"destination"}); err != nil {
				t.Fatal(err)
			}
			sender := &recordingSender{}
			dispatcher := Dispatcher{Queue: q, Sender: sender, CodexHome: home, MaxAttempts: 2, Timeout: time.Second}
			for _, due := range []time.Time{now.Add(time.Second), now.Add(time.Minute)} {
				if sent, err := dispatcher.DeliverOnce(ctx, config.Destination{ID: "destination"}, due); !sent || err != nil {
					t.Fatalf("send failed: sent=%v error=%v", sent, err)
				}
			}
			want := stored
			if want == "" {
				want = "当前对话名"
			}
			for _, sent := range sender.notifications {
				if sent.SessionTitle != want {
					t.Fatalf("title=%q, want %q", sent.SessionTitle, want)
				}
			}
			jobs, err := q.Load()
			if err != nil || len(jobs) != 1 || jobs[0].Event.SessionTitle != stored {
				t.Fatalf("delivery changed stored title: %v", err)
			}
		})
	}
}

func TestStopInterruptsLongPollAndPreservesInflightJob(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "sending"}[inflight], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			started := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				started <- struct{}{}
				<-r.Context().Done()
			}))
			defer server.Close()
			defer cancel()
			dir := t.TempDir()
			c := config.Config{StateDir: filepath.Join(dir, "state"), CodexHome: filepath.Join(dir, "codex"), PollInterval: "1m", RequestTimeout: "1m", MaxAttempts: 2, Destinations: []config.Destination{{ID: "one", Kind: "webhook", Enabled: true, URL: server.URL}}}
			q := store.Queue{Dir: c.StateDir}
			if inflight {
				e := event.Event{SchemaVersion: 1, ID: event.ID("shutdown"), Agent: "codex", Kind: event.TurnCompleted, OccurredAt: time.Now().UTC(), SessionID: "s", TurnID: "t"}
				if _, err := q.Enqueue(ctx, e, []string{"one"}); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, c, log.New(io.Discard, "", 0)) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				var status Status
				if store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.Ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker not ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if inflight {
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("send did not start")
				}
			}
			if err := store.WriteFile(filepath.Join(c.StateDir, "stop"), []byte("stop\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("stop waited for the one-minute polling interval")
			}
			if inflight {
				jobs, err := q.Load()
				if err != nil || len(jobs) != 1 {
					t.Fatalf("pending job lost: %v", err)
				}
				receipt := jobs[0].Deliveries[0]
				if receipt.DeliveredAt != nil || receipt.Failed || receipt.Attempts != 0 {
					t.Fatal("shutdown consumed delivery retry budget")
				}
			}
		})
	}
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
	for _, receipt := range jobs[0].Deliveries {
		if receipt.LastAttemptAt == nil || receipt.LastAttemptAt.IsZero() {
			t.Fatal("delivery attempt time was not recorded")
		}
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
