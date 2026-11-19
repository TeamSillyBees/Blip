package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestTestNotificationReportsActualReceiptsAndRetainsUnsentJobs(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name     string
		receipts []store.Delivery
		cancel   bool
		want     string
		wantErr  bool
	}{
		{name: "delivered", receipts: []store.Delivery{{DestinationID: "one", DeliveredAt: &now}}, want: "已送达渠道"},
		{name: "failed", receipts: []store.Delivery{{DestinationID: "one", Failed: true, LastError: "HTTP 401"}}, want: "投递失败（HTTP 401）", wantErr: true},
		{name: "retry", receipts: []store.Delivery{{DestinationID: "one", LastError: "HTTP 503", NextAttempt: now}}, want: "等待重试（HTTP 503）", wantErr: true},
		{name: "partially sent", receipts: []store.Delivery{{DestinationID: "one", DeliveredAt: &now}, {DestinationID: "two", NextAttempt: now}}, cancel: true, want: "two：仍在等待投递", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := config.Config{Path: filepath.Join(t.TempDir(), "中文 配置.json"), StateDir: t.TempDir()}
			id := event.ID(test.name)
			path := filepath.Join(c.StateDir, "jobs", id+".json")
			job := store.Job{
				Event:     event.Event{SchemaVersion: 1, ID: id, Agent: "codex", Kind: event.TurnCompleted, OccurredAt: now, SessionID: "session", TurnID: "turn"},
				CreatedAt: now, Deliveries: test.receipts,
			}
			if err := store.WriteJSON(path, job); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if test.cancel {
				cancel()
			}
			var output bytes.Buffer
			err := waitForTest(ctx, c, id, &output)
			if (err != nil) != test.wantErr || !strings.Contains(output.String(), test.want) {
				t.Fatalf("result=%q, error=%v", output.String(), err)
			}
			if err != nil && !strings.Contains(err.Error(), cliCommand(c.Path, "status")) {
				t.Fatalf("failure omitted the current configuration: %v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("waiting for a test altered or deleted queued work")
			}
		})
	}
}
