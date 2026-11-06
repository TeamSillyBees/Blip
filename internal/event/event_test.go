package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHumanNotificationFormats(t *testing.T) {
	questions := []Question{
		{Text: "请选择通知渠道", Options: []string{"飞书：团队群", "Gotify：手机提醒", "Webhook", "企业微信"}},
		{Text: "启用测试？", Options: []string{"是", "否"}},
	}
	for _, entry := range []struct {
		kind        Kind
		title, body string
	}{
		{TurnCompleted, "完成 · Blip", "优化通知展示"},
		{InputRequested, "需要输入 · Blip", "优化通知展示\n\n请选择通知渠道\nA. 飞书：团队群\nB. Gotify：手机提醒\nC. Webhook\nD. 企业微信\n\n启用测试？\nA. 是\nB. 否"},
		{ApprovalRequested, "需要输入 · Blip", "优化通知展示\n\n请在 Codex 中确认审批。"},
	} {
		t.Run(string(entry.kind), func(t *testing.T) {
			e := Event{Kind: entry.kind, Project: "Blip", SessionTitle: "  优化通知展示  ", DeviceName: "private-device", SessionID: "private-session", TurnID: "private-turn", Detail: "private-command-or-reply", OccurredAt: time.Unix(1, 0).UTC(), Questions: questions}
			if e.Title() != entry.title || e.Body() != entry.body || e.Text() != entry.title+"\n"+entry.body {
				t.Fatalf("wrong human notification: title=%q body=%q text=%q", e.Title(), e.Body(), e.Text())
			}
			for _, hidden := range []string{"private-", "设备：", "会话：", "轮次：", "时间：", "1970-"} {
				if strings.Contains(e.Text(), hidden) {
					t.Fatalf("human notification exposed metadata %q", hidden)
				}
			}
		})
	}
}

func TestNotificationTitleRemainsBoundedUTF8(t *testing.T) {
	e := Event{Kind: TurnCompleted, Project: strings.Repeat("项目🙂", 100), SessionTitle: "会话标题"}
	if title := e.Title(); !utf8.ValidString(title) || len(title) > len("完成 · ")+120 || !strings.HasSuffix(title, "…") {
		t.Fatalf("long Unicode project broke title bound: %q", title)
	}
}

func TestLegacyEventsWithoutSessionTitleRemainValid(t *testing.T) {
	e := Event{SchemaVersion: 1, ID: ID("legacy"), Agent: "codex", Kind: InputRequested, OccurredAt: time.Unix(1, 0).UTC(), SessionID: "session", TurnID: "turn", Project: "project", Detail: "legacy verbose detail"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e.Title() != "需要输入 · project" || e.Body() != "未命名对话" {
		t.Fatal("legacy notification did not use the conversation fallback")
	}
	data, err := json.Marshal(e)
	if err != nil || strings.Contains(string(data), "session_title") || strings.Contains(string(data), "device_name") {
		t.Fatalf("empty metadata fields must remain optional: %s %v", data, err)
	}
	e.DeviceName, e.SessionTitle = "开发机", "优化通知展示"
	data, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var restored Event
	if err = json.Unmarshal(data, &restored); err != nil || restored.DeviceName != e.DeviceName || restored.SessionTitle != e.SessionTitle || restored.ID != e.ID || restored.Detail != e.Detail {
		t.Fatalf("structured event lost metadata or identity: %+v %v", restored, err)
	}
	e.SessionTitle = " \t\n "
	if e.Body() != "未命名对话" {
		t.Fatal("blank conversation title did not use fallback")
	}
}
