# DSH 来源

Blip 通过一个本地 Cordis 插件接入 DeepSeek Harness。它直接观察原生 `session/event` 与 `user-questions/request`，用原生 session、turn 和请求身份归一化通知，再调用 Blip 持久入队。插件不发送网络请求，也不返回审批或继续执行决定。

## 安装

首次接入默认使用桌面 profile `~/.dsh/profiles/desktop`。其他 profile 可在安装命令后加 `--home <目录>`，该目录应包含客户端创建的 `cordis.yml`。

```powershell
blip install dsh
```

安装器在该 profile 的 `cordis.patch.yml` 增量插入唯一条目，在 `.blip-hooks/<source>/` 写入内容寻址的 `.mjs` 文件。插件使用 DSH 自带 Node 运行时，原生 Blip 程序和配置路径由安装器写为绝对路径，不依赖 PATH 或 shell 转义。重启 DSH profile 后验证回调；`configured_unverified` 不代表它已加载。

现有 patch、注释和 `!!js` 表达式按 YAML 节点保留，不执行表达式，不改变信任或其他插件的启用状态。更新前保存备份及安装清单；已登记条目或脚本被手工修改时拒绝覆盖。使用 `blip uninstall dsh --source dsh-desktop` 删除清单中确认归属的条目和脚本。清单及用户配置之外的目录不递归删除。

## 通知含义

| 原生事实 | 提醒 |
| --- | --- |
| `turn/end`，`reason.kind=completed` | 本轮结束 |
| `turn/end`，`error` | 异常，需要查看；只附错误码，不复制完整错误正文 |
| `blocked`、`max-tokens`、hook 中止或未知结束原因 | 已停止，需要查看 |
| 用户、父任务或销毁导致的取消；历史修复或 fork 边界 | 不提醒 |
| 实际 `user-questions/request` | 需要输入；带 `approval` intent 时为审批申请 |

同一轮存在未回答请求时，不追加泛化结束提醒。限时问题返回 `ASK_TIMED_OUT` 后仍可回答，不能当作已经解决。已被请求提醒覆盖的停止记录，即使之后重放也不再产生完成通知；明确继续执行或新的原生轮次可再次提醒。

子任务过滤优先使用运行时的根 agent 列表；较早接口使用 session 的 `origin=subagent`。普通 fork 的 `parentSession` 本身不表示子任务。新接口允许曾经的子会话以独立根会话恢复后收到提醒。缺少可核验的 live agent 或原生轮次时，不猜测所属身份。

原生请求提供 `wait.callId` 时用它去重；旧同步提问没有该字段时，插件为本次调用分配 UUID。它标识一次回调，无法保证不同进程重建的同一个旧式请求不重复。安装之前及插件未加载时的事件不会补收。

## 验证边界

2026-11-07 在 Windows、DeepSeek Harness `0.2.1-rc.2` 提取的原版模块上验证了 Cordis Loader、SessionStore 的真实事件发布、waterfall 分发、原生程序启动和队列写入。13 次回调覆盖正常结束、异常、用户取消、输入、审批、限时未答、子任务、恢复为根会话以及 hook 中止；每次回调后重启 adapter 的重复回放测试通过。路径包含中文、空格、`$`、`&` 和单引号。

该验证使用受控 registry 和问题答案，未启动真实模型会话、未在真实桌面 profile 安装、未向真实渠道发消息。仅声明该原生接口路径的能力；不保证所有审批插件都会经过 `user-questions/request`。可复跑脚本和脱敏样本见[测试记录](../internal/agents/testdata/dsh/README.md)。

官方 Claude bridge 的 [字段与覆盖说明](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/experimental/hooks-claude-code/README.md) 明确存在缺口；它没有保留本实现需要的轮次、结果及完整身份，因此没有复用 Claude Code parser。原生协议依据：[session 事件](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/core/session/src/types.ts)、[用户提问](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/interaction/user-questions/src/types.ts)。
