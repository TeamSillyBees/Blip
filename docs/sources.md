# 来源管理与迁移

一个 source 表示一个 harness 的独立配置位置。`id` 是永久身份；`harness` 选择内置 adapter；`home` 定位配置与日志；`event_version` 决定新通知的协议版本。模型名称不影响 adapter，例如在 Claude Code 中使用 DeepSeek 模型仍属于 Claude Code。

当前公共层已支持来源隔离，Codex adapter 可使用多个 home。其他 harness 的进度见 [兼容性记录](compatibility.md)。

## 旧配置

`schema_version: 1` 无需修改。运行时将 `codex_home` 映射为 `codex-default`，使用事件 v1，普通读取不写回。环境变量、用户 home 和 Windows 服务捕获的缺省目录继续按旧顺序解析。

显式迁移：

```text
blip sources migrate
```

迁移使用配置编辑器的备份与并发修改检查，将已解析的绝对 home 和 `event_version: 1` 保存到来源数组。默认 Codex 继续使用队列根目录下的 `rollout-cursors.json` 和 `installation.json`；不移动状态、不重算事件 ID、不重置收据。

## 新增和管理

```powershell
blip sources add --source codex-work --harness codex --home 'D:\Agents\codex-work'
blip install codex --source codex-work
blip sources list
blip doctor --source codex-work
blip sources disable --source codex-work
blip sources enable --source codex-work
blip uninstall codex --source codex-work
blip sources remove --source codex-work
```

`add` 在需要时自动迁移旧配置，新来源默认使用事件 v2。`--home` 相对当前终端解析为绝对位置；手动 JSON 中的相对路径以配置文件目录为基准。相同 harness 的同一规范化 home 不得重复注册；已存在的符号链接别名也会检查。

保存配置后重启 worker。Windows 服务新增来源或更改读取位置时，先卸载、再安装服务以更新程序副本及授权；仅重启不会新增权限。Hook 始终调用原生程序和绝对配置路径，新增来源显式携带 `--source`；裸 `blip hook` 仍指向 `codex-default`。

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

新增来源状态在 `data/sources/<id>/`；默认 Codex 仍保留旧状态位置，新增绑定与 hook 诊断保存在其 `sources/codex-default/` 下。不要为了消除错误而删除 checkpoint 或绑定。

回退旧版本前，停止新 worker 和服务，确认无待处理 v2 事件，再恢复旧配置。只恢复 `config.json.bak` 并不足以让旧程序安全处理 v2 队列。启用 v2 前应同时升级用户 hook 程序和 Windows 服务副本。
