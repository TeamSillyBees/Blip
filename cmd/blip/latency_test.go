package main

import (
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestLatencySummaryPreservesBacklogAndOmitsMissingHistory(t *testing.T) {
	now := time.Now()
	jobs := []store.Job{}
	for i := 1; i <= 20; i++ {
		first := now.Add(time.Duration(i) * time.Millisecond)
		signal := now.Add(-3 * time.Millisecond)
		jobs = append(jobs, store.Job{Event: event.Event{SchemaVersion: 2, SourceID: "dsh-work", Agent: "dsh"}, ReceivedAt: &now, SignalAt: &signal, Deliveries: []store.Delivery{{DestinationID: "a", FirstAttemptAt: &first}, {DestinationID: "b"}}})
	}
	// An offline sample stays in the distribution; old receipts without local
	// timings cannot make the apparent latency artificially small.
	late := now.Add(time.Hour)
	jobs[19].Deliveries[0].FirstAttemptAt = &late
	jobs = append(jobs, store.Job{Event: event.Event{SchemaVersion: 1, Agent: "codex"}, Deliveries: []store.Delivery{{DestinationID: "old", FirstAttemptAt: &late}}})
	metrics := summarizeLatency(jobs)
	if len(metrics) != 2 || metrics[0].Samples != 20 || *metrics[0].ReceiveToAttemptP95MS != 19 || *metrics[0].SignalToReceiveP95MS != 3 || metrics[1].Samples != 0 || metrics[1].ReceiveToAttemptP95MS != nil {
		t.Fatal(metrics)
	}
	before := now.Add(-time.Second)
	jobs[0].Deliveries[0].FirstAttemptAt = &before
	metrics = summarizeLatency(jobs)
	if metrics[0].SkippedClockSamples != 1 || metrics[0].Samples != 19 {
		t.Fatal(metrics)
	}
}
