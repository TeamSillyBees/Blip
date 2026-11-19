# Claude Code 来源

源码中的 Claude Code adapter 使用事件 v2，复用已有飞书、企业微信、Gotify 和 webhook 渠道。安装协议以官方 CLI **2.1.318** 为验证基线；直接 executable + args 与 `onFailure: continue` 要求支持对应字段的版本，旧版本须另行验证。

```powershell
blip install claude-code
```

首次接入会读取 `CLAUDE_CONFIG_DIR`，缺省使用 `~/.claude`。安装器合并该目录的 `settings.json`，保留其他 hooks、权限及信任设置。若显式关闭 hooks，请先在 Claude Code 中启用。新 hook 仍须按 Claude Code 的要求审阅和信任。

| 信号 | 提醒与处理 |
| --- | --- |
| Stop | 返回后由 worker 做最多 1 秒的合并，发送“收到停止信号，请查看”；其他 hook 可能继续执行，因此不标为正常完成 |
| StopFailure | 立即发送异常提醒，仅保存错误类别，不默认转发完整错误正文 |
| AskUserQuestion 的 PreToolUse | 立即发送需要回复，携带问题和选项 |
| MCP Elicitation | 立即发送需要回复，携带请求说明；不自动回答 |
| PermissionRequest | 立即发送需要确认，附工具名；不自动批准或拒绝 |

子 agent、TaskCompleted、SessionEnd 和闲置 Notification 不生成根会话接手提醒。官方 Stop 不在用户中断时触发；Blip 不从普通进程退出推断成功。

当前使用原生 `prompt_id` 关联同轮信号，`tool_use_id` / `elicitation_id` 关联请求。`prompt_id` 不作为对外 `turn_id` 冒充原生轮次。停止后观察到新工具执行，或带新回复内容的 `stop_hook_active` 回调时，允许再次提醒；重启、重复停止或迟到结果不重复发送。同一请求存在时合并泛化停止，Claude 同步工具结果或 ElicitationResult 可解除对应等待。

旧 payload 缺少 `prompt_id` 时，按会话保存递增序号，在 UserPromptSubmit 时推进；相同输入的重复回调仅在 2 秒窗口内合并。缺少 request ID 时使用同轮内容摘要合并。这些是尽力去重：缺失开始回调、旧回放、连续相同请求或完全相同的继续回复可能导致重复或合并，不声称精确轮次身份。

该来源没有 transcript collector，无法补收未持久化的回调，也不读取整个 Claude home。对话名暂使用“未命名对话”。Windows 服务仅需访问公共队列，不新增 Claude 配置目录读取权限。

验证包括真实 Windows CLI 的 UserPromptSubmit / Stop、本机模拟模型、持久候选恢复，以及直接进程的特殊字符路径。其他事件使用官方协议样本测试；真实模型、所有交互分支和外部通知渠道未在这项测试中验收。样本与重跑方法见 [testdata](../internal/agents/testdata/claude-code/README.md)。
