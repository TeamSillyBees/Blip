package worker

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestCorruptSourceDoesNotBlockHealthyCollectionOrQueuedDelivery(t *testing.T) {
	root := t.TempDir()
	received := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received <- struct{}{}; w.WriteHeader(204) }))
	defer server.Close()
	c := config.Config{SchemaVersion: 2, Path: filepath.Join(root, "config.json"), StateDir: filepath.Join(root, "state"), PollInterval: "100ms", RequestTimeout: "1s", MaxAttempts: 2, Sources: []config.Source{{ID: "broken", Harness: "codex", Enabled: true, EventVersion: 2, Home: filepath.Join(root, "broken")}, {ID: "healthy", Harness: "codex", Enabled: true, EventVersion: 2, Home: filepath.Join(root, "healthy")}, {ID: "future", Harness: "future", Enabled: true, EventVersion: 2, Home: filepath.Join(root, "future")}}, Destinations: []config.Destination{{ID: "test", Kind: "webhook", Enabled: true, URL: server.URL}}}
	broken := filepath.Join(agents.StateDir(c, c.Sources[0]), "rollout-cursors.json")
	if err := store.WriteFile(broken, []byte("broken-checkpoint")); err != nil {
		t.Fatal(err)
	}
	queued := event.Event{SchemaVersion: 2, ID: event.SourceEventID("removed", "deleted-adapter", "handoff", "s", "t"), Agent: "deleted-adapter", SourceID: "removed", Source: "deleted-adapter/hooks", Kind: event.TurnCompleted, OccurredAt: time.Now(), SessionID: "s"}
	if _, err := (store.Queue{Dir: c.StateDir}).Enqueue(context.Background(), queued, []string{"test"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- RunWithReady(ctx, c, log.New(io.Discard, "", 0), func() { ready <- struct{}{} }) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not become ready")
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("removed adapter's queue failed to drain")
	}
	var status Status
	if err := store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status); err != nil || !status.Degraded || status.Sources[0].State != "paused" || status.Sources[1].State != "ready" || status.Sources[2].State != "unavailable" {
		t.Fatalf("wrong source status: %+v %v", status, err)
	}
	line := `{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}` + "\n"
	file := filepath.Join(c.Sources[1].Home, "sessions", "rollout-new.jsonl")
	if err := store.WriteFile(file, []byte(`{"type":"session_meta","payload":{"id":"root","source":"cli","cwd":"/project"}}`+"\n"+line)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy collector was blocked")
	}
	data, _ := os.ReadFile(broken)
	if string(data) != "broken-checkpoint" {
		t.Fatal("broken checkpoint was rewritten")
	}
}

type blockedAdapter struct{}

func (blockedAdapter) ParseHook(context.Context, []byte, time.Time) ([]event.Event, error) {
	return nil, nil
}
func (blockedAdapter) Capabilities() []agents.Capability { return nil }
func (blockedAdapter) Poll(ctx context.Context, _ func(event.Event) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSourcePollBudgetAndShutdown(t *testing.T) {
	root := t.TempDir()
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(root, "state"), PollInterval: "100ms", RequestTimeout: "1s", Sources: []config.Source{{ID: "slow", Harness: "slow", Home: filepath.Join(root, "slow"), Enabled: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runWithRegistry(ctx, c, log.New(io.Discard, "", 0), nil, agents.Registry{"slow": func(config.Config, config.Source) agents.Adapter { return blockedAdapter{} }})
	if err != nil || time.Since(start) > time.Second {
		t.Fatalf("slow collector blocked shutdown: %v", err)
	}
}
