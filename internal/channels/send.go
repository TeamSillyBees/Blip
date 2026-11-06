// Package channels implements the four outbound protocols.
package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

type SendError struct {
	Message   string
	Retryable bool
}

func (e *SendError) Error() string { return e.Message }
func Retryable(err error) bool {
	var failure *SendError
	return !errors.As(err, &failure) || failure.Retryable
}

type Sender struct {
	Client *http.Client
	Now    func() time.Time
}

func New(timeout time.Duration) *Sender {
	return &Sender{Client: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Now: time.Now}
}

func FeishuSignature(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Sender) Send(ctx context.Context, destination config.Destination, notification event.Event) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	target := destination.URL
	var payload any
	switch destination.Kind {
	case "feishu":
		body := map[string]any{"msg_type": "text", "content": map[string]string{"text": event.LimitBytes(notification.Text(), 16000)}}
		if destination.Secret != "" {
			timestamp := strconv.FormatInt(s.Now().Unix(), 10)
			body["timestamp"] = timestamp
			body["sign"] = FeishuSignature(timestamp, destination.Secret)
		}
		payload = body
	case "wecom":
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": event.LimitBytes(notification.Text(), 2048)}}
	case "gotify":
		parsed, err := url.Parse(target)
		if err != nil {
			return &SendError{"invalid Gotify URL", false}
		}
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/message"
		parsed.RawPath = ""
		target = parsed.String()
		priority := destination.Priority
		if priority == 0 {
			priority = 5
		}
		payload = map[string]any{"title": notification.Title(), "message": event.LimitBytes(notification.Body(), 16000), "priority": priority}
	case "webhook":
		payload = notification
	default:
		return &SendError{"unsupported notification channel", false}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return &SendError{"cannot encode notification", false}
	}
	if destination.Kind == "feishu" {
		// JSON escaping can expand control characters sixfold; cap the encoded body too.
		body := payload.(map[string]any)
		content := body["content"].(map[string]string)
		for len(data) > 20000 {
			content["text"] = event.LimitBytes(content["text"], len(content["text"])*3/4)
			data, _ = json.Marshal(body)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return &SendError{"cannot construct notification request", false}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Blip/0.1")
	if destination.Kind == "webhook" {
		for key, value := range destination.Headers {
			request.Header.Set(key, value)
		}
		if destination.Token != "" {
			request.Header.Set("Authorization", "Bearer "+destination.Token)
		}
		request.Header.Set("X-Blip-Event-Id", notification.ID)
	}
	if destination.Kind == "gotify" {
		request.Header.Set("X-Gotify-Key", destination.Token)
	}
	response, err := s.Client.Do(request)
	if err != nil {
		return &SendError{"notification transport failed or timed out", true}
	}
	defer releaseResponse(response, cancel)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &SendError{fmt.Sprintf("notification HTTP status %d", response.StatusCode), response.StatusCode == 408 || response.StatusCode == 425 || response.StatusCode == 429 || response.StatusCode >= 500}
	}
	if destination.Kind == "webhook" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return &SendError{"cannot read notification response", true}
	}
	if destination.Kind == "gotify" {
		var result struct {
			ID *int64 `json:"id"`
		}
		if json.Unmarshal(body, &result) != nil || result.ID == nil || *result.ID <= 0 {
			return &SendError{"invalid Gotify success response", true}
		}
		return nil
	}
	var result struct {
		Code    *int `json:"code"`
		ErrCode *int `json:"errcode"`
	}
	if json.Unmarshal(body, &result) != nil {
		return &SendError{"invalid bot response JSON", true}
	}
	code := result.Code
	if destination.Kind == "wecom" {
		code = result.ErrCode
	}
	if code == nil {
		return &SendError{"bot response is missing its success code", true}
	}
	if *code != 0 {
		return &SendError{fmt.Sprintf("bot rejected notification (code %d)", *code), true}
	}
	return nil
}

// Small response bodies are consumed so normal webhook replies and HTTP errors
// can reuse their connection. Cleanup is bounded even if the peer stalls after
// returning headers; response content never changes a webhook's 2xx success.
func releaseResponse(response *http.Response, cancel context.CancelFunc) {
	defer response.Body.Close()
	if response.ContentLength < 0 || response.ContentLength > 64*1024 {
		return
	}
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024+1))
}
