# Blip 通知领域

Blip 将 coding agent 中需要用户关注的事件传递到用户选择的通知目的地。

## Language

**会话（Session）**：用户与 coding agent 持续交互的上下文，包含一个或多个轮次。
_Avoid_: 将会话与单次任务混用

**轮次（Turn）**：coding agent 响应一次用户请求的执行过程。
_Avoid_: 将轮次结束称为整个业务目标成功

**轮次完成（Turn completed）**：一个轮次正常结束；不包含中断、取消和失败。
_Avoid_: 任务成功、空闲即完成

**输入请求（Input requested）**：coding agent 通过结构化提问向用户索取回答或选择。
_Avoid_: 正文中任何问句、所有需要审批的请求

**审批请求（Approval requested）**：coding agent 为执行受限操作发起的授权申请。
_Avoid_: 将已观察到申请称为已确认仍待审批

**通知目的地（Destination）**：用户配置的一个具体通知接收端。同一种通知渠道可以有多个目的地。
_Avoid_: 将通知渠道与单个接收端混用

**通知渠道（Channel）**：通知目的地所属的服务类型，首版包含飞书、企业微信、Gotify 与自定义 webhook。
_Avoid_: 将渠道与 coding agent 类型混用

**投递（Delivery）**：把一条通知发送到一个具体通知目的地的过程。同一通知的不同投递可以分别成功或失败。
_Avoid_: 将单个目的地发送成功等同于所有目的地已接收
