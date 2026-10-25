# 兼容性与验证记录

首版验收目标为 Windows 的 Codex CLI 与桌面版。以下区分接口文档、已有实机证据与 Blip 本地测试；不将协议模拟称为所有 Codex 路径已经实测。

## Codex 事件来源

| 来源 | 能够提供的事实 | 首版处理 |
| --- | --- | --- |
| `PreToolUse` | 结构化提问工具准备执行 | 提取全部受限数量的问题和选项，按 tool_use_id 去重 |
| `PermissionRequest` | 准备发起审批申请 | 发送审批申请提醒，不宣称用户此刻仍在等待 |
| `PostToolUse` | 工具已经返回 | 不视为异步问题已获回答，不产生本工具的提醒 |
| `Stop` | Codex 准备停止，但其他 hook 仍可要求继续 | 不用作完成事实 |
| `notify` | 官方 `agent-turn-complete`，没有结果状态 | 不改写既有 notify 配置 |
| rollout `task_complete` | 当前已观察格式中的轮次结束记录 | 核对根会话、turn_id、root_turn_id 与已知异常状态，再入队 |

根会话需有 `thread_source: user`，或已识别的 CLI / VS Code / App Server 来源；明确子 agent 来源、父子会话标识冲突与未知来源不发完成提醒。历史文件按其真实目录递归查找，不按“今天”或“最近文件”猜会话，也不以正文问号、安静时间判断提问或完成。

## 已有实机证据

- 用户提供的 2026-10-21 交接：桌面版 `26.1013.40934`、内核 `0.163.0-alpha.10.2` 的 `request_user_input_async` 已通过 Pre/PostToolUse 捕获中文问题与选项。
- 同次文件权限申请 `request_permissions` 未触发 PermissionRequest。该阴性结论仅限所测路径，不能外推到全部审批；其通用 PreToolUse 覆盖也没有得到该次试验证明。
- 根会话实际 rollout 中观察到 `task_complete`，含 turn_id、root_turn_id、last_agent_message；中断记录为 `turn_aborted`。编辑消息可能产生同会话的额外文件，重启与回放必须去重。
- 2026-10-25 只读检查的本机 CLI 为 `0.163.0`，hooks 为 stable 且已启用；未安装 Blip 的真实 hook 来重新触发审批测试。

CLI 的实际审批、同步输入、各个网络/命令授权，以及真实失败结束仍需分别补实机验证。Blip 仅处理已知事件结构；未知格式不会猜测为完成。Codex 升级后应重新验证兼容性。

## 本地验收（2026-10-25）

- 四类渠道 × 三类事件通过本地 HTTP 服务验证；飞书固定签名向量、企业微信 2048 字节限制、Gotify 子路径和 token、固定 webhook 版本均有测试。
- HTTP 200 的业务失败、缺成功码、非法 JSON、超时、429/5xx、拒绝重定向与错误信息中的凭据隔离有测试。
- 并发重复入队、同时写回不同目的地收据、持久化重试预算、进程锁、读取位置恢复、半行、中断与子 agent 排除有测试。
- 原生 Windows 进程集成测试在中文和空格路径构建实际程序，通过 `cmd.exe /C` 执行生成的 hook 命令，并验证后台启动停止、三事件投递、重启补收与安装卸载。
- `go test ./...`、`go vet ./...` 通过。本机 `CGO_ENABLED=0` 且没有可用 C 编译器，无法执行 race 检测；CI 配置在具备编译器的 Linux 环境执行，但不据此声称 CI 已运行成功。
- 未向飞书、企业微信、Gotify 等真实服务发送消息，尚无真实渠道端到端结果。

## 一手资料

- [Codex Hooks](https://learn.chatgpt.com/docs/hooks)：事件、信任机制、工具覆盖、默认启用与 commandWindows。
- [Codex notifications](https://learn.chatgpt.com/docs/config-file/config-advanced#notifications)：notify 与外部完成回调。
- [Codex App Server](https://learn.chatgpt.com/docs/app-server)：托管客户端的明确请求与轮次状态；不能假设新实例能旁听现有桌面应用。
- [Codex 0.163.0 Windows command runner](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/hooks/src/engine/command_runner.rs)：cmd 的引号包装及 shell 选择。
- [飞书群机器人](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)、[企业微信群机器人](https://developer.work.weixin.qq.com/document/path/99110)、[Gotify 推送](https://gotify.net/docs/pushmsg)。
