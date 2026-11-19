# Antigravity 与 agy CLI

Blip 只读监听 Antigravity 的本地会话摘要。本轮结束、助手提问或工具等待授权时，通知由已有的飞书、企业微信、Gotify 和 Webhook 渠道投递。

## 接入

桌面 App 和 `agy` CLI 都使用 `antigravity` 类型。按使用的客户端添加来源，`--home` 指向包含 `conversation_summaries.db` 的数据目录。

```powershell
blip sources add --source antigravity-app --harness antigravity --home "$env:USERPROFILE\.gemini\antigravity"
blip sources add --source agy --harness antigravity --home "$env:USERPROFILE\.gemini\antigravity-cli"
blip restart
blip status
```

单独使用一个客户端时，只执行对应的 `sources add`。独立 IDE 使用 `~/.gemini/antigravity-ide` 时，可为该目录另加一个来源。自定义数据目录以客户端实际写入的位置为准。

无需安装 Hook。`blip doctor --source agy` 的 `installation` 为 `not_required`。如果客户端尚未创建数据库，`status` 会显示该来源的采集错误；数据库就绪后 Blip 自动重试。

使用 Windows 服务时，新增来源后重新安装服务，以更新对数据库及已有 `-wal`、`-shm` 文件的只读权限。若客户端重建文件后服务失去访问权，可在文件就绪后重新安装服务，或使用普通用户后台进程。[Windows 服务说明](windows-service.md)

## 提醒条件

| 在客户端观察到的状态 | Blip 提醒 |
| --- | --- |
| 根会话已完全空闲，执行器以 `NO_TOOL_CALL` 结束 | 本轮已结束 |
| 步骤正在等待提问、MCP elicitation 或阻塞式反馈 | 需要回复，提问附原生选项 |
| 步骤正在等待原生权限或审批交互，包括普通命令触发的授权 | 需要确认，附可用的工具名 |
| 执行器出错、达到步数或 token 限制，或被终止步骤停下 | 运行遇到问题或已停下，请查看 |

自动放行的工具调用、后台执行中的暂时空闲、子会话和已识别的用户取消均保持静默。用自然语言写在最终回复里的问题随本轮结束提醒处理。

首次启动会跳过已结束的历史轮次，并提醒摘要中仍在等待的请求。来源分别保存去重状态，队列写入成功后才记录新提醒。重启时能看到每个会话最新保存的状态；两次采集之间已经出现并消失的请求，或离线时被后续轮次覆盖的状态，可能漏收。该接入依赖客户端的本地数据库格式，验证版本和样本见[兼容性记录](compatibility.md)。
