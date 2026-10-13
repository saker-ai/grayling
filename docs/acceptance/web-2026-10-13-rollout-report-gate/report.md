# 放量操作报告门禁验收（2026-10-13）

## 已实现

运行中实验的放量卡在当前报告尚未读取、读取失败或无读取权限时，禁用“恢复”“人工推进”“交还自动控制”。暂停和开启人工接管仍可使用。确认这些操作前和提交确认时都会重新读取报告；若读取失败，保留确认框中的理由，且不发送操作请求。未启动实验的待命放量计划不要求提前存在分析报告。

`web/rollout_contract.mjs` 使用真实渲染和绑定代码验证三种受限动作、保留的安全动作、失败后的零 POST，以及成功读取后的恢复。`GOWORK=off go test ./web -count=1` 通过。

## 真实浏览器检查

将现有本地验收数据复制到隔离目录，在真实 HTTP 服务中由管理员提交 `exp-8` 的两阶段放量审批、第二位发布者批准、管理员创建计划，引擎将计划从 armed 启动到 running。浏览器首先读到无曝光的真实 HOLD 报告，此时人工推进按钮可用；随后只在浏览器中中止报告 GET 请求，点击刷新报告，验证首屏提示当前分析不可用，放量卡显示报告重试提示，`advanceDisabled=true`，暂停与开启人工接管仍可用。故障由浏览器注入，服务端报告本身没有停止工作。

浏览器验收图：[桌面放量卡](desktop-rollout-gate.png)、[390px 手机步骤与历史卡](mobile-rollout-gate.png)、[390px 手机操作门禁](mobile-actions.png)。手机页面的主内容宽度与视口同为 390px，步骤和历史改为记录卡后无主内容横向滚动。

## 服务端真实计划验证

`TestOperatorActionsRequireAvailableReport` 在真实 HTTP 服务和文件存储中创建、启动放量计划，然后让持久化实验合同中的指标引用失效，使服务端报告无法构建。人工推进、恢复和交还自动控制均返回 409，计划阶段和暂停/接管状态未被这些失败动作改变；暂停及开启人工接管仍成功。恢复有效合同后，三种动作可以继续执行。该恢复场景没有曝光，报告结论仍为 HOLD，验证“报告可构建”与“健康且可自动推进”是不同门槛；人工推进仍由具备发布权限的操作者负责并记录审计。

`GOLAB_TEST_PG_SKIP=1 GOWORK=off go test ./internal/rollout -run 'TestOperatorActionsRequireAvailableReport|TestRolloutManualTakeoverPauseAdvance' -count=1` 通过。尚未在真实浏览器中以服务端报告构建失败的故障注入执行按钮提交；这一端到端拒绝路径由上述 HTTP 测试覆盖。
