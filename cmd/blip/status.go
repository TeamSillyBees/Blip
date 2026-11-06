package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/TeamSillyBees/Blip/internal/worker"
)

type destinationStatus struct {
	ID          string `json:"id"`
	Enabled     bool   `json:"enabled"`
	Pending     int    `json:"pending"`
	Delivered   int    `json:"delivered"`
	Failed      int    `json:"failed"`
	LastError   string `json:"last_error,omitempty"`
	lastErrorAt time.Time
}

func printStatus(c config.Config, output io.Writer) error {
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	queue := store.Queue{Dir: c.StateDir}
	jobs, err := queue.Load()
	if err != nil {
		return err
	}
	quarantined, err := queue.QuarantineCount()
	if err != nil {
		return err
	}
	byID := map[string]*destinationStatus{}
	for _, destination := range c.Destinations {
		byID[destination.ID] = &destinationStatus{ID: destination.ID, Enabled: destination.Enabled}
	}
	pending, delivered, failed := 0, 0, 0
	for _, job := range jobs {
		for _, receipt := range job.Deliveries {
			destination := byID[receipt.DestinationID]
			if destination == nil {
				destination = &destinationStatus{ID: receipt.DestinationID}
				byID[receipt.DestinationID] = destination
			}
			if receipt.DeliveredAt != nil {
				delivered++
				destination.Delivered++
			} else if receipt.Failed {
				failed++
				destination.Failed++
			} else {
				pending++
				destination.Pending++
			}
			errorAt := receiptErrorTime(job, receipt)
			if receipt.LastError != "" && errorAt.After(destination.lastErrorAt) {
				destination.LastError = receipt.LastError
				destination.lastErrorAt = errorAt
			}
		}
	}
	destinations := []*destinationStatus{}
	for _, destination := range byID {
		destinations = append(destinations, destination)
	}
	sort.Slice(destinations, func(i, j int) bool { return destinations[i].ID < destinations[j].ID })
	var state worker.Status
	if running {
		_ = store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &state)
	}
	return json.NewEncoder(output).Encode(map[string]any{"running": running, "ready": running && state.Ready, "pending": pending, "delivered": delivered, "failed": failed, "quarantined": quarantined, "state_dir": c.StateDir, "destinations": destinations, "collector_error": state.CollectorError})
}

func receiptErrorTime(job store.Job, receipt store.Delivery) time.Time {
	if receipt.LastAttemptAt != nil {
		return *receipt.LastAttemptAt
	}
	// Older receipts predate explicit attempt times. Their scheduled retry time
	// was written as the failed attempt time plus the deterministic backoff.
	if receipt.Attempts > 0 && !receipt.NextAttempt.IsZero() {
		return receipt.NextAttempt.Add(-worker.Backoff(receipt.Attempts))
	}
	if receipt.FailedAt != nil {
		return *receipt.FailedAt
	}
	return job.CreatedAt
}
