# Antigravity 桌面版与 agy CLI

Blip 通过客户端的原生 Hook 接收提问、显式权限申请和结束事件。回调把通知写入本地持久队列，后台进程负责投递到已配置的渠道。

## 安装与升级

Windows 使用当前账户的管理员 PowerShell。按实际使用的客户端执行命令，两端都用时各执行一次。从 `0.3.0` 升级到原生 Hook 版时也需要执行一次，以注册新回调并更新后台进程。

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
| `Stop` 报告完全空闲并正常结束 | 本轮已结束 |
| `Stop` 报告完全空闲并因错误、执行上限或终止步骤停下 | 运行遇到问题或已停下 |

普通命令、文件或 MCP 工具的隐式审批，以及工具运行中由 MCP 服务端发起的提问，都缺少对应的原生 Hook。Blip 无法为这些弹窗发出提醒。如果其他停止钩子要求继续执行，本次 `Stop` 之后仍可能出现新活动。[Google 支持的事件](https://antigravity.google/docs/hooks#supported-events)

`PreInvocation` 提供区分连续轮次的步骤位置。Blip 结合来源、会话和工具调用 ID 或步骤位置去重。通知从 Hook 启用后开始采集，已经入队的通知可在后台重启后继续投递。

## 检查与卸载

```powershell
blip doctor --source antigravity-app
blip doctor --source agy
blip uninstall antigravity
blip uninstall agy
```

`doctor` 显示注册状态、能力范围和最近回调。卸载按来源移除对应的 Blip 回调组。使用自定义来源时可加 `--source <ID>`。
