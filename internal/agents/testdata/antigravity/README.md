# Antigravity 会话摘要样本

`summaries.json` 中的 Base64 是 agy CLI 1.5.9 在 Windows 上写入 `conversation_summaries.raw_summary` 的 protobuf 片段。采样使用独立数据目录、回环 Gemini API 模拟模型和回环 Webhook。保留实际字段编号、交互类型、问题与选项，移除了路径、时间和无关元数据，替换了工具调用和轨迹 ID。

- `completed` 来自 `agy -p` 返回成功后的完全空闲会话。
- `permission` 在 `agy acp` 就 `run_command` 发出 `session/request_permission` 后、测试客户端作答前读取。
- `question` 来自 `ask_question` 等待选择 Yes 或 No 时的摘要。

运行 `go test ./internal/agents -run TestAntigravity -count=1` 会将样本写入临时 WAL 数据库，检查采集、轮次去重和重启恢复。字段投影见 [summary.proto](../../../antigravity/summary.proto)。客户端形态与数据目录可参阅 [Google Antigravity 文档](https://antigravity.google/docs/hooks)。
