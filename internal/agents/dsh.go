package agents

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

//go:embed dsh_plugin.mjs
var dshPlugin []byte

type dshAdapter struct {
	config config.Config
	source config.Source
	ledger handoffs
}

func newDSH(c config.Config, s config.Source) Adapter {
	return &dshAdapter{config: c, source: s, ledger: handoffs{config: c, source: s}}
}

type dshHook struct {
	Version     int              `json:"blip_dsh_version"`
	Name        string           `json:"event"`
	SessionID   string           `json:"session_id"`
	TurnID      string           `json:"turn_id"`
	Root        *bool            `json:"root"`
	CWD         string           `json:"cwd"`
	OccurredAt  time.Time        `json:"occurred_at"`
	Reason      string           `json:"reason"`
	CancelCause string           `json:"cancel_cause"`
	ErrorCode   string           `json:"error_code"`
	RequestID   string           `json:"request_id"`
	Kind        string           `json:"kind"`
	Questions   []event.Question `json:"questions"`
}

func decodeDSH(data []byte) (dshHook, error) {
	var h dshHook
	if json.Unmarshal(data, &h) != nil || h.Version != 1 || h.Name == "" || h.SessionID == "" || h.TurnID == "" || h.Root == nil {
		return h, fmt.Errorf("invalid DSH native bridge envelope")
	}
	if (h.Name == "request" || h.Name == "resolved") && h.RequestID == "" {
		return h, fmt.Errorf("DSH request requires an occurrence identity")
	}
	return h, nil
}

func dshSuppress(h dshHook) bool {
	if !*h.Root {
		return true
	}
	if h.Name != "turn/end" {
		return false
	}
	if h.Reason == "forked" || h.Reason == "interrupted" {
		return true
	}
	return h.Reason == "aborted" && (h.CancelCause == "user" || h.CancelCause == "parent" || h.CancelCause == "disposed")
}

func (a *dshAdapter) hookEvent(h dshHook, now time.Time) *event.Event {
	if dshSuppress(h) {
		return nil
	}
	if !h.OccurredAt.IsZero() {
		now = h.OccurredAt
	}
	e := event.Event{SchemaVersion: 2, Agent: "dsh", SourceID: a.source.ID, Source: "dsh/native-plugin", SessionID: h.SessionID, TurnID: h.TurnID, CWD: h.CWD, Project: harnessProject(h.CWD, "DSH"), OccurredAt: now.UTC()}
	e.SignalAt = h.OccurredAt
	switch h.Name {
	case "turn/end":
		switch h.Reason {
		case "completed":
			e.Kind = event.TurnCompleted
		case "error":
			e.Kind, e.Reason = event.AttentionRequired, "error"
		default:
			e.Kind, e.Reason = event.AttentionRequired, "stopped"
		}
		if e.Kind == event.AttentionRequired {
			e.Detail = event.LimitBytes("DSH: "+h.Reason+" "+h.ErrorCode, 250)
		}
	case "request":
		e.Kind = event.InputRequested
		if h.Kind == "approval" {
			e.Kind = event.ApprovalRequested
		}
		e.RequestID = h.RequestID
		for _, q := range h.Questions {
			if len(e.Questions) >= 20 {
				break
			}
			question := event.Question{Text: event.LimitBytes(q.Text, 2000)}
			for _, option := range q.Options {
				if len(question.Options) >= 20 {
					break
				}
				question.Options = append(question.Options, event.LimitBytes(option, 300))
			}
			e.Questions = append(e.Questions, question)
		}
	default:
		return nil
	}
	e.ID = event.SourceEventID(a.source.ID, "dsh", string(e.Kind), h.SessionID, h.TurnID, h.RequestID)
	return &e
}

func (a *dshAdapter) ParseHook(ctx context.Context, data []byte, now time.Time) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, err := decodeDSH(data)
	if err != nil {
		return nil, err
	}
	if e := a.hookEvent(h, now); e != nil {
		return []event.Event{*e}, nil
	}
	return nil, nil
}

func (a *dshAdapter) ConsumeHook(ctx context.Context, data []byte, now time.Time, emit func(event.Event) error) error {
	h, err := decodeDSH(data)
	if err != nil {
		return err
	}
	if !*h.Root {
		return nil
	}
	key := "turn:" + h.TurnID
	if dshSuppress(h) {
		return a.ledger.cancel(ctx, h.SessionID, key, h.TurnID, now)
	}
	if h.Name == "resolved" {
		return a.ledger.resolveRequest(ctx, h.SessionID, key, h.TurnID, h.RequestID, true)
	}
	e := a.hookEvent(h, now)
	if e == nil {
		return nil
	}
	if h.Name == "request" {
		return a.ledger.request(ctx, *e, key, emit)
	}
	return a.ledger.finish(ctx, *e, key, emit)
}

func (a *dshAdapter) installer() dshInstaller {
	return dshInstaller{config: a.config, source: a.source, path: filepath.Join(a.source.Home, "cordis.patch.yml")}
}
func (a *dshAdapter) Install(ctx context.Context, executable string) error {
	return a.installer().edit(ctx, executable, true)
}
func (a *dshAdapter) Uninstall(ctx context.Context) error { return a.installer().edit(ctx, "", false) }
func (a *dshAdapter) InstallationStatus() string          { return a.installer().status() }
func (a *dshAdapter) Capabilities() []Capability {
	return []Capability{
		{Event: "turn_completed", Support: "native", Evidence: "session/event turn/end completed with native turn", Validation: "DSH 0.2.1-rc.2 runtime seam; full desktop model run unverified"},
		{Event: "attention_required", Support: "native", Evidence: "turn/end error, blocked, max-tokens; known user/parent/disposal cancels excluded", Validation: "native protocol fixtures"},
		{Event: "input_requested", Support: "native", Evidence: "user-questions/request waterfall, before human answer", Validation: "timed questions keep pending coverage"},
		{Event: "approval_requested", Support: "native", Evidence: "actual user-questions/request with approval intent", Validation: "only approvals routed through this native seam"},
	}
}
