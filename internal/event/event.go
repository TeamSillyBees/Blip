// Package event defines the notification contract shared by collectors and senders.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Kind string

const (
	TurnCompleted     Kind = "turn_completed"
	ApprovalRequested Kind = "approval_requested"
	InputRequested    Kind = "input_requested"
	AttentionRequired Kind = "attention_required"
)

type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// Event is versioned independently of Codex's input formats.
type Event struct {
	SchemaVersion int        `json:"schema_version"`
	ID            string     `json:"id"`
	Agent         string     `json:"agent"`
	SourceID      string     `json:"source_id,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	DeviceName    string     `json:"device_name,omitempty"`
	Kind          Kind       `json:"kind"`
	Source        string     `json:"source"`
	OccurredAt    time.Time  `json:"occurred_at"`
	SessionID     string     `json:"session_id"`
	SessionTitle  string     `json:"session_title,omitempty"`
	TurnID        string     `json:"turn_id,omitempty"`
	RequestID     string     `json:"request_id,omitempty"`
	Project       string     `json:"project"`
	CWD           string     `json:"cwd,omitempty"`
	Detail        string     `json:"detail,omitempty"`
	Questions     []Question `json:"questions,omitempty"`
}

func ID(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

// SourceEventID uses an unambiguous, versioned encoding. opportunity is the
// adapter's stable handoff/request identity, not the final notification kind.
func SourceEventID(sourceID, harness, opportunity string, identity ...string) string {
	parts := append([]string{"blip-event-v2", sourceID, harness, opportunity}, identity...)
	data, _ := json.Marshal(parts)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func Project(cwd string) string {
	cwd = strings.TrimRight(strings.ReplaceAll(cwd, "\\", "/"), "/")
	if index := strings.LastIndex(cwd, "/"); index >= 0 {
		cwd = cwd[index+1:]
	}
	if cwd == "" {
		return "Codex"
	}
	return cwd
}

func (e Event) Validate() error {
	if len(e.ID) != 64 || e.SessionID == "" || e.OccurredAt.IsZero() {
		return fmt.Errorf("invalid notification identity")
	}
	switch e.SchemaVersion {
	case 1:
		if e.Agent != "codex" || e.TurnID == "" || e.SourceID != "" || e.Reason != "" {
			return fmt.Errorf("invalid v1 notification identity")
		}
	case 2:
		if !identifier.MatchString(e.Agent) || !identifier.MatchString(e.SourceID) || e.Source == "" {
			return fmt.Errorf("invalid v2 notification identity")
		}
	default:
		return fmt.Errorf("unsupported notification schema_version")
	}
	if _, err := hex.DecodeString(e.ID); err != nil {
		return fmt.Errorf("invalid notification ID")
	}
	switch e.Kind {
	case TurnCompleted, ApprovalRequested, InputRequested:
		if e.Reason != "" {
			return fmt.Errorf("reason is only valid for attention_required")
		}
		return nil
	case AttentionRequired:
		if e.SchemaVersion == 2 && (e.Reason == "stopped" || e.Reason == "error" || e.Reason == "stop_signal") {
			return nil
		}
		return fmt.Errorf("invalid attention reason or schema_version")
	default:
		return fmt.Errorf("unsupported notification kind")
	}
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func (e Event) AgentName() string {
	switch e.Agent {
	case "", "codex":
		return "Codex"
	case "claude-code":
		return "Claude Code"
	case "hermes":
		return "Hermes"
	case "dsh":
		return "DSH"
	case "workbuddy":
		return "WorkBuddy"
	default:
		return e.Agent
	}
}

// LimitBytes never splits a UTF-8 code point, including when adding an ellipsis.
func LimitBytes(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	if limit < 3 {
		return ""
	}
	end := limit - 3
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func (e Event) Title() string {
	label := map[Kind]string{TurnCompleted: "完成", ApprovalRequested: "需要输入", InputRequested: "需要输入"}[e.Kind]
	if e.SchemaVersion == 2 {
		label = map[Kind]string{TurnCompleted: "本轮已结束", ApprovalRequested: "需要确认", InputRequested: "需要回复", AttentionRequired: "已停下，请查看"}[e.Kind]
		if e.Kind == AttentionRequired && e.Reason == "error" {
			label = "运行遇到问题"
		}
		if e.Kind == AttentionRequired && e.Reason == "stop_signal" {
			label = "收到停止信号，请查看"
		}
	}
	title := label + " · " + LimitBytes(e.Project, 120)
	if e.DeviceName != "" {
		title += " · " + LimitBytes(e.DeviceName, 128)
	}
	return title
}

// Body contains the conversation title, local event time, and information needed
// for a response. Full event metadata remains available in the webhook payload.
func (e Event) Body() string {
	var text strings.Builder
	title := strings.TrimSpace(e.SessionTitle)
	if title == "" {
		title = "未命名对话"
	}
	text.WriteString(title)
	if !e.OccurredAt.IsZero() {
		text.WriteString("\n时间：" + e.OccurredAt.Local().Format("2006-01-02 15:04:05 -07:00"))
	}
	switch e.Kind {
	case TurnCompleted:
		if e.SchemaVersion == 2 {
			text.WriteString("\n\n本轮已结束，可以查看结果、继续当前工作或安排下一步。")
		}
	case InputRequested:
		if e.SchemaVersion == 2 {
			text.WriteString("\n\n请回到 " + e.AgentName() + " 回复。")
		}
		for _, question := range e.Questions {
			text.WriteString("\n\n" + question.Text)
			for index, option := range question.Options {
				fmt.Fprintf(&text, "\n%s. %s", optionLetter(index), option)
			}
		}
	case ApprovalRequested:
		text.WriteString("\n\n请在 " + e.AgentName() + " 中确认审批。")
		if e.SchemaVersion == 2 && e.Detail != "" {
			text.WriteString("\n" + LimitBytes(e.Detail, 1500))
		}
	case AttentionRequired:
		text.WriteString("\n\n请回到 " + e.AgentName() + " 查看并决定下一步。")
		if e.Detail != "" {
			text.WriteString("\n" + LimitBytes(e.Detail, 1500))
		}
	}
	return text.String()
}

func (e Event) Text() string { return e.Title() + "\n" + e.Body() }

func optionLetter(index int) string {
	label := ""
	for index++; index > 0; index = (index - 1) / 26 {
		label = string(rune('A'+(index-1)%26)) + label
	}
	return label
}
