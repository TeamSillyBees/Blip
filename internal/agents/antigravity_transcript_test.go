package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

func TestAntigravityStopIncludesCurrentVisibleReply(t *testing.T) {
	home := t.TempDir()
	db := summaryFixture(t, home)
	execSummary(t, db, "INSERT INTO conversation_summaries (conversation_id) VALUES ('chat')")
	a := newAntigravity(config.Config{StateDir: t.TempDir()}, config.Source{ID: "app", Home: home})
	chat := filepath.Join(home, "brain", "chat")
	logs := filepath.Join(chat, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0700); err != nil {
		t.Fatal(err)
	}
	parse := func(h antigravityHook) []event.Event {
		t.Helper()
		data, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		events, err := a.ParseHook(context.Background(), data, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	first, invocation := 3, 0
	parse(antigravityHook{Name: "PreInvocation", Conversation: "chat", Artifacts: chat, InitialSteps: &first, Invocation: &invocation})
	h := antigravityHook{Name: "Stop", Conversation: "chat", Artifacts: chat, FullyIdle: true, Termination: "NO_TOOL_CALL"}
	const final = `{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"已完成。\n第二行🙂","thinking":"private reasoning"}`
	const visible = "已完成。\n第二行🙂"
	// A supplied path cannot redirect a callback to another conversation's text.
	h.Transcript = filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(h.Transcript, []byte(final), 0600); err != nil {
		t.Fatal(err)
	}
	bare := parse(h)
	if len(bare) != 1 || bare[0].Kind != event.TurnCompleted || bare[0].Detail != "" {
		t.Fatalf("missing owned transcript: %+v", bare)
	}
	long := strings.Repeat("结果🙂", 1000)
	for _, tc := range []struct{ name, transcript, want string }{
		{"final reply with trailing metadata", final + "\r\n" + `{"step_index":5,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"internal metadata"}`, visible},
		{"tool output", `{"step_index":4,"type":"GENERIC","source":"MODEL","status":"DONE","content":"tool output"}`, ""},
		{"earlier turn", strings.Replace(final, `"step_index":4`, `"step_index":1`, 1), ""},
		{"new user input", final + "\n" + `{"step_index":5,"type":"USER_INPUT","content":"next user prompt"}`, ""},
		{"tool-call commentary", final + "\n" + `{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"Starting a tool","tool_calls":[{"name":"run_command"}]}`, ""},
		{"unfinished", strings.Replace(final, "DONE", "GENERATING", 1), ""},
		{"reasoning only", strings.Replace(final, `"content":"已完成。\n第二行🙂",`, "", 1), ""},
		{"malformed tail", final + "\n" + `{"step_index":`, ""},
		{"large history", `{"content":"` + strings.Repeat("x", antigravityTranscriptTail) + `"}` + "\n" + final, visible},
		{"long Unicode reply", strings.Replace(final, `已完成。\n第二行🙂`, long, 1), event.LimitBytes(long, 4000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(logs, "transcript.jsonl"), []byte(tc.transcript+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			got := parse(h)
			if len(got) != 1 || got[0].ID != bare[0].ID || got[0].Detail != tc.want || !strings.Contains(got[0].Body(), tc.want) {
				t.Fatalf("wrong completion: %+v", got)
			}
		})
	}
	h.Termination = "error"
	if got := parse(h); len(got) != 1 || got[0].Kind != event.AttentionRequired || got[0].Detail != "" {
		t.Fatalf("unexpected detail on error: %+v", got)
	}
}
