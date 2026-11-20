# Antigravity 原生 Hook 样本

`native-hooks.jsonl` 保留 Windows 上 agy CLI 1.5.9 回调中用于判定事件的字段。采样使用独立的用户目录和回环 Gemini API 模拟模型。样本涵盖提问、普通命令、沙箱外命令请求、后台任务结束和连续相同提示词。沙箱外请求的 `PreToolUse` 携带布尔值 `BypassSandbox: true`，在 ACP 客户端响应 `session/request_permission` 前即可收到。会话、提示词和工具调用 ID 已替换，测试运行时填入临时客户端目录和工作区路径。

运行 `go test ./internal/agents -run TestAntigravity -count=1` 可验证来源隔离、去重、等待输入和终止条件。[Google 官方协议](https://antigravity.google/docs/hooks)
