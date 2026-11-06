# DSH 原生运行时测试

`native-runtime-hooks.jsonl` 来自 2026-11-07 的 Windows 原生进程测试。运行时为已安装的 DeepSeek Harness `0.2.1-rc.2`，Node `24.14.0`。从安装包 `app.asar` 只读提取 Cordis Loader、SessionStore 及其依赖，未更改用户 profile。

`probe.mjs` 通过原版 Loader 加载安装器生成的插件，用原版 SessionStore 的 `append()` 发布事件，再通过真实 Cordis waterfall 分发问题。registry 成员、轮次结束原因和答案是受控测试数据，没有启动模型。测试拦截并记录原生 `spawnSync` 的 stdin 后仍执行原始函数，校验空 stdout、返回值与卸载后不再产生回调。样本路径和时间已替换，不含真实提问或错误正文。

```powershell
$env:BLIP_DSH_TEST_RUNTIME = 'C:\test\dsh-runtime' # 内含 node_modules/@deepseek-ai
go test ./cmd/blip -run '^TestDSHNativeRuntime$' -v -count=1
```

可选 `BLIP_DSH_FIXTURE_DIR` 指定脱敏样本输出目录。常规 `go test ./internal/agents` 会重放样本两次、每次回调后重建 adapter，验证同源去重与持久状态，不需要安装 DSH。

原生 profile 热更新、完整模型会话和外部渠道不属于本次验证。
