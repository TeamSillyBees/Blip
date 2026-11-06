package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestFirstDispatchTimeSurvivesRetryAndRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	q := store.Queue{Dir: t.TempDir()}
	e := event.Event{SchemaVersion: 1, ID: event.ID("timing"), Agent: "codex", Kind: event.TurnCompleted, OccurredAt: now, SessionID: "s", TurnID: "t", ReceivedAt: now.Add(-time.Second), SignalAt: now.Add(-2 * time.Second)}
	if _, err := q.Enqueue(ctx, e, []string{"test"}); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	d := Dispatcher{Queue: q, Sender: sender, Timeout: time.Second, MaxAttempts: 2}
	if _, err := d.DeliverOnce(ctx, config.Destination{ID: "test"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.Load()
	if err != nil {
		t.Fatal(err)
	}
	first := jobs[0].Deliveries[0].FirstAttemptAt
	if first == nil || first.Before(*jobs[0].ReceivedAt) || !jobs[0].SignalAt.Equal(e.SignalAt) {
		t.Fatal(jobs)
	}
	d.Queue = store.Queue{Dir: q.Dir}
	if _, err := d.DeliverOnce(ctx, config.Destination{ID: "test"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	jobs, err = q.Load()
	if err != nil || !jobs[0].Deliveries[0].FirstAttemptAt.Equal(*first) || jobs[0].Deliveries[0].Attempts != 2 {
		t.Fatal(jobs, err)
	}
	wire, _ := json.Marshal(sender.notifications[0])
	if strings.Contains(string(wire), "received_at") || strings.Contains(string(wire), "signal_at") {
		t.Fatal("local timing leaked into webhook")
	}
}

// Opt-in measurement uses real queue files, independent source loops and HTTP.
// Hook payloads are protocol fixtures. No external service or model is called.
func TestMultiHarnessLocalLatency(t *testing.T) {
	if os.Getenv("BLIP_TEST_LATENCY") != "1" || testing.Short() {
		t.Skip("set BLIP_TEST_LATENCY=1 for the 80-event local latency measurement")
	}
	type receivedEvent struct {
		event event.Event
		at    time.Time
	}
	received := make(chan receivedEvent, 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e event.Event
		if json.NewDecoder(r.Body).Decode(&e) != nil {
			w.WriteHeader(400)
			return
		}
		received <- receivedEvent{e, time.Now()}
		w.WriteHeader(204)
	}))
	defer server.Close()
	dir := t.TempDir()
	c := config.Config{SchemaVersion: 2, Path: filepath.Join(dir, "blip.json"), StateDir: filepath.Join(dir, "state"), PollInterval: "1s", RequestTimeout: "2s", MaxAttempts: 2, Destinations: []config.Destination{{ID: "local", Kind: "webhook", Enabled: true, URL: server.URL}}}
	for _, harness := range []string{"codex", "claude-code", "hermes", "dsh"} {
		c.Sources = append(c.Sources, config.Source{ID: harness, Harness: harness, Home: filepath.Join(dir, harness), Enabled: true, EventVersion: 2})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- RunWithReady(ctx, c, log.New(io.Discard, "", 0), func() { ready <- struct{}{} }) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	starts := map[string]time.Time{}
	arrival := map[string][]time.Duration{}
	for wave := 0; wave < 20; wave++ {
		for _, s := range c.Sources {
			session := fmt.Sprintf("%s-%d", s.ID, wave)
			var payload string
			switch s.Harness {
			case "codex":
				payload = fmt.Sprintf(`{"hook_event_name":"Stop","session_id":%q,"turn_id":"t"}`, session)
			case "claude-code":
				payload = fmt.Sprintf(`{"hook_event_name":"Stop","session_id":%q,"prompt_id":"p"}`, session)
			case "hermes":
				payload = fmt.Sprintf(`{"hook_event_name":"on_session_end","session_id":%q,"extra":{"turn_id":"t","completed":true}}`, session)
			case "dsh":
				payload = fmt.Sprintf(`{"blip_dsh_version":1,"event":"turn/end","session_id":%q,"turn_id":"1","root":true,"reason":"completed","occurred_at":%q}`, session, time.Now().UTC().Format(time.RFC3339Nano))
			}
			starts[session] = time.Now()
			if err := agents.Capture(ctx, c, s.ID, strings.NewReader(payload)); err != nil {
				t.Fatal(err)
			}
		}
		for range c.Sources {
			select {
			case got := <-received:
				arrival[got.event.SourceID] = append(arrival[got.event.SourceID], got.at.Sub(starts[got.event.SessionID]))
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	local := map[string][]time.Duration{}
	for _, job := range jobs {
		if job.ReceivedAt == nil || job.Deliveries[0].FirstAttemptAt == nil {
			t.Fatal("timing metadata missing", job.Event.ID)
		}
		local[job.Event.SourceID] = append(local[job.Event.SourceID], job.Deliveries[0].FirstAttemptAt.Sub(*job.ReceivedAt))
	}
	for _, s := range c.Sources {
		times := local[s.ID]
		arrivals := arrival[s.ID]
		if len(times) != 20 || len(arrivals) != 20 {
			t.Fatal("lost or duplicate event", s.ID, len(times), len(arrivals))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		sort.Slice(arrivals, func(i, j int) bool { return arrivals[i] < arrivals[j] })
		t.Logf("%s samples=20 receive_to_attempt_p95=%s local_http_arrival_p95=%s", s.ID, times[18], arrivals[18])
		if times[18] > 2*time.Second || arrivals[18] > 2*time.Second {
			t.Errorf("%s exceeded the local 2s target", s.ID)
		}
	}
}
