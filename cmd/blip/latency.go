package main

import (
	"math"
	"sort"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type latencyStatus struct {
	SourceID              string   `json:"source_id"`
	Harness               string   `json:"harness"`
	DestinationID         string   `json:"destination_id"`
	Samples               int      `json:"samples"`
	ReceiveToAttemptP95MS *float64 `json:"receive_to_attempt_p95_ms,omitempty"`
	SignalSamples         int      `json:"signal_samples"`
	SignalToReceiveP95MS  *float64 `json:"signal_to_receive_p95_ms,omitempty"`
	SkippedClockSamples   int      `json:"skipped_clock_samples"`
	local, signal         []time.Duration
}

func p95MS(samples []time.Duration) *float64 {
	if len(samples) == 0 {
		return nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	value := float64(samples[int(math.Ceil(float64(len(samples))*.95))-1]) / float64(time.Millisecond)
	return &value
}

// Includes the retained queue, including offline time, channel pacing and
// backlog. Missing historical timestamps are not inferred from event times.
func summarizeLatency(jobs []store.Job) []*latencyStatus {
	byKey := map[[2]string]*latencyStatus{}
	for _, job := range jobs {
		if job.ReceivedAt == nil {
			continue
		}
		source := job.Event.SourceID
		if job.Event.SchemaVersion == 1 {
			source = config.DefaultSourceID
		}
		for _, delivery := range job.Deliveries {
			key := [2]string{source, delivery.DestinationID}
			metric := byKey[key]
			if metric == nil {
				metric = &latencyStatus{SourceID: source, Harness: job.Event.Agent, DestinationID: delivery.DestinationID}
				byKey[key] = metric
			}
			if delivery.FirstAttemptAt != nil {
				delta := delivery.FirstAttemptAt.Sub(*job.ReceivedAt)
				if delta >= 0 {
					metric.local = append(metric.local, delta)
				} else {
					metric.SkippedClockSamples++
				}
			}
			if job.SignalAt != nil {
				delta := job.ReceivedAt.Sub(*job.SignalAt)
				if delta >= 0 {
					metric.signal = append(metric.signal, delta)
				} else {
					metric.SkippedClockSamples++
				}
			}
		}
	}
	metrics := []*latencyStatus{}
	for _, m := range byKey {
		m.Samples = len(m.local)
		m.SignalSamples = len(m.signal)
		m.ReceiveToAttemptP95MS = p95MS(m.local)
		m.SignalToReceiveP95MS = p95MS(m.signal)
		metrics = append(metrics, m)
	}
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].SourceID != metrics[j].SourceID {
			return metrics[i].SourceID < metrics[j].SourceID
		}
		return metrics[i].DestinationID < metrics[j].DestinationID
	})
	return metrics
}
