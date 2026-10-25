# Codex 请求采用官方 hook，完成采用结构化轮次结束记录

Codex 的 notify 仅给出 agent-turn-complete，载荷没有可用于区分正常结束、中断和失败的结果状态；Stop 还可能被其他 hook 要求继续执行。Blip 对输入和审批使用官方 PreToolUse / PermissionRequest，对完成按根会话与轮次读取结构化 task_complete，并记录读取位置。这样可以独立接入现有 CLI 与桌面应用，也避免把另起 App Server 当作旁听现有实例的手段。

代价是完成事件依赖版本相关的会话记录格式，审批仍受官方 hook 实际覆盖限制。用户已接受首版明确记录这些边界。根会话需要正向识别，子 agent 结束、历史回放、中断和失败不能直接生成完成提醒。
