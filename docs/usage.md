# 使用指南

本页介绍 Blip 的配置、接入与日常管理。首次使用可从 [README 的快速开始](../README.md#快速开始)入手。

## 通知何时触发

| 提醒 | 含义 |
| --- | --- |
| 本轮结束 | 查看结果，继续当前工作或安排下一个任务 |
| 需要响应 | 回答问题、确认操作，或查看停止和异常原因 |

Codex 通过 Hook 与会话日志采集提问，按原生调用 ID 去重。通知会带上问题和选项，方便你及时回复。

## 安装与首次启动

Blip 支持 npm 安装和直接运行可执行文件。npm 包仅提供 Windows x64 的预编译 Go 程序，安装和使用不需要 Go 编译器；npm 启动器需要 Node.js 20 或更新版本。配置与通知队列保存在用户目录，升级 npm 包不会将它们写入包安装目录。

npm 包名为 `@teamsillybees/blip`，正式版本使用 `latest` 标签。本地安装包的构建、安装与发布说明见 [npm 分发](npm-distribution.md)。安装正式版：

```powershell
npm install -g @teamsillybees/blip@latest
blip
```

选择通知渠道，填写地址和必要凭据，然后选择 **保存并启动**。Blip 会保存配置，为需要 Hook 的来源安装接入，启动通知进程并逐个测试渠道。已有进程会重启以加载新配置；已注册 Windows 服务时使用该服务，需要在管理员终端操作。也可选择 **仅保存并退出**，稍后按提示应用。

版本、升级和卸载步骤见 [npm 安装与分发](npm-distribution.md)。需要固定版本时，可用 `npm install -g @teamsillybees/blip@版本号`。

直接运行单文件时，使用下面的构建方式；构建需要 Go 1.25.8 或更新版本，运行生成的程序不需要安装 Go 或 Node.js。

```powershell
go build -o bin/blip.exe ./cmd/blip
.\bin\blip.exe
```

## 交互配置

直接运行 `blip` 会在终端打开配置向导；`configure`、`config`、`setup` 也可打开同一向导。非交互环境下，裸 `blip` 显示帮助。命令由 [Cobra](https://github.com/spf13/cobra) 管理、终端表单由 [Charm Huh](https://github.com/charmbracelet/huh) 提供。向导支持新增、编辑、启用、停用和删除渠道目的地；飞书填写 Webhook 和可选签名密钥，企业微信只需填写 Webhook，Gotify 填写服务地址和应用 token，自定义 Webhook 支持 Bearer token 或静态请求头。一个渠道可配置多个目的地。

设备名称自动使用当前计算机名（hostname），可在主菜单修改。目的地 ID 自动生成并保持稳定；新目的地默认启用，Gotify 优先级默认为 5，可在“编辑目的地”中调整。`blip init` 也会写入当前计算机名；旧配置缺少 `device_name` 时自动使用此默认值。

URL 明文显示并可直接编辑；Token、签名密钥和请求头值使用遮蔽输入，编辑时可保留现值。修改先保存在内存中，选择保存操作后一次写入配置；保存前取消或 Ctrl+C 不保存。已有配置更新前会备份为 `config.json.bak`，若另一程序同时修改了文件，向导会拒绝覆盖并提示重新打开。选择“保存并启动”后若安装或投递失败，已保存的配置保留，终端会说明失败环节；修正后可重新打开向导继续。仅保存时，根据当前运行状态给出下一步命令，并带上实际配置路径。

配置默认保存在 `%APPDATA%\Blip\config.json`。在真实终端运行向导；需要屏幕阅读器友好的文本提示时使用 `blip configure --accessible`。脚本环境可使用 `blip init` 生成示例后编辑 JSON；[配置示例](../internal/config/example.json)中的所有目的地默认关闭。

## 手动接入与通知测试

需要分别管理接入和后台时，可使用：

```powershell
.\bin\blip.exe install codex
.\bin\blip.exe test --wait
```

交互终端中的 `test`（或显式 `test --wait`）会在需要时启动通知进程，排入测试通知并最多等待 15 秒，分别显示已送达、失败或等待重试。未全部送达时返回非零退出码，待发记录仍留在队列中，可用 `status` 查看。脚本或重定向环境中的 `test` 默认仅入队；需要等待投递结果时，显式加上 `--wait`。

未启用飞书签名时，删去该目的地的 `secret` 字段，或将它设为 `""`。安装命令会增量写入 Codex `hooks.json`，保留其他 hook 并备份原文件。随后在 **Codex 中审阅并信任新 hook**，必要时重开既有会话。当前 Codex 默认启用 hooks；如果配置显式关闭，安装命令会提示先在 Codex 中启用，不会改写 `config.toml` 或信任记录。

## Codex 固定 Hook 入口

Codex hook 通过 home 下的固定入口挂载：Windows 为 `<codex_home>/.blip-hooks/<安装标识>/hook.cmd`，Linux/macOS 为 `hook.sh`。旁边的 `run.cmd` / `run.sh` 保存当前 Blip 程序、配置路径和来源参数；切换 Node/npm 安装目录后，运行 `blip install codex`（其他来源加 `--source ID`），或在向导选择“保存并启动”，即可更新转发目标。普通升级保持入口内容、注册命令和 hook 顺序不变。

从旧版直接调用 `blip.exe` 的注册方式迁移时，需要审阅一次新入口；后续若只变更程序路径，不会因此改变 hook 定义。事件、匹配器或超时等定义发生变化时，仍按 [Codex 的信任规则](https://learn.chatgpt.com/docs/hooks)重新审阅。安装记录兼容旧格式；卸载只清理本配置拥有的 hook 和入口文件。

请将程序放在固定位置后再执行安装；移动程序或更换配置路径后，重新安装 hook。

## 后台进程与开机自启

`start` 保持启动普通后台进程的原有行为，在 Windows 隐藏窗口运行，不设置开机自启；`stop` 保持通过文件请求平稳停止的行为。新增的 `restart`、向导“保存并启动”和 `test --wait` 会自动识别已注册的 Windows 服务，服务权限不足会直接报告错误。日常管理服务仍可使用 `service start/stop`。首次运行只为已有会话建立读取基线，不发送历史完成消息。之后的读取位置与待发通知保存在配置目录下的 `data` 中，停止期间的新记录会在重启后继续读取。

需要开机自动运行时，可注册 Windows 服务。先配置通知渠道并停止普通后台进程，再在同一账户的管理员 PowerShell 中执行：

```powershell
.\bin\blip.exe service install
.\bin\blip.exe service start
.\bin\blip.exe service status
```

服务注册为自动（延迟启动），重启后无需登录即可运行。使用 `service stop` 停止、`service uninstall` 停止并卸载；配置和通知数据保留。服务使用独立低权限身份、安装时记录的配置与 Codex 目录，以及单独保存的程序副本。升级、目录权限和自定义配置用法见 [Windows 服务](windows-service.md)。

## 命令参考

```text
blip [--config PATH] [COMMAND]

（无命令）           在终端打开配置向导
configure           配置渠道，可一次完成接入、启动和测试（别名 config / setup）
init                生成配置示例，适合脚本或手动编辑
install codex       安装输入与审批 hook
uninstall codex     仅移除本配置安装的 Blip hook
start / stop        启动后台进程 / 请求平稳停止
restart             重启并加载新配置；自动识别 Windows 服务
service             管理 Windows 服务（install / uninstall / start / stop / restart / status）
run                 在当前终端运行
status              显示运行摘要和投递结果；--json 输出完整机器可读数据
doctor              显示逐来源安装、能力与最近 hook 回调（可加 --source）
sources             终端默认列出来源；脚本使用 sources list，其他管理子命令保持可用
test                终端自动启动并检查投递；脚本默认仅入队，可加 --wait 检查投递
version             显示版本
```

`--config` 可放在命令之前或之后。例如：

```powershell
.\bin\blip.exe --config 'D:\我的工具\Blip\config.json' start
```

在向导中选择“保存并启动”即可应用配置；手动编辑 JSON 后执行 `blip restart`。重启前会先校验新配置，校验失败不会停止原进程。`codex_home` 默认按 Codex 的 `CODEX_HOME` 设置定位，未设置时使用 `~/.codex`；也可以在配置中指定。`state_dir` 默认为配置文件同目录下的 `data`。相对路径以配置文件目录为基准。服务安装时保存这些默认目录，后续更换目录需要重新安装服务。

`status` 在交互终端显示易读摘要；重定向或管道输出时默认保留 JSON，也可显式使用 `status --json`。

`status` 的 `running` 表示后台进程持有运行锁，`ready` 表示公共队列和各来源初始检查已完成；`degraded` 表示某个来源受限，须查看 `sources` 中的错误码。损坏的来源游标会暂停该来源，其他来源与已入队通知继续工作。`collector_error` 兼容映射默认 Codex 的错误。`destinations` 按目的地列出投递数量与最近失败记录的 `last_error`，不输出目的地 URL 或凭据。`stop` 可中断长轮询和等待中的网络请求；未确认送达的通知会保留到下次启动。

## 多来源配置

支持多个独立 Codex home、[Claude Code](claude-code.md)、[Hermes](hermes.md) 和 [DSH](dsh.md)，使用内置 harness 适配层。旧配置无需修改；需要新增来源时使用：

```powershell
blip sources add --source codex-work --harness codex --home 'D:\Agent Homes\codex-work'
blip
```

在向导选择“保存并启动”，会安装所有已启用来源并重启、测试。单独运行 `install` / `uninstall` 时，非 Codex harness 只有一个来源可省略 `--source`，存在多个来源时显式选择。`install codex` 和 `uninstall codex` 始终默认操作 `codex-default`；操作其他 Codex home 必须指定 `--source`，避免旧命令误操作其他来源。

这会将配置迁移为 v2 并备份原文件。原来的 `codex-default` 保持事件 v1、旧 ID、游标和安装记录；新来源使用事件 v2，webhook 接收端须支持 `schema_version: 2`。每个来源 ID 对应一个固定的 home；切换环境时请创建新来源。详见[来源管理与迁移](sources.md)和 [webhook v2](webhook-v2.md)。

## 通知目的地

同一渠道可以配置多个不同 `id` 的目的地，通知发送到所有启用的目的地。`id` 是投递账本的身份，建议保持稳定。

| `kind` | 配置 | 成功判定 |
| --- | --- | --- |
| `feishu` | 群自定义机器人的 `url`，可选 `secret` | HTTP 2xx 且 JSON `code: 0` |
| `wecom` | 企业微信群机器人的完整 `url`，包含 key | HTTP 2xx 且 JSON `errcode: 0` |
| `gotify` | 服务根 `url`、应用 `token`，可选 `priority`（默认 5） | HTTP 2xx 且返回有效消息 ID |
| `webhook` | `url`，可选 `token` 或 `headers` | HTTP 2xx |

Gotify 的 `url` 可以包含反向代理子路径，Blip 会在其后追加 `/message`。使用应用 token。自定义 webhook 的 `token` 对应 `Authorization: Bearer ...`，也可以通过 `headers` 指定鉴权；两者不要重复配置 Authorization。

URL、token、secret 和 header 值直接填写在本地配置中，不再支持环境变量引用。升级旧配置时，请将 `${变量名}` 替换为实际值；含此类引用的目的地需要修正后才能启用。不要把填有真实密钥的配置提交到版本库。

默认通知标题显示 `完成 · 项目名 · 设备名` 或 `需要输入 · 项目名 · 设备名`，正文显示对话名和事件时间。时间按运行 Blip 的计算机本地时区显示，包含时区偏移，重试仍显示原事件时间。需要输入时追加问题及 A、B、C、D 等选项；审批归为“需要输入”，正文追加简短的确认提示。Gotify 使用独立标题，飞书和企业微信将标题放在文本首行。例如：

```text
需要输入 · Blip · 开发电脑
实现通知模板
时间：2026-11-06 21:30:00 +08:00

请选择通知渠道
A. 飞书
B. Gotify
```

完成通知在对话名和时间下方追加来源提供的最后一条回复内容，最多 4000 字节；内容为空时省略。默认模板不显示会话 ID 和轮次，结构化 webhook 保留详细字段。中文与 emoji 按 UTF-8 安全截断，企业微信整条文本限制为 2048 字节，较长回复可能进一步截断；飞书同时限制编码后的请求体大小。

对话名读取自 Codex 本地 `session_index.jsonl` 中该会话最新的名称记录，入队后保留；旧通知缺少名称时在发送前尝试补齐。没有名称或索引不可读时显示“未命名对话”。

自定义 webhook 的固定版本格式见 [webhook 协议](webhook.md)。

## 投递与恢复

Hook 只快速入队，不在 Codex 回调中请求外部网络。每个目的地有独立发送循环、超时与收据；一个渠道的慢请求不会占用其他渠道的发送循环。企业微信与飞书分别留有最低发送间隔，降低触发服务频率限制的概率。

默认请求超时 5 秒，最多尝试 5 次，重试退避为 5、10、20、40 秒，上限 1 分钟。HTTP 408、425、429、5xx、传输失败和机器人业务失败可重试；其他 HTTP 4xx 直接记为失败。`request_timeout`、`poll_interval` 和 `max_attempts` 可在配置中调整。

有原生请求标识时按标识去重；没有请求标识的审批按内容和 10 秒时间桶尽力合并，跨桶或重复申请仍可能提醒多次。正常完成按会话和轮次去重。投递为**至少一次**：外部接收成功后、本地记账前崩溃，重启可能再次发送。自定义接收端可使用 `X-Blip-Event-Id` 去重。

待发与暂时停用目的地的记录保留到可继续投递；已送达或重试耗尽的记录保留 7 天。`status` 的 `failed` 非零时，可在 `data/jobs/` 查看对应收据的错误码。修复配置后，新增事件会使用新配置；当前没有失败记录的手动重放命令。

保留期限从一条通知的所有目的地均投递结束后计算。损坏的队列记录会自动隔离到 `data/quarantine/records/`，保留原文件和诊断信息，其他通知继续处理。`status` 的 `quarantined` 表示隔离的记录组数量；同一文件再次损坏会在原记录组内另存证据。隔离记录不会自动清理，其事件标识也会被保留，避免回放时重置收据而再次发送。恢复前请先检查原记录与接收端结果。

`data/worker.log` 记录运行错误，不记录 URL、token、签名密钥或渠道响应正文。队列包含通知摘要，读取位置记录包含本地项目路径，请按本地配置数据管理。

## 常见问题与诊断

| 现象 | 检查与处理 |
| --- | --- |
| 测试通知未送达 | 运行 `blip test --wait` 和 `blip status`，检查目的地是否启用、地址与凭据是否正确，以及最近投递错误。 |
| 测试可送达，但编程助手没有触发提醒 | 运行 `blip doctor`，确认来源 home、Hook 安装和客户端内的信任状态；Codex 既有会话可能需要重新打开。 |
| 修改配置后未生效 | 执行 `blip restart`，或在向导选择“保存并启动”；已注册服务时，在管理员终端操作。 |
| 升级后 Hook 仍指向旧程序 | 重新运行向导并“保存并启动”，刷新已启用来源的接入；Windows 服务还需按[服务升级步骤](windows-service.md#账户目录和升级)更新独立副本。 |
| 来源显示 `degraded` | 查看 `status` 中逐来源的 `state` 和 `error_code`，按[来源诊断说明](sources.md#诊断和回退)恢复；保留原读取位置与绑定记录。 |
| 对话名显示“未命名对话” | Codex 的名称索引可能缺失或不可读；部分其他来源尚未提供对话名。Windows 服务的名称索引权限见[服务指南](windows-service.md)。 |

提交问题时，请提供 Blip 与编程助手版本，以及脱敏后的 `doctor` / `status` 输出。
