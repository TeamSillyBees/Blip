# npm 安装与分发

Blip 的 npm 包包含一个轻量 Node.js 启动器，并通过 npm 的可选依赖选择本机平台的 Go 可执行文件。用户不需要 Go 编译器，安装期间不执行下载脚本。命令名称为 `blip`；后台进程和 Codex hook 仍直接运行本地 Go 程序。

当前源码版本为 `0.2.0-dev.1`，尚未发布。公共 npm 的 `@teamsillybees/blip@next` 仍为 `0.2.0-dev.0`；设备名称和简化向导等后续修改需先使用下面的本地构建版本。

## 本地构建并安装

构建机器需要 Go 1.25.8+、Node.js 20+ 和 npm。只构建当前平台：

```powershell
npm run build:npm:current
```

输出位于 `dist/npm/`，包含启动器和当前平台原生包。Windows x64 的本地安装命令为：

```powershell
npm install --global --offline --include=optional ./dist/npm/teamsillybees-blip-win32-x64-0.2.0-dev.1.tgz ./dist/npm/teamsillybees-blip-0.2.0-dev.1.tgz
blip version
blip configure
```

上述两个本地包需要一起安装；`--offline` 保证该验证只使用本地 tarball 和缓存，不访问 npm 注册表。安装到默认全局目录需要该目录的写入权限，也可以使用 npm 的 `--prefix` 指定自己的安装目录并将其命令目录加入 PATH。测试脚本只使用临时 prefix，不改变用户现有全局安装。

构建所有平台：

```powershell
npm run build:npm
```

| 平台 | 架构 | 原生包 |
| --- | --- | --- |
| Windows | x64 | `@teamsillybees/blip-win32-x64` |
| Windows | ARM64 | `@teamsillybees/blip-win32-arm64` |
| Linux | x64 | `@teamsillybees/blip-linux-x64` |
| Linux | ARM64 | `@teamsillybees/blip-linux-arm64` |
| macOS | Intel | `@teamsillybees/blip-darwin-x64` |
| macOS | Apple Silicon | `@teamsillybees/blip-darwin-arm64` |

也可单独构建，例如 `npm run build:npm -- --target darwin-arm64`。这些是分发构建目标；Codex 接入的首版实机验收仍以 Windows 为准。

## 配置和升级

```powershell
blip configure
blip install codex
blip start
blip test
blip status
```

向导只保存配置，不会自动安装 hook 或发出消息。执行 `install codex` 后，在 Codex 中审阅并信任 hook。`configure --accessible` 提供适合屏幕阅读器的提示，仍需要交互终端；自动化脚本可使用 `blip init` 和 JSON 配置。

配置和队列位于用户配置目录，与 npm 包分开。升级前先执行 `blip stop`，避免 Windows 正在运行的可执行文件被占用；升级后重新执行 `blip install codex` 和 `blip start`，并根据 Codex 提示确认 hook 信任。卸载前执行 `blip stop`、`blip uninstall codex`，再卸载 npm 包；配置和历史收据仍保留在用户目录。

需要长期使用 hook 时应将包安装到稳定位置。临时 `npx` 缓存目录可能被清理，不适合作为 hook 的固定程序路径。若 npm 被配置为省略可选依赖，启动器会提示使用 `--include=optional` 重新安装。

## 验证和发布准备

```powershell
npm test
npm run build:npm:current
npm run test:npm-install
```

安装测试从本地 tarball 安装到临时目录，使用隔离的配置目录和 npm 缓存，验证版本、帮助、带空格路径、退出码、npm 命令入口以及缺少原生包时的诊断。它不发布包，不修改真实 Codex 配置，也不发送真实通知。

发布时，根 `package.json` 与 `npm/blip/package.json` 的版本必须一致，启动器的全部原生可选依赖也必须指向同一版本。打包命令将该版本写入 Go 可执行文件，生成 `dist/npm/manifest.json` 记录各 tarball 和完整性哈希。

具备 `@teamsillybees` scope 发布权限后，先发布六个原生包，再发布启动器包。当前是预发行版本，发布时使用单独的 dist-tag，例如：

```powershell
npm publish ./dist/npm/teamsillybees-blip-win32-x64-0.2.0-dev.1.tgz --access public --tag next
# 按相同方式发布其余五个原生包，最后发布启动器：
npm publish ./dist/npm/teamsillybees-blip-0.2.0-dev.1.tgz --access public --tag next
```

安装已发布的预发行版使用 `npm install -g @teamsillybees/blip@next`。`0.2.0-dev.0` 首次发布后，npm 同时设置了 `latest` 和 `next`；删除 `latest` 的请求返回 HTTP 400，因此当前 `latest` 不代表正式版。正式版本发布后再将 `latest` 指向正式版本。仓库脚本不会自动执行 publish。
