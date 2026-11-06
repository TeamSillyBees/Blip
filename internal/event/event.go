// Package event defines the notification contract shared by collectors and senders.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type Kind string

const (
	TurnCompleted     Kind = "turn_completed"
	ApprovalRequested Kind = "approval_requested"
	InputRequested    Kind = "input_requested"
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
	DeviceName    string     `json:"device_name,omitempty"`
	Kind          Kind       `json:"kind"`
	Source        string     `json:"source"`
	OccurredAt    time.Time  `json:"occurred_at"`
	SessionID     string     `json:"session_id"`
	SessionTitle  string     `json:"session_title,omitempty"`
	TurnID        string     `json:"turn_id"`
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
	if e.SchemaVersion != 1 || e.Agent != "codex" || len(e.ID) != 64 || e.SessionID == "" || e.TurnID == "" || e.OccurredAt.IsZero() {
		return fmt.Errorf("invalid notification identity")
	}
	if _, err := hex.DecodeString(e.ID); err != nil {
		return fmt.Errorf("invalid notification ID")
	}
	switch e.Kind {
	case TurnCompleted, ApprovalRequested, InputRequested:
		return nil
	default:
		return fmt.Errorf("unsupported notification kind")
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
	case InputRequested:
		for _, question := range e.Questions {
			text.WriteString("\n\n" + question.Text)
			for index, option := range question.Options {
				fmt.Fprintf(&text, "\n%s. %s", optionLetter(index), option)
			}
		}
	case ApprovalRequested:
		text.WriteString("\n\n请在 Codex 中确认审批。")
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
