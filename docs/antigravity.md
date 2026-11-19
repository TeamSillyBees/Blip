# Antigravity 与 agy CLI

Blip 只读监听 Antigravity 的本地会话摘要。本轮结束、助手提问或工具等待授权时，通知由已有的飞书、企业微信、Gotify 和 Webhook 渠道投递。

## 接入

按使用的客户端执行对应命令。两者都用时各执行一次。Windows 使用当前账户的管理员 PowerShell。

```powershell
blip install antigravity   # 桌面版
blip install agy           # agy CLI
```

安装命令启用对应来源并启动或重启后台监听。桌面版默认读取 `~/.gemini/antigravity`，CLI 默认读取 `~/.gemini/antigravity-cli`。已经用 `sources add` 添加过同一目录时，安装命令沿用原来源 ID 和进度。用 `blip status` 查看运行状态。

npm 版 `0.3.0` 使用 `sources add` 手动接入。可以在 `blip install --help` 中检查当前程序是否包含上述两个快捷命令。

使用自定义数据目录或单独的 `~/.gemini/antigravity-ide` 时，运行 `blip install antigravity --source my-ide --home <数据目录>`。目录应包含客户端的 `conversation_summaries.db`。

Windows 首次接入自动注册并启动服务。再次接入会更新服务副本、授予所需数据库文件的读取权限并重启。若客户端首次创建或重建数据库后服务提示读取失败，再执行同一安装命令即可刷新权限。[Windows 服务说明](windows-service.md)

这两种接入直接读取会话数据库。`doctor` 中的 `installation: not_required` 表示客户端无需安装 Hook。

## 提醒条件

| 在客户端观察到的状态 | Blip 提醒 |
| --- | --- |
| 根会话已完全空闲，执行器以 `NO_TOOL_CALL` 结束 | 本轮已结束 |
| 步骤正在等待提问、MCP elicitation 或阻塞式反馈 | 需要回复，提问附原生选项 |
| 步骤正在等待原生权限或审批交互，包括普通命令触发的授权 | 需要确认，附可用的工具名 |
| 执行器出错、达到步数或 token 限制，或被终止步骤停下 | 运行遇到问题或已停下，请查看 |

自动放行的工具调用、后台执行中的暂时空闲、子会话和已识别的用户取消均保持静默。用自然语言写在最终回复里的问题随本轮结束提醒处理。

首次启动会跳过已结束的历史轮次，并提醒摘要中仍在等待的请求。来源分别保存去重状态，队列写入成功后才记录新提醒。重启时能看到每个会话最新保存的状态；两次采集之间已经出现并消失的请求，或离线时被后续轮次覆盖的状态，可能漏收。该接入依赖客户端的本地数据库格式，验证版本和样本见[兼容性记录](compatibility.md)。
