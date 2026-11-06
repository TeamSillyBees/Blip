# Claude Code 回调样本

`native-hooks.jsonl` 来自官方 npm 包 `@anthropic-ai/claude-code@2.1.318` 的 Windows x64 原生 CLI，采集日期 2026-11-06。通过 `TestClaudeNativeHooks` 在临时 home 运行真实 CLI；模型响应由本机 HTTP SSE 测试服务返回，未调用真实模型或通知渠道。保留原字段结构，替换临时路径、session 和 prompt UUID。

已直接观察 `UserPromptSubmit` 和 `Stop`，二者共享 `prompt_id`。真实 CLI 以安装器生成的 executable + args 调用 Blip，原生进程使用包含中文、空格、美元符号、`&` 和单引号的路径。重开 adapter 后可恢复 Stop 候选并创建一条中性通知。

审批、AskUserQuestion、Elicitation、StopFailure 以及继续执行的组合在 Go 测试中采用[官方 hooks 文档](https://code.claude.com/docs/en/hooks)构造的协议样本；这些测试不作为对应路径的真实客户端实测。

重跑真实 CLI 测试：设置 `BLIP_CLAUDE_TEST_BINARY` 为官方原生可执行文件的绝对路径，执行 `go test ./cmd/blip -run TestClaudeNativeHooks -count=1 -v`。需要重新采样时额外设置 `BLIP_CLAUDE_FIXTURE_DIR`，审核生成文件后再提交。
