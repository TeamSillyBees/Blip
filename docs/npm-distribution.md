# npm 安装与分发

Blip 的 npm 包包含一个轻量 Node.js 启动器，并通过 npm 的可选依赖安装 Windows x64 的 Go 可执行文件。用户不需要 Go 编译器，安装期间不执行下载脚本。命令名称为 `blip`；后台进程直接运行本地 Go 程序，Codex hook 通过固定脚本转发到该程序，不依赖 Node.js 启动器或 PATH 查找。

在 Windows x64 上使用 `npm install -g @teamsillybees/blip@latest` 安装正式版。`0.3.1` 起，Antigravity 桌面版和 `agy` CLI 通过原生 Hook 接收通知。从 `0.3.0` 升级后，按使用的客户端运行 `blip install antigravity` 或 `blip install agy` 注册回调。启动器和 Windows x64 原生包使用同一版本。[Antigravity 接入说明](antigravity.md)

## 本地构建并安装

构建机器需要 Go 1.25.8+、Node.js 20+ 和 npm。在 Windows x64 上构建当前平台：

```powershell
npm run build:npm:current
```

输出位于 `dist/npm/`，包含启动器和当前平台原生包。Windows x64 的本地安装命令为：

```powershell
npm install --global --offline --include=optional ./dist/npm/teamsillybees-blip-win32-x64-0.3.1.tgz ./dist/npm/teamsillybees-blip-0.3.1.tgz
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

配置和队列位于用户配置目录，与 npm 包分开。升级前先执行 `blip stop`，避免 Windows 正在运行的可执行文件被占用；升级后打开 `blip`，选择“保存并启动”以刷新接入、启动并测试。也可单独运行 `blip install codex`（其他来源加 `--source ID`）刷新 Codex hook 的程序路径；仅更新 npm 包不会自动刷新该路径。固定入口及注册定义不随 Node/npm 路径变化；从旧版直接命令迁移时需要审阅一次新入口，之后按 agent 对实际定义变更的提示确认信任。卸载前执行 `blip stop`、`blip uninstall codex`，再卸载 npm 包；配置和历史收据仍保留在用户目录。

服务使用独立的程序副本。升级到包含一键接入的版本后，在当前账户的管理员终端执行 `blip install <客户端>`，即可更新副本、刷新该来源的权限并启动服务。仍使用 npm `0.3.0` 时，按旧版的 `service uninstall`、`service install`、`service start` 步骤更新副本。卸载 Blip 时使用 `blip service uninstall` 移除服务。[Windows 服务](windows-service.md)

需要长期使用 hook 时应使用持久安装。临时 `npx` 缓存目录可能被清理；固定入口仍需可用的目标程序，更新位置后应重新运行安装接入命令。若 npm 被配置为省略可选依赖，启动器会提示使用 `--include=optional` 重新安装。

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
npm publish ./dist/npm/teamsillybees-blip-win32-x64-0.3.1.tgz --access public --tag latest
npm publish ./dist/npm/teamsillybees-blip-0.3.1.tgz --access public --tag latest
```

预发行版本单独使用 `next` 标签。主包和 Windows x64 包保留历史版本，已有的 `0.2.0-dev.0` 仍可在 Windows x64 上通过 `@next` 安装。仓库脚本不会自动执行 publish。
