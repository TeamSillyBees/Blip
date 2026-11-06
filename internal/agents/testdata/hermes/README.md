# Hermes shell hook 样本

`native-shell-hooks.jsonl` 基于本机 Hermes 源码提交 [`4d04303818b61bf3aa09278af48356315515965b`](https://github.com/NousResearch/hermes-agent/tree/4d04303818b61bf3aa09278af48356315515965b)，Python 3.14.7、Windows x64，2026-11-07 采集。

`probe.py` 在临时 profile 中调用原版 consent registry、plugin dispatcher、shell serializer 和 subprocess runner。clarify 使用原版工具与本地回答回调，审批使用原版 human-input context manager，因此 request/resolved 的字段来自真实实现。轮次结束、异常、中断和子 agent 的 kwargs 根据固定源码构造，通过原版 `invoke_hook` 分发；它们不等同于一次真实模型会话的全流程实测。没有调用模型 API、终端命令或外部通知服务。

仅将路径与随机 request ID 替换为固定测试值。记录中的 `profile` 与 `extra.telemetry_schema_version` 由 Hermes 生成。安装命令经过真实 `split_command_line` 与 `_spawn`，路径包含中文、空格、`$`、`&`、单引号。常规 Go 回归会重放这些样本，并在每个回调之间重建 adapter。

重跑：设置 `BLIP_HERMES_TEST_PYTHON` 为 Hermes 的 Python、`BLIP_HERMES_TEST_REPO` 为固定源码目录，运行 `go test ./cmd/blip -run TestHermesNativeShellHooks -count=1 -v`。可选 `BLIP_HERMES_FIXTURE_DIR` 指定重新采样目录；审核后再提交。
