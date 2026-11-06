# Codex 桌面版提问样本

`desktop-input-0.167.jsonl` 来自 2026-11-07 Windows Codex Desktop / 内核 `0.167.0-alpha.3.2` 的真实 `request_user_input_async` 调用。保留 session metadata、最近 `turn_context` 和工具调用三条记录；会话、轮次、调用 ID、工作目录及问题内容已替换。未收录提示词、用户配置、其他工具参数或回答。

本次实测中，`hooks/list` 报告 Blip 的 PreToolUse hook 已启用且已信任，但连续两次桌面提问均未进入 Blip。该样本证明提问结构可从 rollout 补收；不据此断言所有客户端版本都跳过此 hook。同步 `request_user_input` 使用协议回归样本验证。

`TestDesktopInputRolloutFixture` 验证原生轮次关联、问题与选项提取。其他测试覆盖旧游标升级、首次基线、跨记录块恢复、取消、子任务、hook/rollout 双路去重和异步返回不消除输入请求。
