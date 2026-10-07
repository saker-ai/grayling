# OpenFeature Go Provider 规范验收（Issue #17）

本页将 `pkg/ofprovider` 和官方 `github.com/open-feature/go-sdk v1.19.0` 按 [OpenFeature Specification v0.9.0](https://openfeature.dev/specification/) 的 Provider、Evaluation Context 与 Events 章节核对。这里是**逐项契约验收**，不是对整个 v0.9.0 规范或 Go SDK 的完整符合性声明。执行 `GOWORK=off go test -race ./pkg/ofprovider -count=1`；CI 客户端兼容工作流也运行该命令。依赖版本由 [`client-compatibility-matrix.json`](client-compatibility-matrix.json) 固定。

| v0.9.0 条款 | 可观察断言 / 证据 | 状态 |
| --- | --- | --- |
| 2.1.1、2.2.1、2.2.2.1：Provider 元数据与类型化解析 | `Provider` 的接口编译期断言；`TestOFTypedResolutionContract` 通过官方 SDK API 检查 bool、string、float、int、object 的值与变体 | 已覆盖 |
| 2.2.3、2.2.6、2.2.7：成功值不带错误码；失败有错误码 | `TestOFTypedResolutionContract` 检查正常解析、类型不匹配及调用方默认值；`TestOFErrorCodes` 检查 `FLAG_NOT_FOUND`、`TARGETING_KEY_MISSING`；`TestOFNumericMapping` 检查小数请求 int 的 `TYPE_MISMATCH` | 已覆盖 |
| 2.2.5、2.2.9、2.2.10：reason 与 metadata | `TestOFReasonAndVariantMapping` 检查 DEFAULT/TARGETING_MATCH/SPLIT 等映射；`TestOFTypedResolutionContract` 检查 ERROR；`TestOFExperimentExposureAttribution` 检查决策 metadata 与曝光归因 | 已覆盖 |
| 3：上下文转换 | `TestOFContextTransformation` 检查 `targetingKey` 映射为稳定 `user_id`，显式 `user_id` 属性优先，未命中时走默认规则 | 已覆盖 |
| 5.1：Provider 事件与 2.8.1：状态迁移 | `TestProviderLifecycleEvents` 检查 READY、ERROR、STALE、配置变更与恢复事件、错误码及重复事件抑制；`TestOFProviderStateLifecycle` 检查官方 SDK 客户端观察到的 READY→STALE 和陈旧求值降级 | 已覆盖已声明的动态状态 |
| 2.8.2–2.8.3：初始化方法返回前必须发 READY/ERROR | 当前 `Init` 启动异步同步监控，冷启动可在无快照时正常返回；首次 READY/ERROR 在其后发送。`TestOFNotReadyWithoutInit` 和 `TestOFInitValidation` 约束当前行为，但**不满足** v0.9.0 的初始化事件时序。需要改造初始化/等待语义及官方 Go SDK 的事件配合，再增加时序断言 | 已知缺口 |
| 2.8.4：上下文重整事件 | Provider 按调用逐次使用动态上下文，没有 `on context changed` 生命周期方法 | 不适用 |

GoLab 内部的 `NOT_ENROLLED`、`EXPERIMENT_NOT_RUNNING` 映射为 OpenFeature `DEFAULT`，原值保留在 `FlagMetadata.golabReason`；`MISSING_BUCKET_KEY` 映射为 `TARGETING_KEY_MISSING`。object 解析只接受 JSON 对象或数组，标量与 `null` 返回调用方默认值和 `TYPE_MISMATCH`。这些映射由上表测试锁定。

`SetProviderAndWait` 应由调用方设置截止时间；Provider 的首次同步为异步过程。由于上述 2.8 初始化时序缺口，本实现目前只能声明通过表中已覆盖的 v0.9.0 Provider 契约项，不能标作 v0.9.0 全量符合。
