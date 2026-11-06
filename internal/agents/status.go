package agents

import (
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type SourceStatus struct {
	ID             string       `json:"id"`
	Harness        string       `json:"harness"`
	Enabled        bool         `json:"enabled"`
	EventVersion   int          `json:"event_version"`
	State          string       `json:"state"`
	ErrorCode      string       `json:"error_code,omitempty"`
	Installation   string       `json:"installation"`
	Version        string       `json:"version"`
	Trust          string       `json:"trust"`
	Capabilities   []Capability `json:"capabilities"`
	LastReceivedAt *time.Time   `json:"last_received_at,omitempty"`
	LastEventAt    *time.Time   `json:"last_event_at,omitempty"`
	LastHookError  string       `json:"last_hook_error,omitempty"`
}

func Inspect(c config.Config, s config.Source) SourceStatus {
	status := SourceStatus{ID: s.ID, Harness: s.Harness, Enabled: s.Enabled, EventVersion: s.EventVersion, State: "not_running", Installation: "unavailable", Version: "unknown", Trust: "unknown", Capabilities: []Capability{}}
	if status.EventVersion == 0 {
		status.EventVersion = 2
	}
	if !s.Enabled {
		status.State = "disabled"
	}
	adapter, err := Builtins().Open(c, s)
	if err != nil {
		if s.Enabled {
			status.State = "unavailable"
			status.ErrorCode = "adapter_unavailable"
		}
		return status
	}
	status.Capabilities = adapter.Capabilities()
	if err := CheckBinding(c, s); s.Enabled && err != nil {
		status.State = "paused"
		status.ErrorCode = "source_binding"
	}
	if installer, ok := adapter.(Installer); ok {
		status.Installation = installer.InstallationStatus()
	}
	RefreshHookDiagnostic(c, &status)
	ApplyHookHealth(&status)
	return status
}

func RefreshHookDiagnostic(c config.Config, s *SourceStatus) {
	var diagnostic HookDiagnostic
	if store.ReadJSON(DiagnosticPath(c, s.ID), &diagnostic) == nil && !diagnostic.ReceivedAt.IsZero() {
		s.LastReceivedAt = &diagnostic.ReceivedAt
		s.LastHookError = diagnostic.ErrorCode
		if diagnostic.LastEventAt != nil && (s.LastEventAt == nil || diagnostic.LastEventAt.After(*s.LastEventAt)) {
			s.LastEventAt = diagnostic.LastEventAt
		}
	}
	if hasCheckpointFault(c, s.ID) {
		s.LastHookError = "checkpoint_invalid"
	} else if s.LastHookError == "checkpoint_invalid" {
		// The exact checkpoint was revalidated after repair.
		s.LastHookError = ""
	}
}

func ApplyHookHealth(s *SourceStatus) {
	if !s.Enabled || s.ErrorCode != "" || s.LastHookError == "" {
		return
	}
	s.State, s.ErrorCode = "degraded", s.LastHookError
	if s.LastHookError == "checkpoint_invalid" {
		s.State = "paused"
	}
}
