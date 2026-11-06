package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

func sample(kind event.Kind) event.Event {
	return event.Event{SchemaVersion: 1, ID: event.ID("test", string(kind)), Agent: "codex", DeviceName: "开发工作站🙂", Kind: kind, Source: "codex/hooks", OccurredAt: time.Unix(1599360473, 0).UTC(), SessionID: "session", TurnID: "turn", Project: "中文项目", Detail: strings.Repeat("中文🙂", 600)}
}

func TestAllEventKindsReachAllChannelProtocols(t *testing.T) {
	for _, channel := range []string{"feishu", "wecom", "gotify", "webhook"} {
		for _, kind := range []event.Kind{event.TurnCompleted, event.ApprovalRequested, event.InputRequested} {
			t.Run(channel+"/"+string(kind), func(t *testing.T) {
				var received bool
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					received = true
					if request.Method != "POST" || request.Header.Get("Content-Type") != "application/json" {
						t.Error("wrong HTTP contract")
					}
					var body map[string]any
					if json.NewDecoder(request.Body).Decode(&body) != nil {
						t.Error("invalid JSON")
					}
					switch channel {
					case "feishu":
						if body["msg_type"] != "text" || body["timestamp"] != "1599360473" || body["sign"] != "q4jswNiMy51J5JuQV566yJat0/lQ/c+22kINzUgKsGU=" {
							t.Error("incorrect Feishu protocol or signature")
						}
						if !strings.Contains(body["content"].(map[string]any)["text"].(string), "设备：开发工作站🙂") {
							t.Error("Feishu notification lost device name")
						}
						io.WriteString(writer, `{"code":0}`)
					case "wecom":
						text := body["text"].(map[string]any)["content"].(string)
						if body["msgtype"] != "text" || len(text) > 2048 || !utf8.ValidString(text) || !strings.Contains(text, "设备：开发工作站🙂") {
							t.Error("WeCom text byte limit violated")
						}
						io.WriteString(writer, `{"errcode":0}`)
					case "gotify":
						if request.URL.Path != "/prefix/message" || request.Header.Get("X-Gotify-Key") != "test-token" || body["priority"] != float64(5) {
							t.Error("wrong Gotify path, token or priority")
						}
						if !strings.Contains(body["title"].(string), "开发工作站🙂") || !strings.Contains(body["message"].(string), "设备：开发工作站🙂") {
							t.Error("Gotify title or message lost device name")
						}
						io.WriteString(writer, `{"id":12,"appid":2}`)
					case "webhook":
						if body["kind"] != string(kind) || body["schema_version"] != float64(1) || body["device_name"] != "开发工作站🙂" || request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("X-Source") != "test" || request.Header.Get("X-Blip-Event-Id") != sample(kind).ID {
							t.Error("wrong custom webhook contract")
						}
						writer.WriteHeader(http.StatusNoContent)
					}
				}))
				defer server.Close()
				sender := New(time.Second)
				sender.Now = func() time.Time { return time.Unix(1599360473, 0) }
				target := server.URL
				if channel == "gotify" {
					target += "/prefix/"
				}
				err := sender.Send(context.Background(), config.Destination{Kind: channel, URL: target, Token: "test-token", Secret: "secret", Headers: map[string]string{"X-Source": "test"}}, sample(kind))
				if err != nil || !received {
					t.Fatalf("notification not delivered: %v", err)
				}
			})
		}
	}
}

func TestHTTP200DoesNotHideBotFailure(t *testing.T) {
	for _, entry := range []struct{ kind, body string }{{"feishu", `{"code":19024}`}, {"wecom", `{"errcode":93000}`}, {"feishu", `{}`}, {"wecom", `not json`}, {"wecom", ``}, {"gotify", `{"error":"unauthorized"}`}} {
		t.Run(entry.kind+entry.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, entry.body) }))
			defer server.Close()
			if err := New(time.Second).Send(context.Background(), config.Destination{Kind: entry.kind, URL: server.URL}, sample(event.InputRequested)); err == nil {
				t.Fatal("business failure was treated as success")
			}
		})
	}
}

func TestRetryClassificationAndCredentialRedaction(t *testing.T) {
	for _, status := range []int{400, 401, 429, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, "secret-response")
		}))
		err := New(time.Second).Send(context.Background(), config.Destination{Kind: "webhook", URL: server.URL + "/?key=secret-key"}, sample(event.TurnCompleted))
		server.Close()
		if err == nil || Retryable(err) != (status >= 500 || status == 429) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe or wrong error for %d: %v", status, err)
		}
	}
}

func TestTimeoutAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer server.Close()
	err := New(30*time.Millisecond).Send(context.Background(), config.Destination{Kind: "webhook", URL: server.URL}, sample(event.TurnCompleted))
	if err == nil || !Retryable(err) {
		t.Fatal("timeout must be retryable")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, 302) }))
	defer redirect.Close()
	if err = New(time.Second).Send(context.Background(), config.Destination{Kind: "webhook", URL: redirect.URL}, sample(event.TurnCompleted)); err == nil || Retryable(err) {
		t.Fatal("redirect must not silently forward credentials")
	}
}

func TestFeishuCapsEncodedBodyWithEscapedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 20000 || !json.Valid(data) {
			t.Error("Feishu encoded body exceeds limit")
		}
		io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	notification := sample(event.TurnCompleted)
	notification.Detail = strings.Repeat("\x00", 16000)
	if err := New(time.Second).Send(context.Background(), config.Destination{Kind: "feishu", URL: server.URL}, notification); err != nil {
		t.Fatal(err)
	}
}

func TestSmallWebhookResponsesReuseConnection(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			addresses := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				addresses <- r.RemoteAddr
				w.WriteHeader(status)
				io.WriteString(w, "small response body")
			}))
			defer server.Close()
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			sender := New(time.Second)
			sender.Client.Transport = transport
			for range 2 {
				err := sender.Send(context.Background(), config.Destination{Kind: "webhook", URL: server.URL}, sample(event.TurnCompleted))
				if status == http.StatusOK && err != nil || status == http.StatusBadRequest && (err == nil || Retryable(err)) {
					t.Fatalf("unexpected HTTP result: %v", err)
				}
			}
			if first, second := <-addresses, <-addresses; first != second {
				t.Fatalf("small response body prevented connection reuse: %q then %q", first, second)
			}
		})
	}
}

func TestResponseCleanupDoesNotWaitForStalledBodies(t *testing.T) {
	for _, entry := range []struct {
		name          string
		contentLength string
		status        int
	}{
		{"small success", "10", http.StatusOK},
		{"unknown length success", "", http.StatusOK},
		{"large success", "65537", http.StatusOK},
		{"small retryable error", "10", http.StatusServiceUnavailable},
	} {
		t.Run(entry.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if entry.contentLength != "" {
					w.Header().Set("Content-Length", entry.contentLength)
				}
				w.WriteHeader(entry.status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			err := New(3*time.Second).Send(ctx, config.Destination{Kind: "webhook", URL: server.URL}, sample(event.TurnCompleted))
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("response cleanup waited for a stalled body for %v", elapsed)
			}
			if entry.status == http.StatusOK && err != nil || entry.status == http.StatusServiceUnavailable && (err == nil || !Retryable(err)) {
				t.Fatalf("response cleanup changed HTTP result: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("response cleanup canceled the caller's context")
			}
		})
	}
}
