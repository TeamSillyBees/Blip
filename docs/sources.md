# 来源管理与迁移

一个 source 表示一个 harness 的独立配置位置。`id` 是永久身份；`harness` 选择内置 adapter；`home` 定位配置与日志；`event_version` 决定新通知的协议版本。模型名称不影响 adapter，例如在 Claude Code 中使用 DeepSeek 模型仍属于 Claude Code。

Codex adapter 可使用多个 home。其他内置来源包括 [Antigravity 与 agy CLI](antigravity.md)、[Claude Code](claude-code.md)、[Hermes](hermes.md) 和 [DSH](dsh.md)。各 harness 的实测范围见 [兼容性记录](compatibility.md)。

## 旧配置

`schema_version: 1` 无需修改。运行时将 `codex_home` 映射为 `codex-default`，使用事件 v1，普通读取不写回。环境变量、用户 home 和 Windows 服务捕获的缺省目录继续按旧顺序解析。

显式迁移：

```text
blip sources migrate
```

迁移使用配置编辑器的备份与并发修改检查，将已解析的绝对 home 和 `event_version: 1` 保存到来源数组。默认 Codex 继续使用队列根目录下的 `rollout-cursors.json` 和 `installation.json`；不移动状态、不重算事件 ID、不重置收据。

## 新增和管理

日常接入使用 `blip install <客户端>`。命令会找到默认目录、启用来源、安装客户端所需的 hook 或插件，并启动监听。Windows 在当前账户的管理员终端执行，首次接入会注册开机自动启动的服务。可用客户端为 `codex`、`claude-code`、`hermes`、`dsh`、`antigravity`（桌面版）和 `agy`（CLI）。

```powershell
blip install codex --source codex-work --home 'D:\Agents\codex-work'
blip sources list
blip doctor --source codex-work
blip sources disable --source codex-work
blip sources enable --source codex-work
blip uninstall codex --source codex-work
blip sources remove --source codex-work
```

`install` 在新增来源时自动迁移旧配置，新来源使用事件 v2。`--home` 相对当前终端解析为绝对位置，手动 JSON 中的相对路径以配置文件目录为基准。重复安装沿用原来的来源 ID、目录及进度。`--register-only` 可用于只登记来源和安装客户端接入的脚本。

`blip sources` 在交互终端列出来源，脚本使用 `blip sources list` 获取 JSON。使用 `sources add` 手动登记的来源，可用 `blip install <客户端> --source <ID>` 安装接入、刷新服务读取权限并启动。Claude Code、Hermes 和 DSH 各只有一个已配置来源时可省略 `--source`，有多个时命令会列出可选 ID。安装 Codex 时默认使用 `codex-default` 或接入默认目录，其他目录可用 `--source` 指定。卸载 Codex 时省略 `--source` 始终清理 `codex-default` 的接入。Codex 回调直接调用原生程序并显式传入来源 ID，自定义配置还会传入对应的配置路径。

来源停用后，新 hook 被忽略，旧队列继续投递。Codex collector 的 checkpoint 保留，重新启用会补收持久日志中的增量；纯 hook 来源无法补收停用时忽略的回调。

删除配置不删除安装清单、绑定或历史状态，即使已经 `remove`，`uninstall` 仍可定位原位置。ID 只能代表原来的 harness/home，不可给另一个环境复用。需要更换 home 时使用新的 ID；`codex-default` 专用于旧默认 Codex。

## v2 配置

```json
{
  "schema_version": 2,
  "device_name": "开发电脑",
  "sources": [
    {"id": "codex-default", "harness": "codex", "event_version": 1, "enabled": true, "home": "C:/Users/Alice/.codex"},
    {"id": "codex-work", "harness": "codex", "event_version": 2, "enabled": true, "home": "D:/Agents/codex-work"}
  ],
  "destinations": []
}
```

与原设备、队列、超时和渠道字段组合使用；示例目的地为空，需配置后才会实际发送。v2 不再使用顶层 `codex_home`。`sources: []` 表示不采集任何来源，已入队通知仍可投递。

默认 Codex 可显式升级：`blip sources event-version 2 --source codex-default`，随后重新执行 `blip install codex` 并在 Codex 中审阅新增的 hooks。这会影响新 webhook 事件；已入队记录保持原版本。旧正常结束和请求 ID 仍保留，升级后的重复回调不会覆盖原通知或重置收据。Codex v2 收到 Stop 后，worker 做最多 1 秒的有界合并；没有正常结果时发出 `attention_required/stop_signal`。已明确取消或已有对应输入请求时不追加泛化提醒，停止后明确继续执行则允许之后再次提醒。支持与验证边界见 `doctor` 和兼容性记录。

## 诊断和回退

`doctor` 显示配置与安装状态、能力依据、最近实际回调与错误码。`configured_unverified` 只证明安装条目存在；不证明外部信任完成或已真实触发。无法读取客户端版本时明确显示 `unknown`。

`status` 同时显示公共 `ready`、逐来源 `sources`、`degraded` 和目的地收据。来源 checkpoint 损坏时保留原文件并暂停该来源，其他来源和旧队列继续运行。未知或已移除 adapter 不影响旧事件校验与发送。

纯 hook 来源最近一次回调失败也会使 `degraded` 为 true。关联文件损坏会保存 `checkpoint-fault.json`，此后的该来源回调暂停；恢复原文件并通过校验后，下一次回调或 worker 重启会解除暂停。删除关联文件不是恢复，不会自动重建基线。已经暂停的 collector 在修复后需要重启 worker。普通无关回调不会抹掉“最近一次通知事件”的时间。

`latency.by_source_destination` 按来源和目的地报告本地接收至首次投递尝试的 P95，以及有原生时间时的信号至接收 P95。统计范围是当前保留的队列，包括离线、积压和渠道发送间隔；`samples` 为可测样本数。旧记录缺少时间时不估算，时钟倒退产生的负值列入 `skipped_clock_samples`。

`first_attempt_at` 表示发送器调度开始，在调用发送器之前持久化；不表示服务端接收或手机显示。程序若在保存标记后、网络调用前崩溃，记录中仍保留该次调度开始。重试和重启不重置它，也不改变事件发生时间。新增时间仅在本地队列外层记录，webhook v1/v2 的事件格式不受影响。

新增来源状态在 `data/sources/<id>/`；默认 Codex 仍保留旧状态位置，新增绑定与 hook 诊断保存在其 `sources/codex-default/` 下。不要为了消除错误而删除 checkpoint 或绑定。

回退旧版本前，停止新 worker 和服务，确认无待处理 v2 事件，再恢复旧配置。只恢复 `config.json.bak` 并不足以让旧程序安全处理 v2 队列。启用 v2 前应同时升级用户 hook 程序和 Windows 服务副本。
