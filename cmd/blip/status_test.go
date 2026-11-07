package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/TeamSillyBees/Blip/internal/worker"
)

func TestStatusShowsDeliveryFailureWithoutCredentialsOrStaleReadiness(t *testing.T) {
	c := config.Config{StateDir: t.TempDir(), Destinations: []config.Destination{{ID: "bad", Kind: "webhook", Enabled: true, URL: "https://example.invalid/secret-url", Token: "secret-token"}, {ID: "paused", Kind: "gotify", Enabled: false}}}
	q := store.Queue{Dir: c.StateDir}
	e := event.Event{SchemaVersion: 1, ID: event.ID("status"), Agent: "codex", Kind: event.InputRequested, OccurredAt: time.Now().UTC(), SessionID: "s", TurnID: "t"}
	if _, err := q.Enqueue(context.Background(), e, []string{"bad", "paused"}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateDelivery(context.Background(), e.ID, "bad", func(d *store.Delivery) { d.Attempts = 1; d.Failed = true; d.LastError = "notification HTTP status 401" }); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), worker.Status{PID: 999, Ready: true}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := printStatus(c, &output); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Running, Ready  bool
		Pending, Failed int
		Destinations    []destinationStatus
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Running || report.Ready || report.Pending != 1 || report.Failed != 1 {
		t.Fatalf("incorrect worker state: %s", output.String())
	}
	if len(report.Destinations) != 2 || report.Destinations[0].LastError != "notification HTTP status 401" || report.Destinations[1].Enabled {
		t.Fatalf("missing destination diagnosis: %s", output.String())
	}
	if strings.Contains(output.String(), "secret-") {
		t.Fatal("status exposed destination credentials")
	}
	output.Reset()
	if err := printStatusText(c, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "已停止") || !strings.Contains(output.String(), "HTTP status 401") || strings.Contains(output.String(), "secret-") {
		t.Fatalf("human status omitted diagnosis or exposed credentials: %s", output.String())
	}
}

func TestHumanStatusChecksHooksAsCurrentUserInsteadOfServiceSnapshot(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{SchemaVersion: 1, Path: filepath.Join(dir, "config.json"), CodexHome: filepath.Join(dir, "codex"), StateDir: filepath.Join(dir, "state"), Destinations: []config.Destination{}}
	if err := store.WriteJSON(c.Path, c); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), c.Path, []string{"install", "codex"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	lock, available, err := (store.Queue{Dir: c.StateDir}).WorkerLock()
	if err != nil || !available {
		t.Fatalf("worker lock: %v", err)
	}
	defer lock.Close()
	state := worker.Status{Ready: true, Sources: []agents.SourceStatus{{ID: config.DefaultSourceID, Harness: "codex", Enabled: true, State: "ready", Installation: "unreadable_configuration"}}}
	if err := store.WriteJSON(filepath.Join(c.StateDir, "worker.json"), state); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := printStatusText(c, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "安装接入") || strings.Contains(output.String(), "unreadable_configuration") {
		t.Fatalf("human status used the service's restricted hook visibility: %s", output.String())
	}
}

func TestStatusReportsQuarantineAndStillCountsHealthyJobs(t *testing.T) {
	c := config.Config{StateDir: t.TempDir()}
	q := store.Queue{Dir: c.StateDir}
	e := event.Event{SchemaVersion: 1, ID: event.ID("healthy"), Agent: "codex", Kind: event.InputRequested, OccurredAt: time.Now().UTC(), SessionID: "s", TurnID: "t"}
	if _, err := q.Enqueue(context.Background(), e, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.StateDir, "jobs", event.ID("broken")+".json"), []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := printStatus(c, &output); err != nil {
		t.Fatal(err)
	}
	var report struct{ Pending, Quarantined int }
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Pending != 1 || report.Quarantined != 1 {
		t.Fatalf("quarantine hid healthy deliveries: %s", output.String())
	}
}

func TestStatusChoosesLatestAttemptAcrossJobCreationOrder(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current receipts", true: "legacy receipts"}[legacy], func(t *testing.T) {
			c := config.Config{StateDir: t.TempDir()}
			q := store.Queue{Dir: c.StateDir}
			now := time.Now().UTC()
			for i, message := range []string{"newest attempt: HTTP 401", "older attempt: HTTP 503"} {
				createdAt := now.Add(time.Duration(i-2) * time.Hour)
				attemptAt := now.Add(-time.Duration(i) * time.Minute)
				e := event.Event{SchemaVersion: 1, ID: event.ID(message), Agent: "codex", Kind: event.InputRequested, OccurredAt: createdAt, SessionID: "s", TurnID: message}
				receipt := store.Delivery{DestinationID: "one", Attempts: 2, NextAttempt: attemptAt.Add(worker.Backoff(2)), LastError: message}
				if !legacy {
					receipt.LastAttemptAt = &attemptAt
				}
				if err := q.Save(store.Job{Event: e, CreatedAt: createdAt, Deliveries: []store.Delivery{receipt}}); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := printStatus(c, &output); err != nil {
				t.Fatal(err)
			}
			var report struct{ Destinations []destinationStatus }
			if err := json.Unmarshal(output.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Destinations) != 1 || report.Destinations[0].LastError != "newest attempt: HTTP 401" {
				t.Fatalf("status selected an older error: %s", output.String())
			}
		})
	}
}
