// Package antigravity reads the conversation summaries shared by the Antigravity clients.
package antigravity

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
)

// Row contains the indexed metadata accompanying a serialized Summary.
type Row struct {
	ConversationID string
	Title          string
	WorkspaceURIs  string
	ModifiedAt     time.Time
	UserInputAt    time.Time
	Status         string
	NotFullyIdle   bool
}

func (s *Summary) Events(row Row, sourceID string) []event.Event {
	const (
		idle         = "CASCADE_RUN_STATUS_IDLE"
		canceling    = "CASCADE_RUN_STATUS_CANCELING"
		userCanceled = 2 // ExecutorTerminationReason.USER_CANCELED
	)
	if s.Interrupted || row.Status == canceling || row.ConversationID == "" || row.UserInputAt.IsZero() || row.ModifiedAt.IsZero() {
		return nil
	}
	cwd := workspacePath(row.WorkspaceURIs)
	project := "Antigravity"
	if strings.Trim(cwd, "/\\") != "" {
		project = event.Project(cwd)
	}
	base := event.Event{
		SchemaVersion: 2, Agent: "antigravity", Source: "antigravity/conversation-summaries", SourceID: sourceID,
		SessionID: row.ConversationID, SessionTitle: event.LimitBytes(row.Title, 512), Project: project, CWD: cwd,
		OccurredAt: row.ModifiedAt.UTC(),
	}
	// The time of the last user message identifies the current turn across
	// snapshots. It is not a native turn ID and stays out of the wire event.
	turn := row.UserInputAt.UTC().Format(time.RFC3339Nano)
	if len(s.WaitingSteps) > 0 {
		var events []event.Event
		for _, waiting := range s.WaitingSteps {
			e := waiting.request(base)
			if e == nil {
				continue
			}
			e.ID = event.SourceEventID(sourceID, e.Agent, string(e.Kind), e.SessionID, turn, s.TrajectoryId, strconv.FormatUint(uint64(waiting.StepIndex), 10), e.RequestID)
			events = append(events, *e)
		}
		return events
	}
	if row.Status != idle || row.NotFullyIdle || s.LatestExecutorTerminationReason == userCanceled {
		return nil
	}
	// ExecutorTerminationReason values from the native summary. Internal
	// continuations, initial idle snapshots and unknown reasons stay silent.
	switch s.LatestExecutorTerminationReason {
	case 4: // NO_TOOL_CALL
		base.Kind = event.TurnCompleted
	case 1: // ERROR
		base.Kind, base.Reason = event.AttentionRequired, "error"
	case 3, 6, 8, 9, 11: // invocation/token limits or an explicit terminal step/hook
		base.Kind, base.Reason = event.AttentionRequired, "stopped"
	default:
		return nil
	}
	base.ID = event.SourceEventID(sourceID, base.Agent, "handoff", base.SessionID, turn)
	return []event.Event{base}
}

func (w *WaitingStep) request(e event.Event) *event.Event {
	step := w.GetStep()
	if step.GetStatus() != 9 { // CortexStepStatus.WAITING
		return nil
	}
	call := step.GetMetadata().GetToolCall()
	e.RequestID = call.GetId()
	i := step.GetRequestedInteraction()
	switch {
	case i.GetAskQuestion() != nil:
		e.Kind = event.InputRequested
		questions := i.AskQuestion.Questions
		for _, q := range questions[:min(20, len(questions))] {
			question := event.Question{Text: event.LimitBytes(q.GetQuestion(), 2000)}
			options := q.GetOptions()
			for _, option := range options[:min(20, len(options))] {
				question.Options = append(question.Options, event.LimitBytes(option.GetText(), 300))
			}
			e.Questions = append(e.Questions, question)
		}
	case i.GetElicitation() != nil:
		e.Kind = event.InputRequested
		if message := i.Elicitation.Message; message != "" {
			e.Questions = []event.Question{{Text: event.LimitBytes(message, 2000)}}
		}
	case i.GetPermission() != nil, i.GetFilePermission() != nil, i.GetApprovalInteraction() != nil,
		i.GetRunCommand() != nil, i.GetMcp() != nil:
		e.Kind = event.ApprovalRequested
		e.Detail = event.LimitBytes(call.GetName(), 200)
	case step.GetNotifyUser().GetIsBlocking() && !step.NotifyUser.ShouldAutoProceed:
		e.Kind = event.InputRequested
		e.Questions = []event.Question{{Text: event.LimitBytes(step.NotifyUser.NotificationContent, 2000)}}
	default:
		return nil
	}
	return &e
}

func workspacePath(raw string) string {
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) != nil {
		return ""
	}
	for _, value := range uris {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "file" || u.Path == "" {
			continue
		}
		path := u.Path
		if u.Host != "" && u.Host != "localhost" {
			path = "//" + u.Host + path
		} else if len(path) >= 4 && path[0] == '/' && path[2] == ':' && path[3] == '/' {
			path = path[1:]
		}
		return filepath.FromSlash(path)
	}
	return ""
}
