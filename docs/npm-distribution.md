# npm 安装与分发

Blip 的 npm 包包含 Node.js 启动器，Windows x64 原生包随精确版本的可选依赖提供。启动器包记录原生程序的 SHA-256。全局安装或更新时，`postinstall` 校验程序并同步到 `%LOCALAPPDATA%\Programs\Blip\blip.exe`。npm 命令和助手回调都运行这份固定程序。

在 Windows x64 上使用 `npm install -g @teamsillybees/blip@latest` 安装正式版。`0.3.1` 起，Antigravity 桌面版和 `agy` CLI 通过原生 Hook 接收通知。从 `0.3.0` 升级后，按使用的客户端运行 `blip install antigravity` 或 `blip install agy` 注册回调。启动器和 Windows x64 原生包使用同一版本。[Antigravity 接入说明](antigravity.md)

## 本地构建并安装

构建机器需要 Go 1.25.8+、Node.js 20+ 和 npm。在 Windows x64 上构建当前平台：

```powershell
npm run build:npm:current
```

输出位于 `dist/npm/`，包含启动器和当前平台原生包。Windows x64 的本地安装命令为：

```powershell
npm install --global --offline --include=optional ./dist/npm/teamsillybees-blip-win32-x64-0.4.0.tgz ./dist/npm/teamsillybees-blip-0.4.0.tgz
blip version
blip
```

上述两个本地包需要一起安装；`--offline` 保证该验证只使用本地 tarball 和缓存，不访问 npm 注册表。安装到默认全局目录需要该目录的写入权限，也可以使用 npm 的 `--prefix` 指定自己的安装目录并将其命令目录加入 PATH。测试脚本只使用临时 prefix，不改变用户现有全局安装。

构建全部分发包（Windows x64 原生包和启动器）：

```powershell
npm run build:npm
```

| 平台 | 架构 | 原生包 |
| --- | --- | --- |
| Windows | x64 | `@teamsillybees/blip-win32-x64` |

也可显式指定 `npm run build:npm -- --target win32-x64`。不再接受 macOS、Linux 或 Windows ARM64 的 npm 构建目标；启动器声明 `os: ["win32"]` 和 `cpu: ["x64"]`，npm 会在不支持的平台上拒绝安装。

## 配置和升级

```powershell
blip
```

向导的“保存并启动”会安装已启用来源的 hook、启动或重启通知进程，并测试渠道投递；也可选择“仅保存并退出”。安装后仍需在对应 agent 中审阅并信任 hook。`blip --accessible` 提供适合屏幕阅读器的提示，仍需要交互终端；自动化脚本可使用 `blip init` 和 JSON 配置。非交互环境下，裸 `blip` 显示帮助。

**首次升级到固定路径版时，每台接入 Codex 的机器都要运行一次 `blip install codex`，并在 Codex 中信任新注册的回调。** 此后普通 npm 升级只同步固定程序。使用 `--ignore-scripts` 安装时，新版启动器在首次执行 `blip` 时补做同步。

同步按程序的语义版本比较新旧，多个 Node 安装共享同一份程序。校验成功后，安装器在目标目录写入临时文件再替换。校验失败或文件被占用时会报错并保留原程序。校验失败可重新安装匹配的包，遇到占用则先退出正在使用该文件的进程再重试。

解除 Codex 接入时执行 `blip uninstall codex`。它移除本配置注册的回调，固定程序仍供其他接入使用。

服务使用独立的程序副本。升级到包含一键接入的版本后，在当前账户的管理员终端执行 `blip install <客户端>`，即可更新副本、刷新该来源的权限并启动服务。仍使用 npm `0.3.0` 时，按旧版的 `service uninstall`、`service install`、`service start` 步骤更新副本。卸载 Blip 时使用 `blip service uninstall` 移除服务。[Windows 服务](windows-service.md)

若 npm 配置为省略可选依赖，可使用 `--include=optional` 重新安装启动器和原生包。

## 验证和发布准备

```powershell
npm test
npm run build:npm:current
npm run test:npm-install
```

安装测试从本地 tarball 安装到临时目录，使用隔离的配置目录和 npm 缓存，验证版本、帮助、带空格路径、退出码、npm 命令入口以及缺少原生包时的诊断。它不发布包，不修改真实 Codex 配置，也不发送真实通知。

发布时，根 `package.json` 与 `npm/blip/package.json` 的版本必须一致，启动器的全部原生可选依赖也必须指向同一版本。打包命令将该版本写入 Go 可执行文件，生成 `dist/npm/manifest.json` 记录各 tarball 和完整性哈希。

具备 `@teamsillybees` scope 发布权限后，先发布 Windows x64 原生包，再发布启动器包。仅发布本次 `manifest.json` 中列出的两个包，不使用可能包含历史构建的目录通配符。正式版本使用 `latest` 标签，例如：

```powershell
npm publish ./dist/npm/teamsillybees-blip-win32-x64-0.4.0.tgz --access public --tag latest
npm publish ./dist/npm/teamsillybees-blip-0.4.0.tgz --access public --tag latest
```

预发行版本单独使用 `next` 标签。主包和 Windows x64 包保留历史版本，已有的 `0.2.0-dev.0` 仍可在 Windows x64 上通过 `@next` 安装。仓库脚本不会自动执行 publish。
