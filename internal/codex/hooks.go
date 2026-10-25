// Package codex translates versioned Codex hooks and rollout records into events.
package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
)

const MaxHookBytes = 1024 * 1024

type Hook struct {
	Name           string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TurnID         string          `json:"turn_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	Input          json.RawMessage `json:"tool_input"`
}

var inputTool = regexp.MustCompile(`(^|[./_:])request_user_input(_async)?$`)

// ParseHook ignores unrelated tool calls and PostToolUse; asynchronous tool return is not an answer.
func ParseHook(reader io.Reader, now time.Time) (*event.Event, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxHookBytes+1))
	if err != nil || len(data) > MaxHookBytes {
		return nil, fmt.Errorf("hook payload is unreadable or larger than 1 MiB")
	}
	var hook Hook
	if json.Unmarshal(data, &hook) != nil {
		return nil, fmt.Errorf("invalid hook JSON")
	}
	kind := event.ApprovalRequested
	switch hook.Name {
	case "PermissionRequest":
	case "PreToolUse":
		if !inputTool.MatchString(hook.ToolName) {
			return nil, nil
		}
		kind = event.InputRequested
	default:
		return nil, nil
	}
	if hook.SessionID == "" || hook.TurnID == "" {
		return nil, fmt.Errorf("hook is missing session_id or turn_id")
	}
	if IsSubagentTranscript(hook.TranscriptPath) {
		return nil, nil
	}
	input := map[string]any{}
	hook.Input = bytes.TrimSpace(hook.Input)
	if len(hook.Input) > 0 && !bytes.Equal(hook.Input, []byte("null")) {
		if hook.Input[0] == '"' {
			var value string
			if json.Unmarshal(hook.Input, &value) == nil {
				hook.Input = []byte(value)
			}
		}
		if json.Unmarshal(hook.Input, &input) != nil {
			return nil, fmt.Errorf("invalid hook tool_input")
		}
	}
	identity := hook.ToolUseID
	if identity == "" {
		// PermissionRequest does not promise a request ID. Coalesce identical requests for ten seconds only.
		canonical, _ := json.Marshal(input)
		identity = event.ID(hook.ToolName, string(canonical), strconv.FormatInt(now.Unix()/10, 10))
	}
	notification := &event.Event{SchemaVersion: 1, Agent: "codex", Kind: kind, Source: "codex/hooks", OccurredAt: now.UTC(), SessionID: hook.SessionID, TurnID: hook.TurnID, RequestID: hook.ToolUseID, Project: event.Project(hook.CWD), CWD: hook.CWD}
	notification.ID = event.ID("codex", hook.SessionID, hook.TurnID, string(kind), identity)
	if kind == event.InputRequested {
		notification.Detail = "Codex 发起了提问，请查看客户端。"
		if questions, ok := input["questions"].([]any); ok {
			for index, raw := range questions {
				if index >= 12 {
					break
				}
				question, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				text := stringField(question, "question", "title", "header")
				if text == "" {
					continue
				}
				q := event.Question{Text: event.LimitBytes(text, 1500)}
				if options, ok := question["options"].([]any); ok {
					for optionIndex, option := range options {
						if optionIndex >= 20 {
							break
						}
						label, _ := option.(string)
						if object, ok := option.(map[string]any); ok {
							label = stringField(object, "label", "title")
							if description := stringField(object, "description"); description != "" {
								label += "：" + description
							}
						}
						if label != "" {
							q.Options = append(q.Options, event.LimitBytes(label, 400))
						}
					}
				}
				notification.Questions = append(notification.Questions, q)
			}
		}
	} else {
		notification.Detail = "Codex 发起审批申请，请查看客户端。\n工具：" + event.LimitBytes(hook.ToolName, 200)
		for _, key := range []string{"command", "cmd", "justification", "reason", "permissions", "additional_permissions"} {
			if value, ok := input[key]; ok {
				encoded, _ := json.Marshal(value)
				notification.Detail += "\n" + key + "：" + event.LimitBytes(string(encoded), 1500)
			}
		}
		notification.Detail = event.LimitBytes(notification.Detail, 4000)
	}
	return notification, notification.Validate()
}

func stringField(object map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := object[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func IsSubagentTranscript(path string) bool {
	if path == "" {
		return false
	}
	metadata, err := readMetadata(filepath.Clean(path))
	return err == nil && metadata.IsSubagent()
}
