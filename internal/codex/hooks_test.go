package codex

import (
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
)

func TestStructuredQuestionsAndAsyncReturn(t *testing.T) {
	now := time.Unix(1000, 0)
	payload := `{"hook_event_name":"PreToolUse","session_id":"s","turn_id":"t","cwd":"C:\\项目 目录\\Blip","tool_name":"functions.request_user_input_async","tool_use_id":"call-1","tool_input":{"questions":[{"title":"请选择渠道","options":["飞书",{"label":"Gotify","description":"手机提醒"}]},{"question":"第二个问题？","options":[]}]}}`
	notification, err := ParseHook(strings.NewReader(payload), now)
	if err != nil || notification == nil {
		t.Fatalf("parse: %v", err)
	}
	if notification.Kind != event.InputRequested || notification.Project != "Blip" || len(notification.Questions) != 2 || notification.Questions[0].Options[1] != "Gotify：手机提醒" {
		t.Fatalf("question information lost: %+v", notification)
	}
	replayed, _ := ParseHook(strings.NewReader(payload), now.Add(time.Minute))
	if replayed.ID != notification.ID {
		t.Fatal("native tool identity must deduplicate replay")
	}
	post := strings.Replace(payload, "PreToolUse", "PostToolUse", 1)
	if returned, err := ParseHook(strings.NewReader(post), now); err != nil || returned != nil {
		t.Fatal("async PostToolUse was treated as an input or resolution event")
	}
}

func TestPermissionRequestAndIgnoredTool(t *testing.T) {
	payload := `{"hook_event_name":"PermissionRequest","session_id":"s","turn_id":"t","tool_name":"Bash","tool_input":{"command":"build","justification":"需要授权"}}`
	notification, err := ParseHook(strings.NewReader(payload), time.Now())
	if err != nil || notification == nil || notification.Kind != event.ApprovalRequested || !strings.Contains(notification.Detail, "需要授权") {
		t.Fatal("approval request not captured")
	}
	ignored := strings.Replace(payload, "PermissionRequest", "PreToolUse", 1)
	if notification, err = ParseHook(strings.NewReader(ignored), time.Now()); err != nil || notification != nil {
		t.Fatal("unrelated tool was captured")
	}
	if _, err = ParseHook(strings.NewReader(`{"hook_event_name":"PermissionRequest"}`), time.Now()); err == nil {
		t.Fatal("missing identity accepted")
	}
	if _, err = ParseHook(strings.NewReader(strings.Repeat("x", MaxHookBytes+1)), time.Now()); err == nil {
		t.Fatal("oversized payload accepted")
	}
}
