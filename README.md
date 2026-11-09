<p align="center">
  <img src="docs/assets/blip-banner.png" width="960" alt="Blip Logo">
</p>

<p align="center">
  <strong>Agent 需要你时，及时提醒。</strong><br>
  将编程助手的轮次结束、提问与审批申请，发送到你常用的通知渠道。
</p>

<p align="center">
  <a href="https://www.npmjs.com/package/@teamsillybees/blip"><img src="https://img.shields.io/npm/v/%40teamsillybees%2Fblip?style=flat-square&amp;color=ff7900" alt="npm 版本"></a>
  <img src="https://img.shields.io/badge/Node.js-%3E%3D20-339933?style=flat-square" alt="Node.js 20 或更新版本">
  <img src="https://img.shields.io/badge/platform-Windows_x64-18181b?style=flat-square" alt="Windows x64">
</p>

<p align="center">
  <a href="#快速开始">快速开始</a> ·
  <a href="#支持的编程助手">编程助手</a> ·
  <a href="#通知渠道">通知渠道</a> ·
  <a href="#文档">文档</a> ·
  <a href="https://github.com/TeamSillyBees/Blip/issues">反馈问题</a>
</p>

Blip 是一个本地运行的编程助手通知工具，接入 **Codex CLI 与桌面版、Claude Code、Hermes 和 DSH**，支持 **飞书、企业微信、Gotify 与自定义 Webhook**。

- **在关键时刻提醒**：本轮结束、需要回答问题或发起审批申请时，通知你回来处理。
- **消息带上上下文**：项目、设备、对话名和事件时间；提问附选项，完成通知可附最后一条回复。
- **多个环境，一处管理**：支持多个编程助手来源、多个独立 Codex home，以及同一渠道的多个接收端。
- **持久投递**：Hook 快速入队，后台独立发送；每个目的地分别记录结果、重试并在重启后恢复。

收到的提问通知示例：

```text
需要输入 · Blip · 开发电脑
实现通知模板
时间：2026-11-06 21:30:00 +08:00

请选择通知渠道
A. 飞书
B. Gotify
```

## 快速开始

### 1. 安装

需要 **Windows x64** 和 **Node.js 20+**。npm 包包含预编译的 Go 程序，安装和运行无需 Go 编译器。

```powershell
npm install -g @teamsillybees/blip@latest
blip
```

### 2. 配置通知渠道

在终端向导中添加通知目的地，填写地址和必要凭据，然后选择 **保存并启动**。Blip 会保存配置、安装已启用来源的 Hook、启动或重启通知进程，并逐个测试渠道投递。

首次配置默认接入 Codex；其他编程助手和多个 Codex home 的接入方式见下方[编程助手](#支持的编程助手)。已有 Windows 服务时，请在管理员终端应用配置。

### 3. 在编程助手中信任 Hook

按对应编程助手的提示审阅并信任新增 Hook。Codex 中的既有会话可能需要重新打开；若已显式关闭 hooks，需先在 Codex 中启用。

随后检查接入与投递：

```powershell
blip doctor
blip test --wait
blip status
```

配置保存在 `%APPDATA%\Blip\config.json`。重新运行 `blip` 即可编辑渠道与设备名称，完整操作见[使用指南](docs/usage.md)。

## 支持的编程助手

| 编程助手 | 接入方式 | 使用说明 |
| --- | --- | --- |
| **Codex CLI / 桌面版** | Hook + 本地会话日志；支持多个独立 home | 默认接入，[多来源配置](docs/sources.md) |
| **Claude Code** | 原生 Hooks，处理停止、提问与审批申请 | [接入 Claude Code](docs/claude-code.md) |
| **Hermes** | Profile 的 shell hooks | [接入 Hermes](docs/hermes.md) |
| **DSH · DeepSeek Harness** | 本地 Cordis 插件，观察原生会话与提问事件 | [接入 DSH](docs/dsh.md) |

各编程助手的支持版本与接入记录见[兼容性文档](docs/compatibility.md)。

例如，添加另一个 Codex 环境后，重新打开向导应用：

```powershell
blip sources add --source codex-work --harness codex --home 'D:\Agent Homes\codex-work'
blip
```

选择 **保存并启动**，安装已启用来源、重启并测试。新来源使用事件 v2；自定义 Webhook 接收端需支持对应协议。来源隔离、迁移与服务权限见[来源管理](docs/sources.md)。

## 通知渠道

| 渠道 | 需要准备 |
| --- | --- |
| **飞书** | 群自定义机器人 Webhook，可选签名密钥 |
| **企业微信** | 群机器人的完整 Webhook 地址，包含 key |
| **Gotify** | 服务地址、应用 Token，可选优先级 |
| **自定义 Webhook** | 接收地址，可选 Bearer Token 或自定义请求头 |

每个渠道都可以添加多个目的地，通知发送到所有已启用的目的地。配置与消息格式见[使用指南](docs/usage.md#通知目的地)；自行处理通知事件时，参阅 [Webhook v1](docs/webhook.md) / [Webhook v2](docs/webhook-v2.md)。

## 常用命令

| 命令 | 用途 |
| --- | --- |
| `blip` | 打开配置向导，也可使用 `blip configure` |
| `blip status` | 查看运行状态、来源和投递结果 |
| `blip test --wait` | 发送测试通知并等待投递结果 |
| `blip doctor` | 检查来源安装、能力和最近回调 |
| `blip restart` | 重启并加载配置，自动识别已注册的 Windows 服务 |
| `blip start` / `blip stop` | 启动 / 停止普通后台进程 |
| `blip sources list` | 列出已配置的编程助手来源 |

运行 `blip --help` 或 `blip <命令> --help` 查看完整选项。脚本可使用 `blip status --json`；无障碍终端提示使用 `blip configure --accessible`。

需要开机自启时，按 [Windows 服务指南](docs/windows-service.md)安装服务。服务运行独立程序副本，升级 npm 包后还需更新该副本；普通 `start` 不设置开机自启。

## 文档

| 想了解什么 | 去哪里 |
| --- | --- |
| 配置向导、手动接入、消息模板与故障排查 | [使用指南](docs/usage.md) |
| 安装、升级、卸载与本地 npm 分发 | [npm 安装与分发](docs/npm-distribution.md) |
| 开机自启、服务权限和服务升级 | [Windows 服务](docs/windows-service.md) |
| 多个来源、配置迁移与事件版本 | [来源管理与迁移](docs/sources.md) |
| 接收结构化通知事件 | [Webhook v1](docs/webhook.md) · [Webhook v2](docs/webhook-v2.md) |
| 编程助手的支持版本与接入记录 | [兼容性文档](docs/compatibility.md) |
| 事件含义与实现设计 | [术语表](GLOSSARY.md) · [架构决策](docs/adr/) · [多助手接入设计](docs/multi-harness-design.md) |

## 开发与贡献

源码构建需要 **Go 1.25.8+**；运行 npm 开发与分发命令另需 **Node.js 20+**。

```powershell
git clone https://github.com/TeamSillyBees/Blip.git
cd Blip
go build -o bin/blip.exe ./cmd/blip
.\bin\blip.exe
```

提交改动前运行：

```powershell
go test ./...
go vet ./...
npm test
```

进程集成测试使用临时编程助手目录与本地 HTTP 服务。`go test -short ./...` 可跳过进程集成测试；具备 C 编译器时可运行 `go test -race ./...`。npm 打包与安装检查见[分发文档](docs/npm-distribution.md)。

欢迎通过 [Issues](https://github.com/TeamSillyBees/Blip/issues) 反馈问题或提出建议，也欢迎提交 Pull Request。报告接入问题时，请附上 Blip 版本、编程助手版本，以及脱敏后的 `doctor` / `status` 输出。设计约定见[需求说明](docs/requirements.md)。

## 致谢

感谢 [hellolib/agent-notify](https://github.com/hellolib/agent-notify/tree/ed2842189eff44009a7bbe946b8a1b29bbc983d4) 带来的架构启发。终端命令与表单由 [Cobra](https://github.com/spf13/cobra) 和 [Charm Huh](https://github.com/charmbracelet/huh) 提供；依赖许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
