package worker

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestQueueWatchWakesOnPublicationFromAnotherProcess(t *testing.T) {
	const helperDir = "BLIP_TEST_PUBLICATION_DIR"
	if dir := os.Getenv(helperDir); dir != "" {
		enqueueTurn(t, store.Queue{Dir: dir}, "external", "out")
		return
	}

	q := store.Queue{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan string, 4)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		err := watchPublications(ctx, q.JobsDir(), func(name string) {
			select {
			case changes <- name:
			case <-ctx.Done():
			}
		})
		if err != nil {
			t.Error(err)
		}
	}()
	defer func() { cancel(); <-stopped }()
	wait := func(want string) {
		t.Helper()
		select {
		case name := <-changes:
			if name != want {
				t.Fatalf("wake for %q, want %q", name, want)
			}
		case <-stopped:
			t.Fatal("queue watch stopped")
		case <-time.After(5 * time.Second):
			t.Fatal("queue watch missed a publication")
		}
	}
	wait("") // Wait until the directory watch is registered.
	select {
	case <-changes:
		t.Fatal("idle queue produced a wake")
	case <-time.After(600 * time.Millisecond):
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestQueueWatchWakesOnPublicationFromAnotherProcess$")
	cmd.Env = append(os.Environ(), helperDir+"="+q.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("enqueue process: %v: %s", err, out)
	}
	wait(event.ID("external") + ".json")
	if _, err := q.ReadJob(event.ID("external")); err != nil {
		t.Fatalf("published job unreadable: %v", err)
	}
}
