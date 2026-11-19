# Hermes 来源

Hermes adapter 使用 profile 的 `config.yaml` shell hooks。安装到实际使用的 `HERMES_HOME` 或 profile home，不推断共享 gateway 的进程环境属于哪个会话。

```powershell
blip install hermes
```

首次接入默认读取 `HERMES_HOME`，缺省使用 `~/.hermes`。自定义 profile 可直接运行 `blip install hermes --source hermes-work --home 'D:\Agent Homes\hermes-work'`。

安装器合并 YAML 中的 hooks，保留注释、其他 hook、outbound 配置和信任设置。它不会改写 `hooks_auto_accept` 或 `shell-hooks-allowlist.json`；需在 Hermes 中审阅和授权新增命令。卸载只删除安装清单中完整匹配的条目，用户修改过的命令条目会报告冲突。

Hermes 的命令字符串通过其 `split_command_line` 拆分后直接启动 Blip，JSON 经过 stdin。Windows 使用双引号包围路径；POSIX 使用单引号转义；不需要 `.cmd`。当前不支持路径中的双引号或控制字符。

| 事件 | 行为 |
| --- | --- |
| on_session_end + completed | 立即发送“本轮已结束” |
| on_session_end + failed | 立即发送异常提醒，保留简短退出原因 |
| 有轮次结果的结束信号，但结果不明 | 发送中性“已停下，请查看” |
| interrupted / 明确取消 | 抑制接手提醒 |
| on_human_input_request | 实际等待用户时发送；kind=approval 为需要确认，其余为需要回复 |
| on_human_input_resolved | 解除对应请求；不保存用户回答或密码 |
| pre_tool_call | 关联轮次和 clarify 的问题选项；普通工具调用不发送提醒 |
| subagent_start | 保存子会话身份；其结束与请求不生成根会话通知 |

shell envelope 把 `session_id` 放在顶层，`turn_id`、`request_id` 和结果位于 `extra`。Blip 不把原始 Python kwargs 当成 shell payload。`pre_approval_request` 可能表示自动审批，因此本适配器选择实际人工等待事件，避免过早提醒。

每个来源、会话独立持久关联。请求 ID 可精确去重；等待期间的结束信号被合并。只有原生 resolved 才解除等待，普通工具返回不能代替用户回答。原生 turn ID 缺少时使用持久序号与 2 秒的有限合并，属于尽力去重。普通进程退出的精简 on_session_end 不触发通知。

当前版本的子会话过滤依赖先收到 `subagent_start`；应先安装并授权，再新建会话。在运行中途接入、丢失子会话建立回调或旧版本字段不全时，无法保证完整排除子任务。该来源没有日志 collector，停用期间或持久化前被强制终止的回调无法补收；没有对话名查询，也不为 Windows 服务新增 Hermes home 的读取权限。

验证基线为 Hermes `4d043038`、Python 3.14.7、Windows x64。实际 shell runner、clarify 和人工等待回调已在隔离环境中验证；模型轮次结果使用固定源码的受控 kwargs 分发，未将其标为真实模型全链路验收。详见 [样本与重跑方法](../internal/agents/testdata/hermes/README.md)。
