# Windows 服务

从 `0.2.0-dev.2` 起，Blip 可注册为真正的 Windows 服务，由服务控制管理器（SCM）运行。普通 `blip start` 仍只启动当前用户的后台进程；只有显式执行 `blip service install` 才会注册开机自启。

当前源码版本尚未发布到 npm。先从源码构建，或使用对应版本的本地安装包。

## 安装与启动

先在正常用户终端配置通知渠道、安装 Codex hook，并停止已有后台进程：

```powershell
.\bin\blip.exe configure
.\bin\blip.exe install codex
.\bin\blip.exe stop
```

在 Codex 中审阅并信任 hook。然后以同一个 Windows 账户打开管理员 PowerShell，进入 Blip 目录，执行：

```powershell
.\bin\blip.exe service install
.\bin\blip.exe service start
.\bin\blip.exe service status
```

`install` 注册服务并设置自动（延迟启动），不会立即启动；`start` 启动现有服务并等待就绪。服务注册后，Windows 重启时会自动启动它，无需保持终端打开或登录桌面。

自定义配置时，所有命令使用同一个 `--config`。如果管理员终端属于另一个账户，应先在配置中将 `codex_home` 写为原用户 Codex 目录的绝对路径，再明确指定原用户的配置路径；仅指定配置文件不会改变安装账户的默认 Codex 目录：

```powershell
.\bin\blip.exe --config 'C:\Users\你的账户\AppData\Roaming\Blip\config.json' service install
.\bin\blip.exe --config 'C:\Users\你的账户\AppData\Roaming\Blip\config.json' service start
```

服务名由配置文件的绝对路径生成，例如 `Blip-0123456789abcdef`。不同配置使用不同服务名；重复安装已有服务会被拒绝，不会覆盖其他安装。

## 日常管理

```powershell
blip service status      # 查询服务是否注册、运行状态及启动类型
blip status              # 查看采集和通知投递状态
blip service stop        # 平稳停止服务；下次开机仍会自动启动
blip service start       # 再次启动服务
blip service uninstall   # 停止并删除服务，取消开机自启
```

安装、启动、停止和卸载需要管理员权限，服务状态查询只申请读取权限。`service uninstall` 保留原配置、Codex hook 和通知队列；如果还要解除 Codex 接入，再执行 `blip uninstall codex`。

同一配置同时只允许运行一个工作进程。安装服务后使用 `service start/stop` 管理它，避免与普通后台方式混用。

启动和停止命令最多等待 30 秒。首次扫描较慢时，命令超时不代表服务进程已被终止，可用 `service status` 检查进度；启动阶段尚未结束时，停止和卸载命令会先等待服务可以接收停止请求。

## 账户、目录和升级

服务以 `NT AUTHORITY\LocalService` 运行，无需保存 Windows 密码；它是 Windows 内置的低权限账户。[Microsoft LocalService 说明](https://learn.microsoft.com/en-us/windows/win32/services/localservice-account)

安装器为此服务启用独立服务 SID，并只向该身份授予所需权限：读取配置及 Codex `sessions`，修改队列目录。安装时若存在 `session_index.jsonl`，另外授予该文件的读取权限，以显示对话名称；不会授权读取整个 Codex 目录。原有用户权限保留，普通用户的 Codex hook 仍能入队。配置目录的读取权限支持向导原子替换配置文件；不会把整个用户目录授权给服务。

配置、队列和 Codex 目录在安装时解析为绝对路径。服务仍读取原配置文件；缺省目录使用安装时记录的值，不会改用 LocalService 的个人目录。服务模式要求本地磁盘中的独立配置和数据目录，不支持网络路径或相关路径中的目录联接、符号链接。

配置 v2 按 `sources` 注册多个来源，home 必须显式提供，不从 LocalService 的个人目录推断。安装元数据 v2 保存各来源的绝对位置与 adapter 声明的读取文件、子目录；停用来源不新增读取授权，空来源列表只保留公共队列和配置权限。卸载依据安装时的清单撤销本服务身份授权，即使来源后来从配置移除也可清理；旧格式安装记录继续兼容。

服务使用 `%ProgramFiles%\Blip\<服务名>\blip.exe` 中的独立副本。更新普通 CLI 或 npm 包不会自动更新正在运行的服务程序。升级服务时：

1. 使用原配置路径执行 `blip service uninstall`。
2. 更新 CLI 或重新构建程序。
3. 执行 `blip service install`、`blip service start`。

修改渠道或设备名称后，停止并启动服务即可加载。更换配置文件、`codex_home` 或 `state_dir` 的位置时，先卸载原服务，再修改设置并重新安装，以更新路径和访问权限。

新增来源、变更来源读取路径或启用此前未授权的来源，也须重新安装服务。启用事件 v2 前应一起升级用户 hook 程序与服务程序副本，避免旧服务把新事件当作不支持的格式。

从旧版本升级到精简通知模板时，同样需要卸载后重新安装服务以更新程序副本及名称索引权限。名称索引不存在或因替换文件而不可读时，通知会显示“未命名对话”；索引创建后可重新安装服务来授予读取权限。

## 恢复与诊断

服务初始化期间报告“正在启动”，工作进程就绪后才报告“正在运行”。停止或系统关机时会取消工作并等待退出，未确认送达的通知留待下次恢复。

服务异常退出时，Windows 分别在 5 秒和 30 秒后尝试重启，之后停止自动重试；连续 24 小时无故障后重置故障计数。手动停止正常退出，不触发此恢复行为。

`service status` 查询的是 Windows 服务状态；通知是否正常采集与送达仍看 `blip status`。队列目录中的 `worker.log` 记录运行问题，安装时记录的队列目录中的 `service.log` 记录启动或退出问题。配置无法读取或校验时，可在服务停止后用同一 `--config` 执行 `blip run` 查看具体错误。
