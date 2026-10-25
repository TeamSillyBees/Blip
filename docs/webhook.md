# 自定义 webhook 协议 v1

Blip 使用 HTTP POST，`Content-Type: application/json`，任何 HTTP 2xx 表示接收成功，忽略响应正文。不会跟随重定向。可选 Bearer token 或静态请求头，首版没有任意请求体模板。

```json
{
  "schema_version": 1,
  "id": "64位十六进制事件标识",
  "agent": "codex",
  "kind": "input_requested",
  "source": "codex/hooks",
  "occurred_at": "2026-10-25T09:30:00Z",
  "session_id": "session-id",
  "turn_id": "turn-id",
  "request_id": "call-id",
  "project": "Blip",
  "cwd": "D:\\Project\\TeamSillyBees\\Blip",
  "detail": "Codex 发起了提问，请查看客户端。",
  "questions": [
    {"text": "请选择通知渠道", "options": ["飞书", "Gotify"]}
  ]
}
```

`kind` 为 `turn_completed`、`approval_requested`、`input_requested`。`source` 为 `codex/hooks` 或 `codex/rollout`；手动测试通知使用 `blip/test`。`request_id`、`cwd`、`detail`、`questions` 在没有内容时可以缺省；发生时间采用 RFC 3339 UTC。

每次重试携带相同的 `id` 和 `X-Blip-Event-Id` 请求头。接收端应先持久接收，再返回 2xx，并按事件标识去重。Blip 的本地去重与外部接收不是一个事务，存在重复投递窗口。

不会把渠道配置或完整会话载荷放入这个协议。请求详情可能包含用户问题、回复摘要或待授权命令，内容范围以首版需求为准。
