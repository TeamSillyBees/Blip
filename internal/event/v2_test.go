package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestV2IndependentOfAdapterAndNativeTurn(t *testing.T) {
	e := Event{SchemaVersion: 2, ID: SourceEventID("removed", "future-agent", "handoff", "s", "message"), Agent: "future-agent", SourceID: "removed", Source: "future-agent/hooks", Kind: AttentionRequired, Reason: "stop_signal", SessionID: "s", OccurredAt: time.Now()}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(e)
	if err != nil || strings.Contains(string(data), "turn_id") {
		t.Fatalf("optional native turn fabricated: %s %v", data, err)
	}
	if strings.Contains(e.Text(), "完成") || !strings.Contains(e.Text(), "停止信号") {
		t.Fatalf("unknown outcome claims success: %s", e.Text())
	}
	for _, mutate := range []func(*Event){
		func(e *Event) { e.SourceID = "" }, func(e *Event) { e.SessionID = "" }, func(e *Event) { e.Reason = "success" }, func(e *Event) { e.SchemaVersion = 1 }, func(e *Event) { e.Kind = TurnCompleted },
	} {
		invalid := e
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Fatalf("accepted invalid event: %+v", invalid)
		}
	}
}

func TestSourceIdentityIsUnambiguousAndNamespaced(t *testing.T) {
	a := SourceEventID("a", "codex", "handoff", "x\x00y", "z")
	b := SourceEventID("a", "codex", "handoff", "x", "y\x00z")
	c := SourceEventID("b", "codex", "handoff", "x\x00y", "z")
	if a == b || a == c || a != SourceEventID("a", "codex", "handoff", "x\x00y", "z") {
		t.Fatal("unstable or colliding source identity")
	}
}

func TestV2HumanHandoffText(t *testing.T) {
	for _, item := range []struct {
		kind         Kind
		reason, want string
	}{{TurnCompleted, "", "下一步"}, {InputRequested, "", "Claude Code 回复"}, {ApprovalRequested, "", "Claude Code 中确认"}, {AttentionRequired, "error", "运行遇到问题"}} {
		e := Event{SchemaVersion: 2, Agent: "claude-code", Kind: item.kind, Reason: item.reason, Project: "Blip", DeviceName: "workstation", OccurredAt: time.Now()}
		if !strings.Contains(e.Text(), item.want) || !strings.HasSuffix(e.Title(), " · workstation · Claude Code") || !strings.Contains(e.Body(), "时间：") {
			t.Fatalf("missing handoff context: %s", e.Text())
		}
	}
}
