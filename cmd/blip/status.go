package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
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

type statusReport struct {
	Running        bool                  `json:"running"`
	Ready          bool                  `json:"ready"`
	Degraded       bool                  `json:"degraded"`
	Sources        []agents.SourceStatus `json:"sources"`
	Pending        int                   `json:"pending"`
	Delivered      int                   `json:"delivered"`
	Failed         int                   `json:"failed"`
	Quarantined    int                   `json:"quarantined"`
	StateDir       string                `json:"state_dir"`
	Destinations   []*destinationStatus  `json:"destinations"`
	CollectorError string                `json:"collector_error"`
	Latency        map[string]any        `json:"latency"`
}

func printStatus(c config.Config, output io.Writer) error {
	report, err := readStatus(c)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

func printStatusText(c config.Config, output io.Writer) error {
	report, err := readStatus(c)
	if err != nil {
		return err
	}
	state := "已停止"
	if report.Running {
		state = "正在初始化"
		if report.Ready {
			state = "通知进程就绪"
		}
		if report.Degraded {
			state += "，部分来源受限"
		}
	}
	fmt.Fprintln(output, "Blip：", state)
	fmt.Fprintln(output, "配置：", c.Path)
	fmt.Fprintf(output, "通知：待发送 %d · 已送达 %d · 失败 %d\n", report.Pending, report.Delivered, report.Failed)
	for _, destination := range report.Destinations {
		enabled := "已停用"
		if destination.Enabled {
			enabled = "已启用"
		}
		fmt.Fprintf(output, "  %q（%s）：待发送 %d · 已送达 %d · 失败 %d\n", destination.ID, enabled, destination.Pending, destination.Delivered, destination.Failed)
		if destination.LastError != "" {
			fmt.Fprintf(output, "    最近错误：%s\n", destination.LastError)
		}
	}
	for _, source := range report.Sources {
		sourceState := map[string]string{"ready": "就绪", "not_running": "未运行", "disabled": "已停用", "checking": "检查中", "degraded": "受限", "paused": "已暂停", "unavailable": "不可用"}[source.State]
		if sourceState == "" {
			sourceState = source.State
		}
		fmt.Fprintf(output, "来源 %s（%s）：%s\n", source.ID, source.Harness, sourceState)
		if source.ErrorCode != "" {
			fmt.Fprintln(output, "  错误：", source.ErrorCode)
		}
		// A LocalService worker cannot read the user's hook settings. Inspect
		// installation from this CLI's account before suggesting a repair.
		installation := source.Installation
		if configured, ok := c.Source(source.ID); ok {
			installation = agents.Inspect(c, configured).Installation
		}
		if source.Enabled && (installation == "not_installed" || installation == "missing_configuration") {
			fmt.Fprintln(output, "  安装接入：", cliCommand(c.Path, "install", source.Harness, "--source", source.ID))
		} else if source.Enabled && installation != "configured_unverified" && installation != "not_required" {
			fmt.Fprintln(output, "  接入状态：", installation)
			fmt.Fprintln(output, "  检查接入：", cliCommand(c.Path, "doctor", "--source", source.ID))
		}
	}
	if report.Quarantined > 0 {
		fmt.Fprintf(output, "已隔离 %d 组损坏记录：%s\n", report.Quarantined, filepath.Join(c.StateDir, "quarantine", "records"))
	}
	if len(c.Enabled()) == 0 {
		fmt.Fprintln(output, "配置通知渠道：", cliCommand(c.Path, "configure"))
	} else if !report.Running {
		fmt.Fprintln(output, "启动通知：", cliCommand(c.Path, "start"))
	}
	if report.Degraded {
		fmt.Fprintln(output, "检查接入：", cliCommand(c.Path, "doctor"))
	}
	if report.Failed > 0 {
		fmt.Fprintln(output, "检查通知渠道：", cliCommand(c.Path, "configure"))
	}
	return nil
}

func readStatus(c config.Config) (statusReport, error) {
	running, err := isRunning(c)
	if err != nil {
		return statusReport{}, err
	}
	queue := store.Queue{Dir: c.StateDir}
	jobs, err := queue.Load()
	if err != nil {
		return statusReport{}, err
	}
	quarantined, err := queue.QuarantineCount()
	if err != nil {
		return statusReport{}, err
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
	} else {
		state.Sources = []agents.SourceStatus{}
		for _, source := range c.EffectiveSources() {
			state.Sources = append(state.Sources, agents.Inspect(c, source))
		}
		for _, source := range state.Sources {
			if source.Enabled && source.ErrorCode != "" {
				state.Degraded = true
			}
		}
	}
	return statusReport{Running: running, Ready: running && state.Ready, Degraded: state.Degraded, Sources: state.Sources, Pending: pending, Delivered: delivered, Failed: failed, Quarantined: quarantined, StateDir: c.StateDir, Destinations: destinations, CollectorError: state.CollectorError, Latency: map[string]any{"scope": "retained_queue", "by_source_destination": summarizeLatency(jobs)}}, nil
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
