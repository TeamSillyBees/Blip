# Blip 多 harness 事件接入设计

状态：实施中，公共来源基础已落地；各 harness 的扩展按下述批次推进。日期：2026-11-06。

基线：`0.2.0-dev.4` 的 Codex 接入、本地持久队列和四类通知渠道。已实现来源 registry、v1/v2 配置和事件读取、逐来源状态及故障隔离、来源命令与服务权限清单，以及 Codex v2 的持久停止候选和接手序号。后续 harness 能力仍须逐项验收；当前支持情况以 [README](../README.md) 和 [兼容性记录](compatibility.md) 为准。

Blip 的目标是在该由用户接手时及时提醒，减少 harness 停下来之后等待用户的时间：本轮结束后，用户可以查看结果、继续当前工作或安排下一个任务；需要回答、授权或处理异常时，用户可以尽快响应。通知继续发送到飞书、企业微信、Gotify 和自定义 webhook。各 harness 的 hook 名称和数据格式只是识别这些时机的手段。

## 目标与约束

- 接入多个 harness，并允许同一 harness 的多个 home 或 profile 同时存在。
- 用户层只区分“本轮结束，可以安排下一步”和“需要你回复或处理”两个场景；现有三类协议事件继续使用，v2 补充结果未知的停止信号和自动异常结束提醒。
- 正常结束不代表业务任务已经验收成功。已知自动停止但结果不明时及时提醒查看；自动异常结束提示原因，不标为完成。已识别的用户主动取消、子 agent 结束和工具间隙不触发根会话接手提醒。
- 回调快速持久入队，后台按目的地独立发送、超时和有限重试，保持尽力去重的至少一次语义。
- 保留现有 Codex 配置、事件标识、读取位置、待发通知和投递收据。
- 通知保留项目名、设备名、对话名和带时区偏移的事件时间，并说明用户接下来可以做什么。v1 样式保持兼容；v2 按结束、回复、确认或查看原因选择文案，不把未知结果写成成功。
- 所有来源的接手提醒继续发送到所有启用的目的地，本阶段不增加分流规则。

本阶段不建设 agent 启动器、终端管理、远程审批、通用 busy/waiting/idle 状态机、动态插件运行时或第二套原始事件队列。推送出口就是已有渠道，不扩展手机 App 或推送基础设施。

## 从 Orca 采用的设计

参考源码固定为 Orca [`26741256dcccecac4007f64476a17825fdd3858b`](https://github.com/stablyai/orca/tree/26741256dcccecac4007f64476a17825fdd3858b)。采用其职责边界，代码独立实现。

| Orca 的做法 | Blip 的对应决策 |
| --- | --- |
| 各 harness 有独立安装器、脚本或插件，并分别归一化事件 | 建立内置 adapter registry；仅对已验证相同的字段与协议共享辅助函数 |
| 区分事件来源、原生会话和运行实例 | 使用稳定的 source ID 与原生 session、turn、request ID；身份不依赖显示名、模型名或当前工作目录 |
| 将平台调用与事件语义分开 | 优先调用原生 Blip 程序；由安装器适配 argv、cmd、PowerShell 或 POSIX shell |
| 保留不同版本和不同运行方式的差异 | 每项能力记录事件来源、版本、平台、客户端形态与验证状态 |
| 精确维护自己安装的配置 | 按 source 保存安装清单，合并写入、备份、幂等更新和精准卸载 |

Orca 的 [provider 分派](https://github.com/stablyai/orca/blob/26741256dcccecac4007f64476a17825fdd3858b/src/shared/agent-hook-listener/provider-dispatch.ts) 表明，Claude 风格的配置结构并不构成统一事件语义。其 [事件归一化](https://github.com/stablyai/orca/blob/26741256dcccecac4007f64476a17825fdd3858b/src/shared/agent-hook-listener/normalization/normalize-hook-payload.ts) 还处理会话归属、子任务和历史事件；Blip 提取识别用户接手机会所需的部分，不要求不同 harness 提供同名 hook。

独立 `.cmd` 是一种运行环境适配方式，不是多 harness 的共同协议。Orca 的 [Windows 原生 hook](https://github.com/stablyai/orca/blob/26741256dcccecac4007f64476a17825fdd3858b/native/windows-agent-hook/src/main.rs) 明确指出 cmd/curl 启动成本和跨 shell 调用差异。Blip 已有 Go 原生程序，继续以它作为默认 hook 入口。

## 数据流与模块边界

```text
Codex hooks / rollout ──→ Codex adapter ────┐
Claude hooks ──────────→ Claude adapter ───┤
Hermes hooks / plugin ─→ Hermes adapter ───┼→ Event → 本地持久队列
DSH hooks / plugin ────→ DSH adapter ──────┤                ↓
WorkBuddy hooks ───────→ WorkBuddy adapter ┘       各目的地独立发送
                                                         ↓
                                             飞书 / 企业微信 / Gotify / webhook
```

规范化后的 `Event` 是通知持久化边界。adapter 首先识别“本轮交回用户”或“请求用户响应”的信号，再根据已有结果选择文案。收到明确的结束或输入请求就及时入队；结果未知不阻止中性提醒，但不能标成成功完成。早期 Stop 的待核验信息只保存为 adapter 的最小关联状态，尚不创建通知收据，也不保存为第二套原始载荷队列。

collector 用于补收回调、识别取消与失败，或补充明确的结束结果。已知可能被其他 hook 延续的停止信号采用短且有界的核验；仍无法确定结果时，可按本节后述的停止信号策略发出“需要查看”提醒，不无限等待成功证据。确需关联开始、停止和继续时，状态由 adapter 按 source 保存，不上升为所有 harness 共用的活动状态机。

建议新增 `internal/agents` 承载来源解析、registry 和小型接口。保留现有 `internal/codex` 实现，先通过注册接入公共层；新增 harness 可放在 `internal/agents/claude` 等目录，不为目录统一迁移已有解析代码。

| 职责 | 输入与输出 | 边界 |
| --- | --- | --- |
| 接入检测 | 来源配置 → 安装位置、版本、客户端形态、能力报告 | 检测不启动 agent，不自动修改外部配置；检测不到版本时标为未知 |
| 安装管理 | 来源、原生程序路径 → 变更计划、安装清单 | 保留其他 hooks、权限与信任设置；失败保留可恢复备份 |
| Hook 解析 | 有界原始输入、来源上下文 → 零个或多个 Event | 识别事件、身份和结果；忽略不相关事件，拒绝损坏载荷 |
| Hook 响应 | 执行结果 → harness 允许的 stdout、stderr、退出码 | 与 Event 分离，不生成 approve、deny 或继续执行指令 |
| Collector，可选 | 来源状态、结构化记录 → 已确认 Event | 独立读取位置、基线与错误恢复；入队成功后才推进消费位置 |
| TitleResolver，可选 | 来源、session ID → 可选对话名 | 查询有界、允许失败；不能阻止通知入队或投递 |

优先使用小接口和可选能力，避免每个 adapter 实现没有意义的空方法。事件身份规则属于 adapter；队列锁、投递收据、重试和渠道响应判断属于公共层。

## 来源配置与身份

### 字段含义

| 字段 | 含义 | 示例 |
| --- | --- | --- |
| 配置 `sources[].id` / 事件 `source_id` | Blip 中稳定的接入实例 ID | `codex-default`、`hermes-work` |
| 配置 `harness` / 事件 `agent` | 实际执行框架 | `codex`、`claude-code`、`hermes`、`dsh`、`workbuddy` |
| 配置 `event_version` | 该来源新通知使用的事件协议版本 | 旧默认 Codex 保持 `1`；启用完整接手提醒使用 `2` |
| 事件 `source` | 具体采集途径 | `codex/hooks`、`codex/rollout`、`hermes/shell-hooks` |
| 原生 `session_id`、`turn_id`、`request_id` | harness 提供的身份 | 原样保留，不借用终端标题或项目路径代替 |

模型提供方不决定 adapter。例如 Claude Code 使用 DeepSeek 模型时仍使用 `claude-code`；只有实际 DSH 运行时才使用 `dsh`。

配置 v2 的来源部分示例，实施后与现有设备、运行参数和目的地字段组成完整配置：

```json
{
  "schema_version": 2,
  "sources": [
    {
      "id": "codex-default",
      "harness": "codex",
      "event_version": 2,
      "enabled": true,
      "home": "C:/Users/Alice/.codex"
    },
    {
      "id": "claude-main",
      "harness": "claude-code",
      "event_version": 2,
      "enabled": true,
      "home": "C:/Users/Alice/.claude"
    }
  ]
}
```

`home` 是安装和读取的定位信息，不是事件去重依据。不同工具的 profile 定位由 adapter 解析，新增 profile 字段以实际接入需要为准，不预设所有工具共用目录结构。

来源管理规则：

- ID 创建后保持稳定，并限制为可安全用作目录名的 ASCII 字符；不能包含路径分隔符或 `.`、`..` 等路径段。
- 同一实际运行环境中，同一 harness 的同一个规范化配置位置不能重复注册。检查路径别名，避免同一会话被两套 hooks 重复采集。
- 修改显示名称不改变身份。已安装来源的 home/profile 绑定发生变化时，拒绝静默沿用旧游标和安装清单；先显式解除原绑定或完成经过核验的迁移。
- 来源停用后不采集新事件，残留 hook 返回该 harness 的中性响应；已经入队的通知继续按既有收据投递。
- 停用是暂停采集，不清空读取位置。collector 型来源重新启用后按原 checkpoint 补收停用期间的持久日志增量；纯 hook 来源无法补收被忽略的回调。该差异须进入能力说明，不能承诺停用期间所有事件都会丢弃或都会补收。
- 配置 v2 必须显式提供 `sources` 数组；`sources: []` 表示不采集任何来源，但继续处理已入队通知，不隐式恢复默认 Codex 来源。只有读取旧配置 v1 时才建立兼容映射。
- 来源删除或 adapter 不可用时，已入队事件仍可验证并投递；只放弃依赖该来源的可选补充查询。事件存储校验不能依赖当前启用的 registry。
- 删除来源配置不删除安装清单、身份绑定和历史状态。卸载可凭持久安装清单定位旧配置，不要求来源仍存在于 `sources` 中；保留已使用 ID 的记录，防止旧回调落入另一个来源。

### 去重规则

新来源的事件 ID 采用带版本的、无歧义编码后的字段组合计算 SHA-256，至少包含 source ID、harness、接手机会类型与 adapter 提供的身份。长度前缀或规范 JSON 数组均可，不能简单拼接可能含分隔符的原始字符串。

回合结束提醒以 session＋turn 为主要身份，`turn_completed` 与没有被明确继续执行分隔的停止、异常查看提醒共用一次接手身份，不按最终文案或协议 kind 分开生成 ID。请求优先按 session＋原生 request/tool-use ID 去重；同一次等待已产生具体提问或审批提醒时，不再追加泛化停止提醒。存在 hook、插件或日志多条采集路径时，同一接手机会必须导出同一个 ID。

首次持久化的通知快照保持不变。停止提醒后才到达的完成证据，若中间没有明确继续执行，只补本地诊断，不再发送第二条通知，也不以不同 payload 覆盖已入队事件。adapter 的关联及首次快照选择必须与持久化协调，避免并发回调将同一次接手误判为队列冲突；不相关的 ID 冲突仍按原规则处理。

若停止信号提醒发出后，harness 明确恢复执行，则之后再次交回用户是新的接手机会，即使原生 turn ID 没变也应重新提醒。adapter 持久记录该回合的接手序号，只有明确恢复执行才能推进；重复日志或任务结束后的维护动作不能推进。第一段沿用既有身份，后续段加入该序号，避免一条早期停止提醒把最终结束永久去重掉。

缺少原生 turn/request ID 时，可采用稳定消息 ID、日志记录身份，或 adapter 持久记录的回合序号。只有低粒度信号时允许有限时间窗口的尽力合并，并明确重复风险，不能因没有 Codex 式 turn ID 而完全丢弃可用提醒。随机 UUID 和接收时间不能冒充精确可重放的轮次身份；来源和会话归属仍须可靠。

## 统一事件与协议演进

### 用户接手时机

核心问题是“现在是否该提醒用户回来”，而不是“这个 harness 的 hook 是否叫 Stop、PermissionRequest 或 PreToolUse”。用户层只需两个场景：

| 用户场景 | 提醒时机 | 用户下一步 |
| --- | --- | --- |
| 本轮结束，可以安排下一步 | harness 报告本轮回复结束或主动交回控制 | 查看结果，继续当前工作或开启下一个任务 |
| 需要你回复或处理 | harness 提问、申请授权，或因异常停下需要处理 | 回答问题、确认操作、检查原因或重试 |

“正常结束”只表示本轮回复结束，不是对任务完成质量的验收。异步提问即使没有让模型完全停止，也属于用户应及时知道的响应请求。正文提问如果伴随本轮结束，同样通过结束提醒让用户回来；不要求问题必须来自某个结构化工具，也不依赖扫描正文问号触发通知。

### 协议事件与文案

协议事件是上述场景的实现细分，不要求各 harness 提供同名 hooks。保留既有三类 kind，v2 增加 `attention_required` 承接停止或异常查看提醒。

| 协议事件 | 生成条件 | 用户文案与边界 |
| --- | --- | --- |
| `turn_completed` | harness 明确报告本轮正常结束 | “本轮已结束，可以查看结果或安排下一步”；不代表业务目标已验收 |
| `approval_requested` | 观察到授权申请 | “需要确认”，附简短操作说明；申请在实际送达时可能已处理 |
| `input_requested` | 结构化提问，或 harness 明确报告需要用户输入 | “需要回复”；有问题和选项就携带，没有结构化内容也可提示回到原会话 |
| `attention_required`，v2 | harness 报告主动停止、自动异常结束，或已观察的停止信号短暂核验后仍无法确认结果 | `reason` 区分 `stopped`、`error`、`stop_signal`；分别提示“已停下，请查看”“运行遇到问题”或“收到停止信号，请查看”，不宣称成功 |

最后一种事件只依据已经观察到的 harness 信号。普通静默、长工具调用、网络等待、未开始任务的闲置、子 agent 结束和已明确识别的用户主动取消不触发。无法区分停止原因时使用中性文案，并在能力报告中说明取消识别的缺口；无需为了得到“成功”标签而等待。

### 及时性与停止信号处理

- 明确的结束、输入或审批事件立即入队。标题、摘要、问题选项等补充信息不能延迟核心提醒。
- 已知可能自动继续的 Stop 候选先持久保存最小关联状态并中性返回，交给 worker 在回调返回后做最多 1 秒的本地核验或合并。同步 hook 内不能等待“继续”信号，因为 harness 可能正在等该 hook 退出。明确观察到继续、取消或已有对应请求提醒时，抑制候选；到期仍只有停止信号时，按 `attention_required/reason=stop_signal` 提醒，承认它可能随后继续，不把超时或没有新输出解释为已完成。核验窗口按持久时间戳计算，重启不能反复延长。
- 每次接手机会尽量只提醒一次；停止信号、结束日志和迟到结果按前述身份规则合并。不能用长时间安静检测换取表面上的低重复率。
- 在 worker 正常运行、无队列积压且渠道允许发送的条件下，本地目标为从 Blip 接收到信号至首次投递尝试 P95 不超过 2 秒，包含上述核验窗口。发送端应在新通知入队后及时处理，同时遵守已有渠道发送间隔。
- 分别记录 harness 信号至 Blip 接收、接收至首次投递尝试的延迟；没有可靠原始时间时只测后者。厂商处理和手机实际显示时间不计入本地时延承诺。离线、限流和积压仍按既有持久队列恢复。

新增事件格式 v2 保留现有通知字段，增加必填 `source_id`，允许原生 `turn_id`、`request_id` 缺省；`session_id` 仍必填。缺少原生 session ID 的来源须先取得可证明的会话归属，否则不进入正式适配。

v2 的 `attention_required` 携带 `reason`，其他事件保留既有通知字段。原始事件名称、解析器版本、关联证据和去重精度保存在本地诊断或 adapter 状态，不默认把完整原始载荷写入队列或 webhook。异常自动结束及时提醒，已识别的用户主动取消仍抑制；它们不能共用一个不区分原因的“失败”判断。

配置版本、事件版本和内部状态格式分别管理，不能把它们视为同一版本号。事件读取端同时支持 v1 与 v2：

- 旧配置映射出的默认 Codex 来源保持 `event_version=1`，沿用原三类事件和 webhook 语义；将旧配置迁移为 sources 时显式保存这个值。
- 新增来源默认使用 `event_version=2`。默认 Codex 也可显式升级到 2，获得停止和异常查看提醒；v1 仅作为默认 Codex 的兼容模式，不能成为其他 harness 的接入限制。
- 默认 Codex 升级协议后，既有请求和正常结束仍沿用原身份算法；同轮第一段停止提醒复用原完成通知身份。若发出停止提醒后明确恢复执行，后续接手使用带持久序号的新身份；同一段的重复回调复用首次快照，避免迟到证据和协议升级触发重复发送或冲突隔离。
- 旧队列按原版本发送，不在读取或重试时批量改写事件、重算 ID 或重置收据。
- 增加 v2 来源或升级默认 Codex 前，配置流程明确显示已有 webhook 目的地将收到 v2 及新增 kind；接收端须按 `schema_version` 处理。保留 [webhook v1](webhook.md)，新增独立 v2 协议说明及示例。
- 旧程序不能安全读取 v2 队列。回退旧程序前须停止新 worker，并确保没有需要旧程序处理的 v2 记录；仅恢复旧配置备份不构成安全回退。

事件格式的变化不增加通知渠道。飞书、企业微信、Gotify 继续使用公共模板；webhook 使用固定结构化事件格式和 `X-Blip-Event-Id`。

## Hook 命令与生命周期

公共入口已支持来源参数；具体 harness 安装命令在其 adapter 可用后注册：

```text
blip install claude-code --source claude-main
blip uninstall claude-code --source claude-main
blip doctor --source claude-main
blip hook --source claude-main
```

`hook` 是内部入口。安装器写入原生可执行文件、配置文件的绝对路径和 source ID，不依赖 PATH、npm 的 `blip.cmd` 或当前项目目录。裸 `blip hook` 和现有 `install codex`、`uninstall codex` 继续映射到 `codex-default`。

明确信号的运行过程为“有界读取 → 来源校验 → 解析 → 持久入队 → 中性响应”；早期 Stop 候选则持久保存 adapter 的最小关联状态后返回，由 worker 核验并创建通知。公共 runner 限制载荷大小，初始沿用 1 MiB 上限；内部读取、锁等待与可选查询共用有界预算，并匹配目标 harness 的外部超时约束。不能只给入队设置 timeout，却让 stdin 永久等待 EOF。

网络请求不进入 hook。返回前完成必要的持久写入，不能启动未托管的后台写入后立即退出。若调用方强制终止回调，能否补收取决于该 adapter 是否有独立 collector；纯 hook 来源不承诺补收未入队的事件。

接入管理区分直接 executable＋argv、cmd、PowerShell、POSIX sh/Git Bash。JSON 保持在 stdin 中；仅在实际执行协议要求时生成薄 wrapper。各 adapter 声明空 stdout 或中性 JSON 的正确形式，通知错误记录在本地诊断中，按已验证协议返回不会阻止 agent 的响应；不能把错误转换成自动批准或拒绝。

安装必须保留已有 hooks、文件权限、用户明确关闭的功能及信任决策。安装完成不等于 hook 已被信任或实际触发；`doctor` 分别报告配置存在、外部信任状态是否可判定、最近实际回调情况。卸载只删除清单中可确认归属的条目，其他内容已发生冲突时提示人工处理。

## 多来源运行与故障隔离

worker 管理多个独立来源，各自拥有 collector、读取位置与诊断。慢采集、失败重试及名称读取有界，不能串行拖住其他来源；目的地发送仍使用现有独立循环。

| 故障 | 处理 |
| --- | --- |
| 某来源版本不支持、hook 未安装或缺少读取权限 | 标记该来源受限；其他来源和已入队通知继续运行 |
| 某来源 checkpoint 损坏 | 暂停该来源采集，保留原文件，禁止静默重置基线；已入队通知继续投递 |
| 单条队列记录损坏 | 沿用现有隔离机制，保留身份和证据，健康记录继续处理 |
| 队列根目录不可用、配置结构损坏、worker 锁冲突 | 作为公共运行故障报告，不能显示正常运行 |
| 某目的地失败或超时 | 仅更新该目的地收据，不阻塞其他目的地 |

`status` 保留运行状态与目的地统计，新增逐来源的安装状态、版本、能力、采集健康、最近事件时间和错误码。全局 `ready` 表示公共队列和调度已初始化，各来源完成初始检查；`degraded` 表示有来源受限，不能将 `ready` 解读为所有来源都健康。旧 `collector_error` 在过渡期映射默认 Codex 来源错误。

仅有 hooks 的来源无需伪造 collector；长时间没有事件也不自动判定故障。能力报告围绕本轮结束、输入、审批和自动停止/异常四项，分别记录 native/derived/signal-only/unavailable，以及文档依据、样本验证、实机验证和延迟。只有停止信号而无法判断结果时报告 signal-only，不冒称正常完成，也不隐藏已可用的提醒能力。桌面版、CLI 和服务运行分别验证。

Windows 服务安装保存来源解析后的绝对位置，不使用 LocalService 的 home 推断用户配置。各 adapter 声明实际需要读取的日志目录和标题索引，按来源登记授权；不为方便读取而授予整个 agent home 的权限。服务程序副本、路径校验、授权归属与卸载清理规则沿用现有实现，并扩展安装元数据版本。新增来源、变更读取路径或升级服务程序后需重新安装服务以应用变更；普通来源配置编辑不隐式提权。

服务卸载依据安装时持久保存的 grants 清理本服务身份的授权，即使来源后来被删除也能正确撤销。其他身份权限继续保留，现有网络路径和 reparse point 限制不随多来源扩展而放宽。

## 现有数据迁移

第一批实施优先通过兼容映射完成重构，避免移动状态文件。

| 现有数据或行为 | 迁移规则 |
| --- | --- |
| 配置 v1 与 `codex_home` | 读取时映射为默认 `codex-default`；普通读取不写回。保存多来源配置时生成 v2、备份原配置并沿用并发修改检查 |
| Codex 默认目录解析 | v1 保留原环境变量、用户目录和服务安装默认值的解析顺序；迁移成 v2 时保存已解析位置 |
| v2 `sources` | 为唯一来源定义；不接受同时设置顶层 `codex_home`，避免两个来源指向同一 home |
| `rollout-cursors.json` | 默认 Codex 继续读取原文件；新增来源使用 `sources/<id>/` 下的独立 checkpoint |
| `installation.json` | 识别为默认 Codex 安装清单，继续支持原卸载逻辑；新增来源使用独立安装清单 |
| 旧 Codex 事件与收据 | 已入队事件的 ID、版本、投递次数和结果保持不变；该来源可以显式启用 v2 新通知，同次接手仍复用首次快照 |
| 首次采集基线 | 已有来源继续原位置；新来源首次建立基线，不发送既有历史；worker 停止后重启按原恢复契约处理增量。来源配置停用的补收边界见来源管理规则 |
| 已有通知显示 | 设备名、对话名和事件时间沿用现有捕获与补齐规则；按 source 查询可选标题，不能跨来源碰撞 session ID |

`codex-default` 是兼容来源的保留 ID，不能重新分配给其他 harness 或另一个 home。其旧状态一旦被识别为损坏，不能以“没有新目录”为由创建全新基线。新来源也不能复用已删除来源的身份和状态来代表不同的会话空间。

多来源版本部署时须一起升级用户 hook 所用程序和 Windows 服务程序副本，再启用 v2 来源，避免旧服务隔离新事件。迁移测试必须覆盖进程中断后的重启，不只验证一次成功转换。

## 各 harness 的接入顺序与边界

以下是实施路线，除 Codex 的既有覆盖外，不代表 Blip 已完成兼容验收。

| Harness | 接入路线 | 验收重点 |
| --- | --- | --- |
| Codex | 保留请求 hooks＋rollout 正常结束核验，v2 评估停止和自动异常信号 | 默认来源可升级 v2；原 ID 保持，迟到完成不重复提醒；手动取消与自动异常分开处理 |
| Claude Code | 原生程序接收 command hooks，独立安装与解析 | 对可被延续的 Stop 做有界合并；明确结束立即提醒，不明结果使用中性文案；区分审批、输入与闲置 Notification |
| Hermes Agent | 优先评估目标版本 shell hooks，必要时使用薄 Python 插件 | 明确 shell envelope 与插件 kwargs 差异；保留 profile、session、turn 和结束结果；共享 gateway 正确关联会话 |
| DeepSeek Harness / DSH | 先使用官方 hooks bridge，再按必要能力评估原生插件 | bridge 事件集、字段与子会话身份并非完整 Claude 协议；缺少审批事实时明确不支持；停止信号可先提供查看提醒，正常结果作为更具体证据 |
| WorkBuddy | 独立 adapter，基于桌面版真实插件与回调样本实施 | 配置位置、shell、事件字段和版本分别验证；不能以 CodeBuddy CLI 文档替代桌面实测 |

Hermes 的结束事件必须区分每轮 finalization、自动异常和普通进程退出，按实际含义选择结束或查看提醒。Orca 的 [Hermes 插件](https://github.com/stablyai/orca/blob/26741256dcccecac4007f64476a17825fdd3858b/src/main/hermes/hermes-managed-plugin-source.ts) 展示了共享 gateway 会话绑定处理；Blip 保留可用的结果字段，但不将完整结果字段作为及时通知的前提。

Orca 的 [DSH 事件处理](https://github.com/stablyai/orca/blob/26741256dcccecac4007f64476a17825fdd3858b/src/shared/agent-hook-listener/providers/dsh-events.ts) 展示了明确降级的做法。是否能生成某类通知由真实事件能力决定，不能通过空闲时间、屏幕文字或普通工具调用补猜。

官方接口基线：

- [Codex Hooks](https://learn.chatgpt.com/docs/hooks) 与现有 [Codex 事件来源 ADR](adr/0001-codex-event-sources.md)。
- [Claude Code Hooks](https://code.claude.com/docs/en/hooks)。
- [Hermes Event Hooks](https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks)；[轮次收尾源码参考](https://github.com/NousResearch/hermes-agent/blob/90d6b98fd86d20981ee06ddd5b7d93e4320f26a4/agent/turn_finalizer.py)。
- [DSH Claude bridge 固定版本说明](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/experimental/hooks-claude-code/README.md)。
- [WorkBuddy 插件说明](https://www.codebuddy.cn/docs/workbuddy/From-Beginner-to-Expert-Guide/Function-Description/Plug-In) 与仅适用于对应 CLI 的 [CodeBuddy Hooks](https://www.codebuddy.ai/docs/cli/hooks)。

各 adapter 实施时固定被测客户端版本，将脱敏回调样本与能力记录一起入库；不把动态官方文档链接本身当成版本测试结果。

## 实施批次

### 第一批 公共边界与 Codex 兼容迁移

交付来源 registry、小接口、v1/v2 读取、source 配置、命令兼容映射、来源状态目录、逐来源诊断和故障隔离。修改点主要在 `internal/config`、`internal/event`、`internal/worker`、`internal/service` 与 `cmd/blip`，现有 `internal/codex` 通过注册接入。

完成标准：旧配置无需修改即可运行；旧游标不重置、旧 ID 不重算、收据不丢失；一个来源失败时健康来源和投递继续工作。更新对应迁移、服务与 webhook 文档，但不将其他 harness 标成已支持。

### 第二批 Claude Code 与 Hermes

先实现 Claude Code 的安装、解析、身份规则和真实进程验证，用第二个 harness 检验公共接口；再接 Hermes 验证不同事件形状、profile 与结果字段。仅提取两者已证明相同的辅助逻辑，不预先设计万能 Claude-compatible parser。

完成标准：每个发布能力都有真实脱敏样本和目标版本验证记录，缺失能力可在 `doctor/status` 中看到；各类接手提醒使用同一套渠道发送器。无法判断正常结果但已有停止信号时，提供 signal-only 查看提醒；没有任何可用信号时才标为 unavailable。

### 第三批 DSH 与 WorkBuddy

DSH 按 bridge 的实际覆盖接入，WorkBuddy 按桌面目标版本实测接入。根据结果完善安装器和能力矩阵，维持既有队列与目的地协议边界。

完成标准：声明的能力与实际回调对应；安装、停用和卸载不影响用户其他插件；不会因共享配置格式而继承错误的子任务、完成或审批判断。

## 验收矩阵

| 维度 | 必须覆盖 |
| --- | --- |
| 语义 | 正常结束、停止结果未知、自动异常、停止后继续、手动取消、子 agent、普通会话退出、明确等待输入但无结构化问题、正文提问后结束本轮 |
| 身份 | 同一 harness 多来源、不同来源同 session ID、重复回调、多途径同次接手、重启回放；停止提醒后仅迟到完成不重复，明确继续后再次结束可重发，已有具体请求不追加泛化停止 |
| 安装 | 幂等安装、保留其他 hooks、并发修改冲突、配置损坏、备份失败、精准卸载、信任尚未完成 |
| 进程协议 | 中文及空格路径、目标 shell、stdin 大小与 EOF 行为、超时、stdout 契约、退出码；按实际支持范围增加特殊字符样本 |
| 持久化 | 入队完成前退出、候选核验及接手序号恢复、入队后游标保存前崩溃、游标损坏、旧 v1 数据、v2 数据、隔离记录及服务升级顺序 |
| 故障隔离 | 单来源卡住或损坏、可选标题查询失败、单目的地超时、公共队列不可用、停止时未完成请求 |
| 投递 | 多来源进入同一队列，四类渠道均可发送；收据独立、重试 ID 稳定、模板保留设备名和事件时间 |
| 及时性 | 明确信号及时入队、可选补充不阻塞、候选核验有界；测量正常条件下接收到首次投递尝试 P95，并单独报告限流和离线情况 |
| Windows 服务 | 安装用户与服务身份分离、来源路径保存、最小读取权限、升级程序副本和卸载授权清理 |

实现阶段按改动运行现有 Go、Node 和必要进程检查。协议模拟、真实 harness 回调、真实外部渠道投递分别记录，不将其中一项通过写成全链路验收。外部通知发送仍只在已授权的测试目的地进行。
