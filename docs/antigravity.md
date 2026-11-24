# Antigravity 桌面版与 agy CLI

Blip 提醒你回到自己打开的主对话。主任务结束、异常停下，或者团队中有一步正在等你回复或确认时，提醒会进入本地持久队列。

## 安装与升级

Windows 使用当前账户的管理员 PowerShell。升级 Blip 后，按实际使用的客户端重新运行对应命令，更新 Hook 程序、Windows 服务副本和数据库文件的读取权限。两端都用时各执行一次。

```powershell
blip install antigravity   # 桌面版
blip install agy           # CLI
```

两端共用用户级 `~/.gemini/config/hooks.json`。Blip 为每个配置和来源建立独立的命名组，按回调携带的会话目录区分客户端。桌面版可在 Settings → Customizations → Hooks 查看，agy 可在交互界面输入 `/hooks`。[Google 官方 Hook 文档](https://antigravity.google/docs/hooks)

默认来源分别为 `antigravity-app`（`~/.gemini/antigravity`）和 `agy`（`~/.gemini/antigravity-cli`）。已有相同目录的来源会沿用原 ID。自定义数据目录可运行 `blip install antigravity --source my-ide --home <数据目录>`。这里的目录应与客户端报告的会话目录所属的数据目录一致。

## 提醒条件

Antigravity 的每个代理都有自己的会话 ID。Teamwork 的访谈员、调度器、调查员和审核员可以嵌套多层。它们完成一项工作后，会在自己的会话结束本轮，再把结果交给上级。客户端也会把需要用户授权的子任务显示在主界面。[Teamwork](https://antigravity.google/docs/teamwork/) · [子代理与权限](https://antigravity.google/docs/subagents)

| 客户端发生的事 | Blip 的行为 |
| --- | --- |
| 主对话完全空闲，正常结束本轮 | 提醒一次，附本轮最后一条可见回复。访谈问题、待审方案也可能通过这样的普通回复交给你 |
| 主对话完全空闲，因错误、执行上限或终止步骤停下 | 提醒回到主对话查看 |
| 主对话仍有后台工作，或子代理结束、出错、给上级交报告 | 保持安静 |
| 主对话或子代理有正在等待用户的提问、权限申请、阻塞式审阅 | 以主对话名称提醒，每个独立请求一次。结构化问题会附上选项 |
| 已自动放行的命令、进度播报、用户主动取消 | 保持安静 |

**结束提醒**来自原生 `Stop`。Blip 检查 `fullyIdle` 和终止原因，再用当前客户端 `conversation_summaries.db` 中的 `parent_conversation_id` 和 `nesting_depth` 确认顶层身份。摘要尚未写入或暂时不可读时，可用当前会话记录中第 0 步的显式用户输入确认身份。两处都无法确认时，会跳过这次结束提醒。最后回复取自本轮 `transcript.jsonl`，保留换行，最多 4000 字节。其他 Stop 钩子仍能要求客户端继续运行。[Google Hook 文档](https://antigravity.google/docs/hooks#stop)

**提问和审批**以客户端已经写入的等待步骤为准。Blip 从 `PreInvocation` 开始跟踪本次执行，也能从已有的 `PreToolUse` 回调中途接入。后台读取该会话摘要中的 `WAITING` 状态和请求类型，可识别结构化提问、命令和文件审批、MCP 交互以及阻塞式审阅。沙箱外命令也遵循同一规则，客户端自动放行时保持安静。极短、在两次读取之间已解决的请求可能被跳过。

子代理确实需要人工处理时，Blip 沿父会话逐级找到主对话，使用主对话的名称、会话 ID 和项目。去重仍区分发起请求的子代理，两个子任务各自需要审批时能够分别收到。父子关系暂时缺失时，仍在等待的请求会在摘要更新后重新检查。

通知优先显示客户端保存的主对话标题，入队时缺失会在投递前再查询一次。标题仍为空时，正文显示“项目名 对话”。

Windows 服务直接打开已配置的会话目录。安装程序为该服务登记 `conversation_summaries.db`、`-wal` 和 `-shm` 三个文件的只读权限。客户端首次创建数据库或重建临时文件后，开始跟踪本轮会话的用户回调会按安装记录恢复权限。

## 检查与卸载

```powershell
blip doctor --source antigravity-app
blip doctor --source agy
blip uninstall antigravity
blip uninstall agy
```

`doctor` 显示注册状态、能力范围和最近回调。卸载按来源移除对应的 Blip 回调组。使用自定义来源时可加 `--source <ID>`。
