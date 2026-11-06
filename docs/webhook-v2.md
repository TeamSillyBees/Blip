# 自定义 webhook 协议 v2

传输、鉴权、重试和 `X-Blip-Event-Id` 与 [v1](webhook.md) 相同。接收端按 `schema_version` 分支；同一目的地可以收到旧来源的 v1 和新增来源的 v2，旧队列不会转换版本。

```json
{
  "schema_version": 2,
  "id": "64位十六进制事件标识",
  "agent": "codex",
  "source_id": "codex-work",
  "source": "codex/hooks",
  "kind": "attention_required",
  "reason": "stop_signal",
  "occurred_at": "2026-11-06T13:30:00Z",
  "session_id": "session-id",
  "turn_id": "turn-id",
  "session_title": "实现通知模板",
  "project": "Blip",
  "device_name": "开发电脑",
  "detail": "收到停止信号，请回到原会话查看。"
}
```

`source_id` 必填，标识 Blip 中稳定的接入实例。`agent` 是实际 harness，`source` 是具体采集途径。`session_id` 必填；原生 `turn_id`、`request_id` 缺失时省略，不用合成 ID 冒充原生字段。其余显示字段沿用 v1。

| kind | 含义 |
| --- | --- |
| `turn_completed` | 本轮正常结束，可以查看结果或安排下一步；不表示业务目标已验收 |
| `input_requested` | 需要回复，有结构化问题和选项时附 `questions` |
| `approval_requested` | 观察到授权申请，实际送达时可能已经处理 |
| `attention_required` | 已观察到停止或异常信号，需要查看原会话 |

只有 `attention_required` 带 `reason`，取值为 `stopped`、`error` 或 `stop_signal`。`stop_signal` 表示已收到停止信号，但结果尚不明确；它不能解释为成功，也不能承诺 harness 此后不会继续。

实际生成哪些 kind 取决于 adapter 的能力报告。公共协议支持某字段不等于每个 harness 都提供相应信号；具体支持和验证状态见 [兼容性记录](compatibility.md)。

新来源的 ID 使用带版本的 JSON 数组编码后进行 SHA-256，包含 source、harness 与 adapter 的稳定身份。默认 Codex 保留旧请求和正常结束算法。同一次接手的第一份入队快照和收据不变，重试携带相同 ID；接收端不应按标题或 kind 自建跨事件去重。

来源删除、停用或 adapter 不可用后，已入队 v2 仍可投递。可选对话名称只向匹配来源查询；查不到时省略，不使用其他 home 的同名 session。
