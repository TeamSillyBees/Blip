package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"google.golang.org/protobuf/proto"
)

// Hooks run before the client decides whether it needs a human. Observe the
// resulting waiting steps after returning control to the client.
type antigravityWatch struct {
	Conversation string    `json:"conversation"`
	FirstStep    int       `json:"first_step"`
	Started      time.Time `json:"started"`
	CWD          string    `json:"cwd,omitempty"`
	CommandID    string    `json:"command_id,omitempty"`
	CommandStep  *int      `json:"command_step,omitempty"`
	CommandHint  string    `json:"command_hint,omitempty"`
}

// An unrelated SQLite commit must not report the same pending request again.
type antigravityCheck struct {
	version   int64
	checkedAt time.Time
	sent      map[string]bool
}

func (a *antigravityAdapter) watches() string {
	return filepath.Join(StateDir(a.config, a.source), "interaction-watches")
}

func (w antigravityWatch) name() string {
	return event.ID(w.Conversation, strconv.Itoa(w.FirstStep)) + ".json"
}

func (a *antigravityAdapter) watch(h antigravityHook, first int, now time.Time) error {
	w := antigravityWatch{Conversation: h.Conversation, FirstStep: first, Started: now.UTC(), CWD: h.CWD}
	path := filepath.Join(a.watches(), w.name())
	var args struct {
		Bypass  json.RawMessage `json:"BypassSandbox"`
		Summary string          `json:"toolSummary"`
	}
	if h.ToolCall.Name == "run_command" && json.Unmarshal(h.ToolCall.Args, &args) == nil &&
		(string(args.Bypass) == "true" || string(args.Bypass) == `"true"`) {
		w.CommandID, w.CommandStep, w.CommandHint = h.ToolCall.ID, h.Step, "申请在沙箱外运行命令。"
		if summary := strings.TrimSpace(args.Summary); summary != "" {
			w.CommandHint += "\n" + event.LimitBytes(summary, 300)
		}
	} else if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return store.WriteJSON(path, w)
}

func (a *antigravityAdapter) watchTool(h antigravityHook, marker string, now time.Time) error {
	var first int
	err := store.ReadJSON(marker, &first)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		// A numeric marker from an older installation (or a settled turn)
		// is still useful for Stop identity, but cannot arm the current turn.
		w := antigravityWatch{Conversation: h.Conversation, FirstStep: first}
		_, err = os.Stat(filepath.Join(a.watches(), w.name()))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		first = -1 // attached with a call ID but no step index
		if h.Step != nil && *h.Step >= 0 {
			first = *h.Step
		}
	}
	return a.watch(h, first, now)
}

// Once a watch has settled, remove only that execution's file. A new
// invocation may have armed the same conversation while the worker was reading.
func (a *antigravityAdapter) Poll(ctx context.Context, emit func(event.Event) error) error {
	entries, err := os.ReadDir(a.watches())
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		return a.Close()
	}
	if err != nil {
		return err
	}
	path := filepath.Join(a.source.Home, antigravity.SummaryFilename)
	info, err := os.Stat(path)
	if err != nil {
		_ = a.Close()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if a.db == nil || !os.SameFile(a.dbFile, info) {
		if err := a.Close(); err != nil {
			return err
		}
		if a.db, err = antigravity.OpenSummaryDB(ctx, path); err != nil {
			return err
		}
		a.dbFile = info
		a.checked = make(map[string]antigravityCheck)
	}
	var version int64
	if err := a.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		_ = a.Close()
		return err
	}
	var problems error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// A damaged or temporarily unavailable conversation must not starve
		// unrelated roots. Only successfully examined watches are checkpointed.
		if err := a.checkWatch(ctx, entry.Name(), version, emit); err != nil {
			problems = errors.Join(problems, err)
		}
	}
	return problems
}

func (a *antigravityAdapter) checkWatch(ctx context.Context, name string, version int64, emit func(event.Event) error) error {
	last, ok := a.checked[name]
	if ok && last.version == version && time.Since(last.checkedAt) < 24*time.Hour {
		return nil
	}
	path := filepath.Join(a.watches(), name)
	var w antigravityWatch
	if err := store.ReadJSON(path, &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if w.Conversation == "" || w.Started.IsZero() || w.FirstStep < -1 || w.name() != name {
		return errors.New("invalid Antigravity interaction watch")
	}
	events, done, err := a.waiting(ctx, w)
	if err != nil {
		return err
	}
	if done {
		delete(a.checked, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	sent := make(map[string]bool, len(events))
	for _, e := range events {
		if !last.sent[e.ID] {
			if err := emit(e); err != nil {
				return err
			}
		}
		sent[e.ID] = true
	}
	a.checked[name] = antigravityCheck{version: version, checkedAt: time.Now(), sent: sent}
	return nil
}

func (a *antigravityAdapter) Close() error {
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db, a.dbFile, a.checked = nil, nil, nil
	return err
}

func (a *antigravityAdapter) waiting(ctx context.Context, w antigravityWatch) ([]event.Event, bool, error) {
	// Read the waiting step and its ancestry in the same snapshot. Release the
	// snapshot before emitting, so delivery never holds a client database lock.
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var raw []byte
	var status string
	var stepCount, userStep int
	var busy, killed, fresh bool
	err = tx.QueryRowContext(ctx, `SELECT status, not_fully_idle, killed, step_count, last_user_input_step_index,
		coalesce(julianday(last_modified_time) >= julianday(?), 0), raw_summary
		FROM conversation_summaries WHERE conversation_id = ?`, w.Started.Format(time.RFC3339Nano), w.Conversation).
		Scan(&status, &busy, &killed, &stepCount, &userStep, &fresh, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Since(w.Started) > 24*time.Hour, nil
	}
	if err != nil {
		return nil, false, err
	}
	if killed || (w.FirstStep >= 0 && userStep > w.FirstStep) || status == "CASCADE_RUN_STATUS_CANCELING" {
		return nil, true, nil
	}
	var summary antigravity.Summary
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &summary); err != nil {
		return nil, false, err
	}
	var events []event.Event
	if !summary.Interrupted {
		var root antigravityConversation
		for _, waiting := range summary.WaitingSteps {
			idx := int(waiting.StepIndex)
			if idx < w.FirstStep || waiting.GetStep().GetStatus() != 9 { // CortexStepStatus.WAITING
				continue
			}
			e := waitingEvent(waiting, w)
			if e.Kind == "" {
				continue
			}
			if root.id == "" {
				root, err = conversationRoot(ctx, tx, w.Conversation)
				if errors.Is(err, sql.ErrNoRows) {
					return nil, false, nil
				}
				if err != nil {
					return nil, false, err
				}
			}
			e.SessionID, e.SessionTitle = root.id, root.title
			e.CWD = root.cwd()
			if root.id == w.Conversation && w.CWD != "" {
				e.CWD = w.CWD
			}
			e.Project = harnessProject(e.CWD, "Antigravity")
			e.SchemaVersion, e.Agent, e.SourceID, e.Source = 2, "antigravity", a.source.ID, "antigravity/conversation-summaries"
			e.OccurredAt = time.Now().UTC()
			key := e.RequestID
			if key == "" {
				key = "step:" + strconv.Itoa(idx)
			}
			e.ID = event.SourceEventID(e.SourceID, e.Agent, string(e.Kind), w.Conversation, key)
			events = append(events, e)
		}
	}
	// The snapshot may still describe the *previous* idle turn when the
	// PreInvocation hook arrives. Wait until the client has passed our boundary.
	done := len(events) == 0 && status == "CASCADE_RUN_STATUS_IDLE" && !busy && ((w.FirstStep >= 0 && stepCount > w.FirstStep) || (w.FirstStep < 0 && fresh))
	return events, done, nil
}

func waitingEvent(waiting *antigravity.WaitingStep, watch antigravityWatch) event.Event {
	step := waiting.GetStep()
	call := step.GetMetadata().GetToolCall()
	e := event.Event{RequestID: call.GetId()}
	i := step.GetRequestedInteraction()
	switch {
	case i.GetAskQuestion() != nil:
		e.Kind = event.InputRequested
		questions := i.AskQuestion.Questions
		for _, q := range questions[:min(20, len(questions))] {
			item := event.Question{Text: event.LimitBytes(q.GetQuestion(), 2000)}
			options := q.GetOptions()
			for _, o := range options[:min(20, len(options))] {
				item.Options = append(item.Options, event.LimitBytes(o.GetText(), 300))
			}
			e.Questions = append(e.Questions, item)
		}
	case i.GetElicitation() != nil:
		e.Kind = event.InputRequested
		e.Questions = []event.Question{{Text: event.LimitBytes(i.Elicitation.Message, 2000)}}
	case i.GetPermission() != nil, i.GetFilePermission() != nil, i.GetApprovalInteraction() != nil, i.GetRunCommand() != nil, i.GetMcp() != nil:
		e.Kind, e.Detail = event.ApprovalRequested, event.LimitBytes(call.GetName(), 200)
		if call.GetName() == "run_command" && watch.CommandHint != "" &&
			((e.RequestID != "" && watch.CommandID == e.RequestID) || (watch.CommandID == "" && watch.CommandStep != nil && *watch.CommandStep == int(waiting.StepIndex))) {
			e.Detail = watch.CommandHint
		}
	case step.GetNotifyUser().GetIsBlocking() && !step.NotifyUser.ShouldAutoProceed:
		e.Kind = event.InputRequested
		e.Questions = []event.Question{{Text: event.LimitBytes(step.NotifyUser.NotificationContent, 2000)}}
	}
	return e
}
