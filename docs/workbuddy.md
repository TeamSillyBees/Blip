# WorkBuddy 接入门槛

2026-11-07：用户确认尚未安装 WorkBuddy 桌面版。当前没有注册 `workbuddy` adapter；手工配置未知来源时，`doctor/status` 报告 `adapter_unavailable`，不阻断其他来源及已有队列投递。`sources add` 拒绝创建尚未实现的 adapter。

后续应在隔离的桌面版 profile 上收集下列材料，确认协议后复用现有队列、渠道和来源边界。

| 待验证项 | 需要的证据 |
| --- | --- |
| 目标环境 | 桌面版本、平台、具体配置目录和插件加载方式 |
| 程序调用 | 可执行文件/argv 或实际 shell；stdin、stdout、退出码、超时及中文与特殊字符路径 |
| 本轮结束 | 正常结束、结果未知、自动异常、主动取消、Stop 后继续执行的脱敏原始回调 |
| 人工响应 | 实际等待输入与审批的回调、请求身份、回答/取消后的解除信号 |
| 会话身份 | 根会话、原生轮次、子 agent、恢复会话和多 profile 的身份规则 |
| 安装归属 | 保留现有插件、显式关闭与信任设置；幂等安装、冲突检测、备份和精准卸载 |

仅有停止信号时可声明 `signal-only` 并提醒查看；不得把它解释为成功完成。没有真实输入或审批事实时，对应能力保持 `unavailable`。模型正文提问后正常结束可由结束提醒覆盖。

官方 [WorkBuddy 插件说明](https://www.codebuddy.cn/docs/workbuddy/From-Beginner-to-Expert-Guide/Function-Description/Plug-In) 是调查入口。CodeBuddy CLI 的配置、事件字段或验证结果不能证明 WorkBuddy 桌面版兼容；不预先生成可能改变真实桌面配置的安装条目。
