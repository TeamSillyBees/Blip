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
| rollout `response_item/function_call` | 已识别的 `request_user_input(_async)` 调用及问题参数 | 用最近原生 turn context 关联轮次；与 hook 共用 call ID 去重，补收缺失回调 |

根会话需有 `thread_source: user`，或已识别的 CLI / VS Code / App Server 来源；明确子 agent 来源、父子会话标识冲突与未知来源不发完成提醒。历史文件按其真实目录递归查找，不按“今天”或“最近文件”猜会话，也不以正文问号、安静时间判断提问或完成。

## 已有实机证据

- 用户提供的 2026-10-21 交接：桌面版 `26.1013.40934`、内核 `0.163.0-alpha.10.2` 的 `request_user_input_async` 已通过 Pre/PostToolUse 捕获中文问题与选项。
- 同次文件权限申请 `request_permissions` 未触发 PermissionRequest。该阴性结论仅限所测路径，不能外推到全部审批；其通用 PreToolUse 覆盖也没有得到该次试验证明。
- 根会话实际 rollout 中观察到 `task_complete`，含 turn_id、root_turn_id、last_agent_message；中断记录为 `turn_aborted`。编辑消息可能产生同会话的额外文件，重启与回放必须去重。
- 2026-10-25 只读检查的本机 CLI 为 `0.163.0`，hooks 为 stable 且已启用；未安装 Blip 的真实 hook 来重新触发审批测试。

CLI 的实际审批、同步输入、各个网络/命令授权，以及真实失败结束仍需分别补实机验证。Blip 仅处理已知事件结构；未知格式不会猜测为完成。Codex 升级后应重新验证兼容性。

## 桌面版输入补收（2026-11-07，未发布）

- Windows 桌面内核 `0.167.0-alpha.3.2` 的 `hooks/list` 确认 Blip 输入 hook 已启用且已信任，但连续两次真实选择题均未进入 Blip。提问记录仍完整写入 rollout；不能用“完成通知正常”推断请求 hook 正常，也不能把早期版本的实测结论外推到当前版本。
- 新增结构化提问补收，适用于事件 v1/v2；只解析已识别的工具调用、原生 call ID 和非空问题。原生轮次来自 turn context / start 记录，不能关联时不猜测。首扫不推历史问题；旧游标按需回看原生上下文，不回退读取位置。
- Hook 与日志使用同一请求 ID 和通知 ID，首次快照保留，重启或双路到达不重复入队。异步工具的 `accepted` 返回不代表用户已答复；v2 的具体问题仍覆盖对应停止提醒。
- 脱敏真实样本与回归见 [Codex 桌面提问样本](../internal/codex/testdata/README.md)。同步提问仍是协议测试；没有为未知审批形状添加推断。
- Windows 服务实测发现来源身份查询在 LocalService 下因目录访问受限而暂停采集。修复为专门的目录元数据句柄，服务只获 home 的不继承属性/遍历权限；测试确认不能列出目录，身份查询仍可完成，且不扩大凭据文件读取权限。

## 本地验收（2026-10-25）

- 四类渠道 × 三类事件通过本地 HTTP 服务验证；飞书固定签名向量、企业微信 2048 字节限制、Gotify 子路径和 token、固定 webhook 版本均有测试。
- HTTP 200 的业务失败、缺成功码、非法 JSON、超时、429/5xx、拒绝重定向与错误信息中的凭据隔离有测试。
- 并发重复入队、同时写回不同目的地收据、持久化重试预算、进程锁、读取位置恢复、半行、中断与子 agent 排除有测试。
- 原生 Windows 进程集成测试在中文和空格路径构建实际程序，通过 `cmd.exe /C` 执行生成的 hook 命令，并验证后台启动停止、三事件投递、重启补收与安装卸载。
- `go test ./...`、`go vet ./...` 通过。本机 `CGO_ENABLED=0` 且没有可用 C 编译器，无法执行 race 检测；CI 配置在具备编译器的 Linux 环境执行，但不据此声称 CI 已运行成功。
- 未向飞书、企业微信、Gotify 等真实服务发送消息，尚无真实渠道端到端结果。

## 可靠性补充验收（2026-11-06）

- 首次扫描固定已观察到的文件长度；扫描期间有新写入的文件按基线时间筛选。回归覆盖快照之后追加记录与初扫期间产生完成事件，避免将新事件跳过为历史记录。
- 保存会话头部和读取位置前 512 字节的指纹，回归覆盖重启后同路径等长、更长文件替换。旧读取位置记录可升级；有限指纹不保证发现保留头部及读取位置附近内容的任意中段修改。
- 空游标、负偏移和非法指纹会被识别为损坏并保留原文件。多来源实现前会导致启动失败；现在只暂停对应来源，公共 worker 以 `ready: true, degraded: true` 继续投递。
- 损坏队列、冲突事件标识和无效收据被隔离，健康通知继续处理；回归验证原始字节保留、重启后不重建已隔离事件的收据、最近送达或失败的积压通知拥有完整的 7 天保留期。
- 并发读取与清理测试在 Windows 复现文件共享冲突；共享读锁与独占变更锁修复后通过。长轮询及等待网络请求期间停止的测试通过，取消发送不消耗重试次数。
- 配置测试覆盖非法 URL 端口、token 空白或控制字符、重复及保留请求头；HTTP 测试覆盖小响应后的连接复用、响应正文停滞时有界清理，并保持原有成功与重试判定。
- 状态查询回归覆盖停止后的旧就绪记录、各目的地诊断、隔离计数，以及旧通知晚重试时按实际尝试时间选择最新错误。
- `go test ./...`、`go vet ./...` 和 Windows 构建通过。测试使用临时 Codex home 与本机 HTTP 服务，没有改写真实 Codex 配置或向真实通知渠道发送消息；实际审批覆盖与渠道端到端验证的缺口不变。本机仍不具备 race 检测所需的 C 编译器。

## npm 与交互配置验收（2026-11-06）

- npm 启动器使用原生平台可选依赖，Windows/Linux/macOS 的 x64、ARM64 共六个目标交叉构建通过；原生可执行文件和启动器的 tarball 执行权限经过校正，包内包含依赖许可文本。
- Windows x64 从本地 tarball、空 npm 缓存进行离线安装验证通过，临时全局 prefix 中的 `blip.cmd`、版本、帮助、含空格的配置路径、退出码与缺少原生依赖的提示均有测试。其他平台的构建结果不等于 Codex 接入已实机验收。
- Cobra 命令测试覆盖 `--config` 前后位置、别名帮助、无效参数及非交互输入拒绝；旧 Windows 进程集成测试继续通过。
- 配置向导测试覆盖四类渠道、多个目的地、请求头管理、遮蔽凭据、原始字段保留、取消不写入、并发编辑冲突和备份失败。
- Windows 终端实测完成 Webhook 配置保存、已有配置编辑、遮蔽输入和 Ctrl+C 取消；取消后原文件不变且未产生备份。普通终端与显式无障碍模式均已操作验证，`TERM=dumb` 自动切换模式另有回归覆盖，保留摘要及取消检查。
- `go test ./...`、`go vet ./...`、Node 启动器与归档测试通过。CI 已加入各运行平台的 npm 打包与安装检查，但未据此声称远端 CI 已执行。未向真实通知渠道发送消息。
- `@teamsillybees/blip` 及六个平台原生包的 `0.2.0-dev.0` 已发布到公共 npm，`next` 标签指向该版本，公共注册表中的完整性哈希与本地发布清单一致。npm 首次发布时还设置了 `latest`；删除该标签的请求返回 HTTP 400，当前仍为预发行版。
- Windows x64 在临时全局 prefix、空 npm 缓存和空 npm 配置下，从公共注册表安装 `@teamsillybees/blip@next` 通过；验证了平台依赖自动选择、`blip.cmd version` 和 `blip.cmd configure --help`。未修改真实 Codex 配置或向真实通知渠道发送消息。

## 设备名称与配置简化（2026-11-06，0.2.0-dev.1）

- 首次配置默认填写系统 hostname，可自定义并保存 `device_name`；旧配置缺少该字段时仍可使用。回归覆盖默认值、改名、中文名称、取消不写入、保留其他字段及原子保存。
- URL 在普通终端和无障碍模式下明文显示、预填并可编辑；Token、签名密钥及请求头值继续遮蔽。实测默认值可用回车接受、Gotify 配置保存及取消编辑，取消后配置字节不变且不产生备份。
- 已删除渠道配置的环境变量展开逻辑及相关提示。URL、Token、签名密钥和请求头中的旧引用会被拒绝，不会读取环境变量后继续发送；旧配置需改填实际值。
- 本机 HTTP 测试验证四类渠道的设备标识，其中 Gotify 标题和正文均含设备名；进程集成测试覆盖完成、审批、输入、手动测试及重启恢复的设备名传播。重试保留事件入队时的名称，旧队列缺少名称时在发送阶段补用当前配置。
- `go test ./...`、`go vet ./...`、Node 测试及 Windows x64 本地 npm 打包/离线安装通过。`0.2.0-dev.1` 尚未发布到公共 npm；没有修改真实 Codex 配置或发送真实通知。

## Windows 服务（2026-11-06，0.2.0-dev.2）

- 新增 `service install/start/stop/status/uninstall`，注册为 LocalService 下的独立 Windows 服务，设置自动延迟启动，保存管理员保护的程序副本及安装时解析的配置、Codex 和队列路径。
- Windows 原生测试覆盖中文与空格路径、启动参数转义、服务归属检查、受保护目录 ACL、配置原子替换后的读取权限、卸载时只清理本服务身份的授权，以及非管理员查询未安装服务。
- 服务运行测试覆盖初始化后报告就绪、启动失败、停止与系统关机处理；CLI 回归覆盖服务环境缺少 `APPDATA` 时使用显式配置路径，以及启动诊断不记录配置中的敏感内容。
- `go test ./...`、`go vet ./...`、Node 测试、六个平台的 npm 构建和 Windows x64 本地 npm 安装验证通过。测试使用临时配置，没有发送真实通知。
- 当前终端不是管理员，真实 SCM 注册、启动、卸载及重启后的自动启动尚未实测。已提供需管理员权限并显式设置 `BLIP_TEST_WINDOWS_SERVICE=1` 才运行的 `TestWindowsServiceSCMLifecycle` 集成测试。`0.2.0-dev.2` 尚未发布到公共 npm。

## 精简通知模板（2026-11-06，0.2.0-dev.3）

- 默认标题仅为“完成／需要输入 · 项目名”，正文为对话名；提问附问题和字母选项，审批附确认提示。Gotify 的正文不重复标题，飞书与企业微信在文本首行显示标题；结构化 webhook 保留原字段并新增可选 `session_title`。
- 本机只读检查确认 `session_index.jsonl` 使用 `id`、`thread_name`、`updated_at`，重命名会产生后续同 ID 记录。名称解析使用最后一条完整有效记录；不将状态数据库中的初始请求文本当作当前对话名。这是已观察的本地格式，Codex 升级后需重新验证。
- 回归覆盖重命名、文件替换、损坏行与半行、Unicode、名称缺省、发送时补齐旧队列、已入队名称保留，以及完成、输入、审批和手动测试通知的实际进程传递。服务只为已存在的名称索引文件增加读取权限，卸载保留索引和其他身份的权限。
- `go test ./...`、`go vet ./...`、Node 测试、六个平台 npm 构建及 Windows x64 本地 npm 安装通过。`0.2.0-dev.3` 尚未发布；未向真实通知渠道发送消息，真实 SCM 生命周期验证边界不变。

## 设备与时间补充（2026-11-06，0.2.0-dev.4）

- 按后续确认，标题恢复设备名，正文在对话名下方显示事件时间。使用运行 Blip 的计算机本地时区和明确的 UTC 偏移；重试保留事件时间。
- 格式及渠道回归覆盖设备缺省、长 Unicode 名称、固定时区转换和三类通知的标题与正文。`go test ./...`、`go vet ./...`、npm 构建及本地安装验证通过。`0.2.0-dev.4` 尚未发布。

## 多来源基础（2026-11-06，未发布）

- v1 配置读取不改写文件，默认来源保留 `event_version: 1`、事件 ID、旧游标和安装记录；显式迁移保存备份与已解析 home。v2 必须包含 `sources`，空数组不会恢复默认 Codex。
- 多个 Codex home 使用独立安装记录、游标、诊断和事件命名空间；测试覆盖同 session/turn/request ID 的来源隔离、停用与删除后旧队列继续发送、同一次事件跨协议升级保持首次快照。
- 绑定记录在来源移除及卸载后保留，拒绝 ID 改绑 home；测试覆盖损坏绑定、旧安装记录冲突、配置备份、按保存的位置卸载已删除来源。
- hook 输入限制 1 MiB，并在 3 秒共享预算内读取、解析与入队；EOF 等待有取消路径。通知失败返回中性空 stdout，错误码保存在本地诊断，不输出原始载荷。
- worker 按来源独立采集；损坏 checkpoint 暂停该来源，不重置基线。测试同时验证健康来源采集、未知 adapter 降级及已删除 adapter 的待发记录投递。
- v2 事件和公共模板支持 `attention_required`。Codex v2 安装 Stop、Interrupt、UserPromptSubmit 及工具回调，worker 在最多 1 秒的候选核验后发出中性停止提醒；v1 不增加这些回调。支持情况见下一节，其他 harness 此时尚未注册。
- Windows 服务安装元数据 v2 保存每个来源的绝对位置与必需读取路径，保留 v1 清理兼容；测试覆盖多 home、空来源、不授权整个 agent home，以及卸载按保存的授权清理。真实 SCM 生命周期及真实外部通知的验证边界保持不变。

## Codex v2 接手关联（2026-11-06，未发布）

- Stop 候选持久化后立即返回，不在同步 hook 中等待后续执行。重复回调和重启不延长原始核验期限；到期仅提示停止信号，未冒称正常完成。
- 明确的 Interrupt / rollout turn_aborted 抑制提醒。已有输入或审批请求时合并泛化停止；异步输入的工具返回不当作用户回答。未观察到取消信号时仍可能发中性提醒。
- 已通知停止后，后续明确工具执行或用户输入可开启新的接手序号；重复及迟到开始记录不能推进序号。迟到完成保持首次快照，正常结束后的维护工具不产生第二次完成提醒。
- 模拟 rollout 的 turn_failed/task_failed、task_complete 的明确失败状态可产生 error 提醒；这些失败形状尚无新增实机样本，不宣称当前所有 Codex 异常路径都能覆盖。
- 回归覆盖候选期限、取消、子 agent、请求去重、继续执行、迟到日志、并发最终信号、入队后关联状态保存前中断，以及新旧状态文件同时存在时的恢复。状态损坏暂停来源并保留原文件。
- 默认 Codex 第一段接手沿用旧完成 ID；旧 v1 已入队完成的快照会阻止升级后的停止候选改变其结果。没有执行真实 Codex 会话或真实通知渠道验收；本地时延 P95 目标尚未作为全链路承诺。

## Claude Code（2026-11-06，未发布）

- 新增独立安装器与解析器。安装到指定 home 的 settings.json，使用真实程序 + args，不生成 shell wrapper；保留其他 hooks、权限与信任设置，用户修改过的已登记条目报告冲突。
- 官方 CLI 2.1.318 Windows x64 在临时 home、本机模型服务下实际触发 UserPromptSubmit 与 Stop；安装命令在中文、空格、`$`、`&`、单引号路径上运行，worker 可恢复并通知持久候选。[样本与重跑方式](../internal/agents/testdata/claude-code/README.md)。
- Stop 按 signal-only 提醒；StopFailure、AskUserQuestion、Elicitation、PermissionRequest 采用官方协议样本验证，尚未将这些交互分支标为客户端实测。没有调用真实模型或外部通知渠道。
- 身份优先使用 prompt_id 与原生 request ID；旧 payload 的持久序号和内容合并是尽力去重。未读取 transcript，不新增 Windows 服务读取 Claude home 的权限。使用方法与局限见 [Claude Code 来源](claude-code.md)。

## Hermes（2026-11-07，未发布）

- 根据本机固定源码 `4d043038` 接入 profile 的 config.yaml shell hooks。YAML 节点编辑保留注释与其他配置，清单按来源保存，不自动修改 consent allowlist。
- 读取真实 shell envelope 的 extra 字段；区分完成、自动异常、中断与普通进程退出。采用实际 on_human_input_request，不将可能自动处理的 pre_approval_request 当作人工等待。
- 真实 Python 3.14.7 / Windows shell runner 测试验证 YAML 解析、分发、stdin、空 stdout、特殊字符路径与持久入队；clarify 和 human-input context manager 生成实际请求/解除回调。结束和子任务事件使用固定源码 kwargs 通过原版分发器测试，未启动真实模型会话。
- 回归覆盖多次重启、重复请求、并行等待、迟到已回答回调、取消与子会话身份。子会话过滤要求先收到 subagent_start；中途安装可能缺失关联。详见 [Hermes 来源](hermes.md) 与[样本](../internal/agents/testdata/hermes/README.md)。

## DSH（2026-11-07，未发布）

- 独立 adapter 和内容寻址的本地 Cordis 插件已接入；安装具体 profile 的 patch，保留其他插件与 YAML 表达式，按清单精准更新和卸载。
- 原版 DSH `0.2.1-rc.2` 的 Cordis Loader / SessionStore 与真实原生进程测试通过，13 次回调覆盖输入、审批、正常结束、异常、取消、限时未答及子会话过滤；registry、结束原因和答案由测试控制，未启动真实模型。
- 重复回放和 adapter 重启不重复通知；限时未答不解除请求，已被具体请求覆盖的停止在迟到重放后不追加完成提醒。继续执行后可开启新接手。
- 用户尚未安装 WorkBuddy，暂不注册 adapter；不以 CodeBuddy CLI 测试代替 WorkBuddy 桌面验证。剩余样本和验收要求见 [WorkBuddy 接入门槛](workbuddy.md)。DSH 使用与限制见 [DSH 来源](dsh.md)。

## 来源故障与本地延迟（2026-11-07，未发布）

- 纯 hook 来源的错误参与全局降级判断；损坏的候选、会话序号和读取位置保存来源故障标记。回归验证暂停整个来源、保留损坏字节、拒绝以删除文件恢复，以及原文件修复后的重新校验。普通回调与并发完成顺序不会丢失最近一次通知时间。
- 队列额外保存本地接收时间、可用的原生信号时间，以及按目的地记录的首次调度开始。重试、重启与旧事件快照保持兼容；这些字段不进入 webhook 事件。`status` 按来源与目的地报告 P95、样本数和时钟异常数。
- Windows 本机使用 4 个来源、默认 1 秒采集配置、20 轮共 80 个协议回调，经真实持久队列及本机 HTTP 服务测得如下结果。每轮投递结束后才发下一轮；Codex、Claude 均包含 1 秒停止核验。这是单次本地测试，不包含真实厂商推送、手机显示或模型运行时间。

| 来源 | 样本数 | 接收至首次调度 P95 | 接收至本机 HTTP 到达 P95 |
| --- | ---: | ---: | ---: |
| Codex v2 Stop | 20 | 1565 ms | 1566 ms |
| Claude Code Stop | 20 | 1592 ms | 1594 ms |
| Hermes 结束 | 20 | 257 ms | 259 ms |
| DSH 结束 | 20 | 516 ms | 518 ms |

- 可复跑：设置 `BLIP_TEST_LATENCY=1`，运行 `go test ./internal/worker -run '^TestMultiHarnessLocalLatency$' -v -count=1`。当前记录满足该场景下本地 P95 ≤ 2 秒的目标；离线、积压和渠道限流不属于此测试条件。历史队列统计不自动排除这些延迟，也不根据缺失字段推算旧数据。

## 一手资料

- [Codex Hooks](https://learn.chatgpt.com/docs/hooks)：事件、信任机制、工具覆盖、默认启用与 commandWindows。
- [Codex notifications](https://learn.chatgpt.com/docs/config-file/config-advanced#notifications)：notify 与外部完成回调。
- [Codex App Server](https://learn.chatgpt.com/docs/app-server)：托管客户端的明确请求与轮次状态；不能假设新实例能旁听现有桌面应用。
- [Codex 0.163.0 Windows command runner](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/hooks/src/engine/command_runner.rs)：cmd 的引号包装及 shell 选择。
- [飞书群机器人](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)、[企业微信群机器人](https://developer.work.weixin.qq.com/document/path/99110)、[Gotify 推送](https://gotify.net/docs/pushmsg)。
