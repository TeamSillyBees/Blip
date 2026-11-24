package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type antigravityAdapter struct {
	config          config.Config
	source          config.Source
	registeredEvent string
	db              *antigravity.SummaryDB
	dbFile          os.FileInfo
	checked         map[string]antigravityCheck
}

func newAntigravity(c config.Config, s config.Source) Adapter {
	return &antigravityAdapter{config: c, source: s}
}

// Older clients omit hookEventName. Each installed command identifies its
// event independently of stdin and delegates to the same durable queue.
func CaptureAntigravity(ctx context.Context, c config.Config, id, name string, input io.Reader) error {
	return capture(ctx, c, id, input, Registry{"antigravity": func(c config.Config, s config.Source) Adapter {
		return &antigravityAdapter{config: c, source: s, registeredEvent: name}
	}})
}

type antigravityHook struct {
	Name          string   `json:"hookEventName"`
	Conversation  string   `json:"conversationId"`
	Artifacts     string   `json:"artifactDirectoryPath"`
	Transcript    string   `json:"transcriptPath"`
	CWD           string   `json:"cwd"`
	Workspaces    []string `json:"workspacePaths"`
	LastUserInput string   `json:"lastUserInput"`
	Invocation    *int     `json:"invocationNum"`
	InitialSteps  *int     `json:"initialNumSteps"`
	Execution     int      `json:"executionNum"`
	Step          *int     `json:"stepIdx"`
	FullyIdle     bool     `json:"fullyIdle"`
	Termination   string   `json:"terminationReason"`
	ToolCall      struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// Both clients load the same global hooks.json. Match the data directory
// reported by the client before reading or writing any per-conversation state.
func (a *antigravityAdapter) ownsConversation(h antigravityHook) (bool, error) {
	if h.Conversation == "" || h.Conversation == "." || h.Conversation == ".." || strings.ContainsAny(h.Conversation, "/\\:\x00") {
		return false, fmt.Errorf("invalid Antigravity conversationId")
	}
	dir := h.Artifacts
	if dir == "" && h.Transcript != "" {
		logs := filepath.Dir(h.Transcript)
		if filepath.Base(logs) == "logs" && filepath.Base(filepath.Dir(logs)) == ".system_generated" {
			dir = filepath.Dir(filepath.Dir(logs))
		}
	}
	if relative, ok := strings.CutPrefix(filepath.ToSlash(dir), "~/"); ok {
		profile, err := os.UserHomeDir()
		if err != nil {
			return false, err
		}
		dir = filepath.Join(profile, filepath.FromSlash(relative))
	}
	if !filepath.IsAbs(dir) {
		return false, fmt.Errorf("Antigravity hook has no absolute conversation directory")
	}
	match := func(home string) bool {
		rel, err := filepath.Rel(filepath.Join(home, "brain", h.Conversation), dir)
		return err == nil && rel == "."
	}
	if match(a.source.Home) {
		return true, nil
	}
	// Resolve only the configured home. Never open a path supplied on stdin.
	home, err := config.CanonicalHome(a.source.Home)
	return err == nil && match(home), err
}

func (a *antigravityAdapter) ParseHook(ctx context.Context, data []byte, now time.Time) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var h antigravityHook
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("invalid Antigravity hook JSON: %w", err)
	}
	if h.Name == "" {
		h.Name = a.registeredEvent
	}
	if h.Name == "" || (a.registeredEvent != "" && h.Name != a.registeredEvent) {
		return nil, fmt.Errorf("unexpected Antigravity hook event")
	}
	owned, err := a.ownsConversation(h)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, errForeignHook
	}
	if h.CWD == "" && len(h.Workspaces) > 0 {
		h.CWD = h.Workspaces[0]
	}
	// The numeric invocation marker is also read from installations predating
	// conversation watches. Keep its format and handoff identity stable.
	marker := filepath.Join(StateDir(a.config, a.source), "invocations", event.ID(h.Conversation)+".json")
	switch h.Name {
	case "PreInvocation":
		if h.Invocation != nil && *h.Invocation == 0 && h.InitialSteps != nil && *h.InitialSteps >= 0 {
			if err := store.WriteJSON(marker, *h.InitialSteps); err != nil {
				return nil, err
			}
			return nil, a.watch(h, *h.InitialSteps, now)
		}
	case "PreToolUse":
		// This can be the first callback when attaching in the middle of a turn.
		// A tool about to run does not prove that its caller is waiting for a human.
		return nil, a.watchTool(h, marker, now)
	case "Stop":
		if !h.FullyIdle {
			return nil, nil
		}
		e := event.Event{SchemaVersion: 2, Agent: "antigravity", Source: "antigravity/hooks", SourceID: a.source.ID,
			SessionID: h.Conversation, Project: harnessProject(h.CWD, "Antigravity"), CWD: h.CWD, OccurredAt: now.UTC()}
		return a.stopEvent(ctx, h, e, marker)
	}
	return nil, nil
}

func (a *antigravityAdapter) stopEvent(ctx context.Context, h antigravityHook, e event.Event, marker string) ([]event.Event, error) {
	switch strings.TrimPrefix(strings.ToUpper(h.Termination), "EXECUTOR_TERMINATION_REASON_") {
	case "NO_TOOL_CALL", "MODEL_STOP":
		e.Kind = event.TurnCompleted
	case "ERROR":
		e.Kind, e.Reason = event.AttentionRequired, "error"
	case "MAX_STEPS_EXCEEDED", "MAX_INVOCATIONS", "MAX_FORCED_INVOCATIONS", "MAX_TOKEN_BUDGET_EXCEEDED", "TERMINAL_STEP_TYPE", "TERMINAL_CUSTOM_HOOK":
		e.Kind, e.Reason = event.AttentionRequired, "stopped"
	default:
		return nil, nil
	}
	// Stop describes the execution loop of *this* conversation, including for
	// workers several levels below the conversation the user opened.
	root := a.stopRoot(ctx, h.Conversation)
	if root.id == "" {
		return nil, nil
	}
	e.SessionTitle = root.title
	var steps *int
	err := store.ReadJSON(marker, &steps)
	firstStep := 0
	turn := "execution:" + strconv.Itoa(h.Execution) // attached after the first invocation
	if err == nil && steps != nil && *steps >= 0 {
		firstStep = *steps
		turn = "steps:" + strconv.Itoa(*steps)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("invalid Antigravity invocation marker")
	}
	e.ID = event.SourceEventID(e.SourceID, e.Agent, "handoff", h.Conversation, turn, event.ID(h.LastUserInput))
	if e.Kind == event.TurnCompleted {
		e.Detail = antigravityFinalReply(ctx, a.source.Home, h.Conversation, firstStep)
	}
	return []event.Event{e}, nil
}

func (*antigravityAdapter) Capabilities() []Capability {
	const validation = "Google Antigravity hook contract; agy CLI 1.5.9; Antigravity App 2.34.0 conversation hierarchy"
	return []Capability{
		{Event: "turn_completed", Support: "derived", Evidence: "fully idle normal Stop of the user’s root conversation", Validation: validation},
		{Event: "input_requested", Support: "derived", Evidence: "a waiting step requests user input or blocking review, attributed to its root conversation", Validation: validation},
		{Event: "approval_requested", Support: "derived", Evidence: "a waiting step requests user authorization, attributed to its root conversation", Validation: validation},
		{Event: "attention_required", Support: "derived", Evidence: "fully idle root Stop with an error, limit or terminal reason", Validation: validation},
	}
}

func (a *antigravityAdapter) ReadPaths() []ReadPath {
	path := filepath.Join(a.source.Home, antigravity.SummaryFilename)
	return []ReadPath{{Path: path, Optional: true}, {Path: path + "-wal", Optional: true}, {Path: path + "-shm", Optional: true}}
}
