# Antigravity 桌面版与 agy CLI

Blip 通过客户端的原生 Hook 接收提问、显式权限申请、沙箱外命令请求和结束事件。回调把通知写入本地持久队列，后台进程负责投递到已配置的渠道。

## 安装与升级

Windows 使用当前账户的管理员 PowerShell。按实际使用的客户端执行命令，两端都用时各执行一次。原先使用 `0.4.0` 或更早版本的用户，安装包含沙箱外命令提醒的版本后，需要重新执行对应命令，让客户端把 `run_command` 也交给 Blip。

```powershell
blip install antigravity   # 桌面版
blip install agy           # CLI
```

两端共用用户级 `~/.gemini/config/hooks.json`。Blip 为每个配置和来源建立独立的命名组，按回调携带的会话目录区分客户端。桌面版可在 Settings → Customizations → Hooks 查看，agy 可在交互界面输入 `/hooks`。[Google 官方 Hook 文档](https://antigravity.google/docs/hooks)

默认来源分别为 `antigravity-app`（`~/.gemini/antigravity`）和 `agy`（`~/.gemini/antigravity-cli`）。已有相同目录的来源会沿用原 ID。自定义数据目录可运行 `blip install antigravity --source my-ide --home <数据目录>`。这里的目录应与客户端报告的会话目录所属的数据目录一致。

## 提醒条件

| 原生回调 | Blip 提醒 |
| --- | --- |
| `PreToolUse` 调用 `ask_question` | 需要回复，附问题和选项 |
| `PreToolUse` 调用 `ask_permission` | 工具已发起显式权限申请 |
| `PreToolUse` 调用 `run_command`，且 `BypassSandbox` 为 `true` | 命令申请在沙箱外运行，附客户端提供的操作摘要 |
| `Stop` 报告完全空闲并正常结束 | 本轮已结束，通知正文可附最后一条助手回复 |
| `Stop` 报告完全空闲并因错误、执行上限或终止步骤停下 | 运行遇到问题或已停下 |

例如，`git commit` 在沙箱内执行失败，助手申请改在沙箱外执行时，Blip 会推送一次“需要确认”。原生 Hook 报告的是这次授权申请，客户端仍按当前权限规则决定是否弹窗。已有授权或自动批准时也可能收到这条通知。[Google 权限设置](https://antigravity.google/docs/agent-settings)

普通命令仅因客户端策略触发的确认框、文件或 MCP 工具的隐式审批，以及 MCP 工具运行中发起的提问，缺少可单独识别的原生回调。其他停止钩子如果要求继续执行，本次 `Stop` 之后仍可能出现新活动。[Google 支持的事件](https://antigravity.google/docs/hooks#supported-events)

正常结束时，Blip 从当前会话的 `transcript.jsonl` 末尾提取本轮助手对用户可见的最终回复，保留换行，最多 4000 字节。各通知渠道仍有自身的长度限制。记录暂时不可读或本轮缺少最终回复时，Blip 仍会发送完成提醒。[Google Stop 回调](https://antigravity.google/docs/hooks#stop)

`PreInvocation` 提供区分连续轮次的步骤位置。Blip 结合来源、会话和工具调用 ID 或步骤位置去重。通知从 Hook 启用后开始采集，已经入队的通知可在后台重启后继续投递。

## 检查与卸载

```powershell
blip doctor --source antigravity-app
blip doctor --source agy
blip uninstall antigravity
blip uninstall agy
```

`doctor` 显示注册状态、能力范围和最近回调。卸载按来源移除对应的 Blip 回调组。使用自定义来源时可加 `--source <ID>`。
