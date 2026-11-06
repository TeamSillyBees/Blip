# 自定义 webhook 协议 v1

Blip 使用 HTTP POST，`Content-Type: application/json`，任何 HTTP 2xx 表示接收成功，忽略响应正文。不会跟随重定向。可选 Bearer token 或静态请求头，首版没有任意请求体模板。

```json
{
  "schema_version": 1,
  "id": "64位十六进制事件标识",
  "agent": "codex",
  "device_name": "开发电脑 A",
  "kind": "input_requested",
  "source": "codex/hooks",
  "occurred_at": "2026-10-25T09:30:00Z",
  "session_id": "session-id",
  "session_title": "实现通知模板",
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

`kind` 为 `turn_completed`、`approval_requested`、`input_requested`。`source` 为 `codex/hooks` 或 `codex/rollout`；手动测试通知使用 `blip/test`。`device_name` 为配置中的设备名称，默认当前计算机名；这是协议 v1 新增的可选字段，旧记录可能缺省。`request_id`、`cwd`、`detail`、`questions` 在没有内容时可以缺省；发生时间采用 RFC 3339 UTC。

`session_title` 是可选的对话名称，读取 Codex 本地名称索引；没有名称或无法读取时省略。新事件保留入队时的名称，旧事件在发送前尝试补齐。默认渠道的精简模板不改变此结构化协议，设备名、标识及详情字段继续保留。

每次重试携带相同的 `id` 和 `X-Blip-Event-Id` 请求头。接收端应先持久接收，再返回 2xx，并按事件标识去重。Blip 的本地去重与外部接收不是一个事务，存在重复投递窗口。

自定义请求头不能覆盖 `Host`、`Content-Length`、`Content-Type`、`Transfer-Encoding` 或 `X-Blip-Event-Id`，也不能同时配置仅大小写不同的重复名称。头值中的非法控制字符和 token 中的空白字符会在后台启动时被拒绝。

不会把渠道配置或完整会话载荷放入这个协议。请求详情可能包含用户问题、回复摘要或待授权命令，内容范围以首版需求为准。
