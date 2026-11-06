# Blip

把 Codex 需要你关注的消息，发送到飞书、企业微信、Gotify 或自定义 webhook。

首版面向 **Windows 上的 Codex CLI 与桌面版**，提供三类提醒：

| 提醒 | 触发依据 |
| --- | --- |
| 本轮完成 | 根会话中带会话、轮次标识的正常 `task_complete` 记录 |
| 审批申请 | 官方 `PermissionRequest` hook 实际触发 |
| 需要输入 | `request_user_input` / `request_user_input_async` 的 `PreToolUse` |

“本轮完成”表示一次回复正常结束。审批覆盖取决于 Codex 的实际 hook 路径，完整边界见[兼容性与验证](docs/compatibility.md)。普通正文问句、其他 coding agent、远程审批和通知模板编辑器不在首版范围。

Hook 安装按 Codex `0.163.0` 的配置协议实现；更早版本需要另行验证，桌面版与 CLI 的实机证据分别记录在兼容性说明中。

## 开始使用

Blip 支持 npm 安装和直接运行可执行文件。npm 包提供对应平台的预编译 Go 程序，安装和使用不需要 Go 编译器；npm 启动器需要 Node.js 20 或更新版本。配置与通知队列保存在用户目录，升级 npm 包不会将它们写入包安装目录。

npm 包名为 `@teamsillybees/blip`，当前预发行版本使用 `next` 标签。本地安装包的构建、安装与发布说明见 [npm 分发](docs/npm-distribution.md)。安装预发行版：

```powershell
npm install -g @teamsillybees/blip@next
blip configure
blip install codex
blip start
blip test
```

当前源码版本为 `0.2.0-dev.1`，尚未发布；公共 npm 仍为 `0.2.0-dev.0`。下面的设备名称设置和简化配置向导可先通过源码构建使用。

直接运行单文件时，使用下面的构建方式；构建需要 Go 1.25.8 或更新版本，运行生成的程序不需要安装 Go 或 Node.js。

```powershell
go build -o bin/blip.exe ./cmd/blip
.\bin\blip.exe configure
```

`configure` 是交互式配置向导（也可写作 `config` 或 `setup`），由 [Cobra](https://github.com/spf13/cobra) 管理命令、[Charm Huh](https://github.com/charmbracelet/huh) 提供终端表单。向导支持新增、编辑、启用、停用和删除渠道目的地；飞书填写 Webhook 和可选签名密钥，企业微信填写 Webhook，Gotify 填写服务地址、应用 token 和优先级，自定义 Webhook 支持 Bearer token 或静态请求头。一个渠道可配置多个目的地。

首次配置会填写“设备名称”，默认使用当前计算机名（hostname），可自行修改；之后可在主菜单修改，保存为 `device_name`。`blip init` 也会写入当前计算机名。旧配置缺少该字段时，运行时默认使用当前计算机名，再次打开向导可确认并保存。

URL 明文显示并可直接编辑；Token、签名密钥和请求头值使用遮蔽输入，编辑时可保留现值。修改先保存在内存中，确认保存后一次写入配置；取消或 Ctrl+C 不保存。已有配置更新前会备份为 `config.json.bak`，若另一程序同时修改了文件，向导会拒绝覆盖并提示重新打开。保存后按提示安装 Codex hook、启动后台进程；修改运行中的配置需要 `stop`、`start` 才能生效。

配置默认保存在 `%APPDATA%\Blip\config.json`。在真实终端运行向导；需要屏幕阅读器友好的文本提示时使用 `blip configure --accessible`。脚本环境可使用 `blip init` 生成示例后编辑 JSON；[配置示例](internal/config/example.json)中的所有目的地默认关闭。

例如，启用飞书群机器人时，在配置向导中选择飞书，填写 Webhook URL 和可选的签名密钥，保存后执行：

```powershell
.\bin\blip.exe install codex
.\bin\blip.exe start
.\bin\blip.exe test
.\bin\blip.exe status
```

未启用飞书签名时，删去该目的地的 `secret` 字段，或将它设为 `""`。安装命令会增量写入 Codex `hooks.json`，保留其他 hook 并备份原文件。随后在 **Codex 中审阅并信任新 hook**，必要时重开既有会话。当前 Codex 默认启用 hooks；如果配置显式关闭，安装命令会提示先在 Codex 中启用，不会改写 `config.toml` 或信任记录。

请将程序放在固定位置后再执行安装；移动程序或更换配置路径后，重新安装 hook。

`start` 在 Windows 隐藏窗口启动后台进程；首次运行只为已有会话建立读取基线，不发送历史完成消息。之后的读取位置与待发通知保存在配置目录下的 `data` 中，停止期间的新记录会在重启后继续读取。首版不设置开机自启，重新登录系统后需要再次启动。

## 命令

```text
blip [--config PATH] COMMAND

configure           交互式管理各渠道配置（别名 config / setup）
init                生成配置示例，适合脚本或手动编辑
install codex       安装输入与审批 hook
uninstall codex     仅移除本配置安装的 Blip hook
start / stop        启动后台进程 / 请求平稳停止
run                 在当前终端运行
status              显示就绪状态、各目的地投递结果与损坏记录数量
test                向所有启用的目的地排入一条测试通知
version             显示版本
```

`--config` 可放在命令之前或之后。例如：

```powershell
.\bin\blip.exe --config 'D:\我的工具\Blip\config.json' start
```

配置变更后执行 `stop`、`start` 生效。`codex_home` 默认按 Codex 的 `CODEX_HOME` 设置定位，未设置时使用 `~/.codex`；也可以在配置中指定。`state_dir` 默认为配置文件同目录下的 `data`。相对路径以配置文件目录为基准。

`status` 的 `running` 表示后台进程持有运行锁，`ready` 表示它已完成一次采集检查。`collector_error` 非空时，完成事件采集遇到了问题，请结合 `data/worker.log` 检查；已入队通知仍独立投递。`destinations` 按目的地列出投递数量与最近失败记录的 `last_error`，不输出目的地 URL 或凭据。`stop` 可中断长轮询和等待中的网络请求；未确认送达的通知会保留到下次启动。

## 通知目的地

同一渠道可以配置多个不同 `id` 的目的地，三类事件发送到所有启用的目的地。`id` 是投递账本的身份，建议保持稳定。

| `kind` | 配置 | 成功判定 |
| --- | --- | --- |
| `feishu` | 群自定义机器人的 `url`，可选 `secret` | HTTP 2xx 且 JSON `code: 0` |
| `wecom` | 企业微信群机器人的完整 `url`，包含 key | HTTP 2xx 且 JSON `errcode: 0` |
| `gotify` | 服务根 `url`、应用 `token`，可选 `priority`（默认 5） | HTTP 2xx 且返回有效消息 ID |
| `webhook` | `url`，可选 `token` 或 `headers` | HTTP 2xx |

Gotify 的 `url` 可以包含反向代理子路径，Blip 会在其后追加 `/message`。使用应用 token。自定义 webhook 的 `token` 对应 `Authorization: Bearer ...`，也可以通过 `headers` 指定鉴权；两者不要重复配置 Authorization。

URL、token、secret 和 header 值直接填写在本地配置中，不再支持环境变量引用。升级旧配置时，请将 `${变量名}` 替换为实际值；含此类引用的目的地需要修正后才能启用。不要把填有真实密钥的配置提交到版本库。

通知标题和正文包含设备名称，便于在 Gotify 等渠道区分不同开发设备；正文还包含项目、会话、轮次、发生时间和简短详情。完成附回复摘要，输入附问题与选项，审批附工具与申请原因。事件入队时记录设备名，修改名称不会改写已经入队的新格式通知。中文与 emoji 按 UTF-8 安全截断，企业微信文本限制为 2048 字节；飞书同时限制编码后的请求体大小。

自定义 webhook 的固定版本格式见 [webhook 协议](docs/webhook.md)。

## 投递与恢复

Hook 只快速入队，不在 Codex 回调中请求外部网络。每个目的地有独立发送循环、超时与收据；一个渠道的慢请求不会占用其他渠道的发送循环。企业微信与飞书分别留有最低发送间隔，降低触发服务频率限制的概率。

默认请求超时 5 秒，最多尝试 5 次，重试退避为 5、10、20、40 秒，上限 1 分钟。HTTP 408、425、429、5xx、传输失败和机器人业务失败可重试；其他 HTTP 4xx 直接记为失败。`request_timeout`、`poll_interval` 和 `max_attempts` 可在配置中调整。

有原生请求标识时按标识去重；没有请求标识的审批按内容和 10 秒时间桶尽力合并，跨桶或重复申请仍可能提醒多次。正常完成按会话和轮次去重。投递为**至少一次**：外部接收成功后、本地记账前崩溃，重启可能再次发送。自定义接收端可使用 `X-Blip-Event-Id` 去重。

待发与暂时停用目的地的记录保留到可继续投递；已送达或重试耗尽的记录保留 7 天。`status` 的 `failed` 非零时，可在 `data/jobs/` 查看对应收据的错误码。修复配置后，新增事件会使用新配置；首版没有失败记录的手动重放命令。

保留期限从一条通知的所有目的地均投递结束后计算。损坏的队列记录会自动隔离到 `data/quarantine/records/`，保留原文件和诊断信息，其他通知继续处理。`status` 的 `quarantined` 表示隔离的记录组数量；同一文件再次损坏会在原记录组内另存证据。隔离记录不会自动清理，其事件标识也会被保留，避免回放时重置收据而再次发送。恢复前请先检查原记录与接收端结果。

`data/worker.log` 记录运行错误，不记录 URL、token、签名密钥或渠道响应正文。队列包含通知摘要，读取位置记录包含本地项目路径，请按本地配置数据管理。

## 开发

```powershell
go test ./...
go vet ./...
npm test
```

测试覆盖四类渠道协议、错误响应、Unicode 字节限制、并发去重、独立收据、重试、读取位置恢复，以及真实进程的中文与空格路径、安装卸载、后台启动停止。集成测试仅使用临时 Codex home 和本地 HTTP 服务。`go test -short ./...` 可跳过进程集成测试；具备 C 编译器的环境可运行 `go test -race ./...`。

项目约定见[首版需求](docs/requirements.md)、[术语表](GLOSSARY.md)和[架构决策](docs/adr/)。本项目参考 [hellolib/agent-notify](https://github.com/hellolib/agent-notify/tree/ed2842189eff44009a7bbe946b8a1b29bbc983d4) 的适配器、统一消息与渠道发送器分工，代码独立实现。依赖许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
