# Windows 服务

在 Windows 上，`blip install <客户端>` 会为该配置注册系统服务，设置开机自动延迟启动，并等待服务运行。服务由 Windows 服务控制管理器（SCM）管理。

下文的一键接入适用于当前源码构建。npm `0.3.0` 需要通过 `blip service install` 注册服务，再运行 `blip service start` 启动。

## 安装与启动

以当前使用编程助手的账户打开管理员 PowerShell。以 Codex 为例，接入和检查服务只需运行以下命令。

```powershell
blip install codex
blip service status
```

安装会自动找到客户端目录并接管已有的普通 Blip 后台进程。首次接收提醒前，运行 `blip configure` 设置通知渠道并选择“保存并启动”。对于 Codex 等使用 hook 的客户端，按其提示审阅并信任新 hook。

已有服务时，再执行安装命令会刷新来源权限、更新程序副本并重启服务。低层命令 `blip service install` 仅用于单独注册服务，之后可用 `blip start` 启动。

自定义配置时，所有命令使用同一个 `--config`。如果管理员终端属于另一个账户，应先在配置中将 `codex_home` 写为原用户 Codex 目录的绝对路径，再明确指定原用户的配置路径；仅指定配置文件不会改变安装账户的默认 Codex 目录：

```powershell
blip --config 'C:\Users\你的账户\AppData\Roaming\Blip\config.json' install codex
```

服务名由配置文件的绝对路径生成，例如 `Blip-0123456789abcdef`。不同配置使用不同的服务名。

## 日常管理

```powershell
blip service status      # 查询服务是否注册、运行状态及启动类型
blip status              # 查看采集和通知投递状态
blip stop                # 平稳停止服务，下次开机仍会自动启动
blip start               # 再次启动服务
blip restart             # 重启服务并加载配置
blip service uninstall   # 停止并删除服务，取消开机自启
```

安装、启动、停止和卸载需要管理员权限，服务状态查询只申请读取权限。`service uninstall` 保留原配置、Codex hook 和通知队列；如果还要解除 Codex 接入，再执行 `blip uninstall codex`。

同一配置同时运行一个工作进程。`start`、`stop`、`restart` 和 `test --wait` 优先使用已注册的服务，查询或控制服务失败时会报告错误。只有该配置尚未注册服务时，顶层启停命令才管理普通后台进程。`service start/stop/restart` 可用于明确指定 SCM。

启动和停止命令最多等待 30 秒。首次扫描较慢时，命令超时不代表服务进程已被终止，可用 `service status` 检查进度；启动阶段尚未结束时，停止和卸载命令会先等待服务可以接收停止请求。

## 账户、目录和升级

服务以 `NT AUTHORITY\LocalService` 运行，无需保存 Windows 密码；它是 Windows 内置的低权限账户。[Microsoft LocalService 说明](https://learn.microsoft.com/en-us/windows/win32/services/localservice-account)

安装器为此服务启用独立服务 SID，并只向该身份授予所需权限：读取配置及 Codex `sessions`，修改队列目录。来源 home 本身只授予不继承的属性查询与路径遍历权限，用于核对来源身份，不允许目录列表或读取其中的凭据文件。安装时若存在 `session_index.jsonl`，另外授予该文件的读取权限，以显示对话名称。原有用户权限保留，普通用户的 Codex hook 仍能入队。配置目录的读取权限支持向导原子替换配置文件；不会把整个用户目录授权给服务。

配置、队列和 Codex 目录在安装时解析为绝对路径。服务仍读取原配置文件；缺省目录使用安装时记录的值，不会改用 LocalService 的个人目录。服务模式要求本地磁盘中的独立配置和数据目录，不支持网络路径或相关路径中的目录联接、符号链接。

配置 v2 按 `sources` 注册多个来源，home 必须显式提供，不从 LocalService 的个人目录推断。安装元数据 v2 保存各来源的绝对位置与 adapter 声明的读取文件、子目录；停用来源不新增读取授权，空来源列表只保留公共队列和配置权限。卸载依据安装时的清单撤销本服务身份授权，即使来源后来从配置移除也可清理；旧格式安装记录继续兼容。

服务使用 `%ProgramFiles%\Blip\<服务名>\blip.exe` 中的独立副本。更新 CLI 后，用原配置执行 `blip install <客户端>` 即可升级副本。需一次刷新所有已启用来源时，在管理员终端打开向导，选择“保存并启动”。

修改渠道或设备名称后，执行 `blip restart`，或在管理员终端的向导中选择“保存并启动”即可加载。更换配置文件、`codex_home` 或 `state_dir` 的位置时，先卸载原服务，再修改设置并重新安装，以更新路径和访问权限。

新接入使用 `blip install <客户端>`。自定义目录可在同一条命令中指定 `--home`，多个同类来源可用 `--source` 命名。启用事件 v2 时，用新版 CLI 重新执行安装命令，以同步更新用户 hook 和服务副本。

Codex 名称索引不存在或因文件替换而不可读时，通知会显示“未命名对话”。索引创建后可运行 `blip install codex` 刷新读取权限。

## 恢复与诊断

服务初始化期间报告“正在启动”，工作进程就绪后才报告“正在运行”。停止或系统关机时会取消工作并等待退出，未确认送达的通知留待下次恢复。

服务不读取 Codex `hooks.json`，因此服务侧安装诊断可能显示 `unreadable_configuration`；这不等于 hook 未安装。可在原用户终端运行 `blip doctor` 检查配置与最近回调。`status` 中的来源 `state`、`error_code` 和 `degraded` 才反映采集是否受阻。

服务异常退出时，Windows 分别在 5 秒和 30 秒后尝试重启，之后停止自动重试；连续 24 小时无故障后重置故障计数。手动停止正常退出，不触发此恢复行为。

`service status` 查询的是 Windows 服务状态；通知是否正常采集与送达仍看 `blip status`。队列目录中的 `worker.log` 记录运行问题，安装时记录的队列目录中的 `service.log` 记录启动或退出问题。配置无法读取或校验时，可在服务停止后用同一 `--config` 执行 `blip run` 查看具体错误。
