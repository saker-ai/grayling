# 放量操作报告门禁验收（2026-10-13）

## 已实现

运行中实验的放量卡在当前报告尚未读取、读取失败或无读取权限时，禁用“恢复”“人工推进”“交还自动控制”。暂停和开启人工接管仍可使用。确认这些操作前和提交确认时都会重新读取报告；若读取失败，保留确认框中的理由，且不发送操作请求。未启动实验的待命放量计划不要求提前存在分析报告。

`web/rollout_contract.mjs` 使用真实渲染和绑定代码验证三种受限动作、保留的安全动作、失败后的零 POST，以及成功读取后的恢复。`GOWORK=off go test ./web -count=1` 通过。

## 浏览器检查

本地服务中 `exp-8` 没有真实放量计划。浏览器打开真实实验详情后，向放量卡注入**仅用于界面验收的合成运行计划**，调用页面实际渲染、绑定和报告状态函数。报告状态设为 `unavailable` 后得到：`advanceDisabled=true`、`pauseDisabled=false`、`takeoverDisabled=false`，页面显示重试当前报告的提示。该检查不证明服务端存在这个计划，也不作为真实放量操作的证据。

## 服务端真实计划验证

`TestOperatorActionsRequireAvailableReport` 在真实 HTTP 服务和文件存储中创建、启动放量计划，然后让持久化实验合同中的指标引用失效，使服务端报告无法构建。人工推进、恢复和交还自动控制均返回 409，计划阶段和暂停/接管状态未被这些失败动作改变；暂停及开启人工接管仍成功。恢复有效合同后，三种动作可以继续执行。该恢复场景没有曝光，报告结论仍为 HOLD，验证“报告可构建”与“健康且可自动推进”是不同门槛；人工推进仍由具备发布权限的操作者负责并记录审计。

`GOLAB_TEST_PG_SKIP=1 GOWORK=off go test ./internal/rollout -run 'TestOperatorActionsRequireAvailableReport|TestRolloutManualTakeoverPauseAdvance' -count=1` 通过。尚需真实浏览器在服务端运行计划上验收完整交互；上节浏览器检查使用的是合成计划。
