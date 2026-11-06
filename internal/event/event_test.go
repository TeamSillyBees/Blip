package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDeviceNameInTitleAndBody(t *testing.T) {
	e := Event{Kind: TurnCompleted, DeviceName: "开发工作站🙂", Project: strings.Repeat("项目", 100), OccurredAt: time.Unix(1, 0).UTC()}
	if title := e.Title(); !strings.Contains(title, " · 开发工作站🙂 · ") || !utf8.ValidString(title) || len(title) > 300 {
		t.Fatalf("device name missing or title not bounded: %q", title)
	}
	if text := e.Text(); !strings.Contains(text, "\n设备：开发工作站🙂\n项目：") {
		t.Fatalf("device not clearly identified in body: %q", text)
	}
	e.DeviceName = strings.Repeat("设备🙂", 128)
	if title := e.Title(); !utf8.ValidString(title) || len(title) > 300 {
		t.Fatalf("long Unicode name broke title limit: %q", title)
	}
}

func TestLegacyEventsWithoutDeviceNameRemainValid(t *testing.T) {
	e := Event{SchemaVersion: 1, ID: ID("legacy"), Agent: "codex", Kind: InputRequested, OccurredAt: time.Unix(1, 0).UTC(), SessionID: "session", TurnID: "turn", Project: "project"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e.Title() != "Blip · 需要输入 · project" || strings.Contains(e.Text(), "设备：") {
		t.Fatal("legacy notification gained an empty device label")
	}
	data, err := json.Marshal(e)
	if err != nil || strings.Contains(string(data), "device_name") {
		t.Fatalf("empty device field must remain optional: %s %v", data, err)
	}
	e.DeviceName = "开发机"
	data, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var restored Event
	if err = json.Unmarshal(data, &restored); err != nil || restored.DeviceName != e.DeviceName || restored.ID != e.ID {
		t.Fatalf("device metadata or event identity lost: %+v %v", restored, err)
	}
}
