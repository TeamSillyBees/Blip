package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHumanNotificationFormats(t *testing.T) {
	occurredAt := time.Unix(1, 0).UTC()
	bodyPrefix := "优化通知展示\n时间：" + occurredAt.Local().Format("2006-01-02 15:04:05 -07:00")
	questions := []Question{
		{Text: "请选择通知渠道", Options: []string{"飞书：团队群", "Gotify：手机提醒", "Webhook", "企业微信"}},
		{Text: "启用测试？", Options: []string{"是", "否"}},
	}
	for _, entry := range []struct {
		kind        Kind
		title, body string
	}{
		{TurnCompleted, "完成 · Blip · 开发工作站", bodyPrefix},
		{InputRequested, "需要输入 · Blip · 开发工作站", bodyPrefix + "\n\n请选择通知渠道\nA. 飞书：团队群\nB. Gotify：手机提醒\nC. Webhook\nD. 企业微信\n\n启用测试？\nA. 是\nB. 否"},
		{ApprovalRequested, "需要输入 · Blip · 开发工作站", bodyPrefix + "\n\n请在 Codex 中确认审批。"},
	} {
		t.Run(string(entry.kind), func(t *testing.T) {
			e := Event{Kind: entry.kind, Project: "Blip", SessionTitle: "  优化通知展示  ", DeviceName: "开发工作站", SessionID: "private-session", TurnID: "private-turn", Detail: "private-command-or-reply", OccurredAt: occurredAt, Questions: questions}
			if e.Title() != entry.title || e.Body() != entry.body || e.Text() != entry.title+"\n"+entry.body {
				t.Fatalf("wrong human notification: title=%q body=%q text=%q", e.Title(), e.Body(), e.Text())
			}
			for _, hidden := range []string{"private-", "设备：", "会话：", "轮次："} {
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
	e.DeviceName = strings.Repeat("设备🙂", 100)
	title := e.Title()
	parts := strings.Split(title, " · ")
	if !utf8.ValidString(title) || len(parts) != 3 || len(parts[1]) > 120 || len(parts[2]) > 128 || !strings.HasSuffix(parts[1], "…") || !strings.HasSuffix(parts[2], "…") {
		t.Fatalf("long Unicode project or device broke title bound: %q", title)
	}
}

func TestNotificationTimeUsesLocalEventTime(t *testing.T) {
	originalLocal := time.Local
	t.Cleanup(func() { time.Local = originalLocal })
	e := Event{Kind: TurnCompleted, SessionTitle: "对话", OccurredAt: time.Date(2026, 11, 6, 20, 3, 4, 0, time.UTC)}
	for _, entry := range []struct {
		offset int
		want   string
	}{
		{8 * 60 * 60, "对话\n时间：2026-11-07 04:03:04 +08:00"},
		{-(4*60 + 30) * 60, "对话\n时间：2026-11-06 15:33:04 -04:30"},
	} {
		time.Local = time.FixedZone("test-local", entry.offset)
		if body := e.Body(); body != entry.want {
			t.Fatalf("event time was not converted to local time: got %q, want %q", body, entry.want)
		}
	}
	e.OccurredAt = time.Time{}
	if body := e.Body(); body != "对话" {
		t.Fatalf("zero event time should be omitted: %q", body)
	}
}

func TestLegacyEventsWithoutSessionTitleRemainValid(t *testing.T) {
	e := Event{SchemaVersion: 1, ID: ID("legacy"), Agent: "codex", Kind: InputRequested, OccurredAt: time.Unix(1, 0).UTC(), SessionID: "session", TurnID: "turn", Project: "project", Detail: "legacy verbose detail"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	wantBody := "未命名对话\n时间：" + e.OccurredAt.Local().Format("2006-01-02 15:04:05 -07:00")
	if e.Title() != "需要输入 · project" || e.Body() != wantBody {
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
	if e.Body() != wantBody {
		t.Fatal("blank conversation title did not use fallback")
	}
}
