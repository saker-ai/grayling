# golab

面向 **AI 应用**的灰度发布与 A/B 实验平台。

## 定位

- **功能对标** [Unleash](https://github.com/Unleash/unleash) 与 [GrowthBook](https://github.com/growthbook/growthbook)，覆盖其功能集（含商业版能力）
- **评估内核**复用 [go-feature-flag](https://github.com/thomaspoignant/go-feature-flag) 的 `modules/core`
- **控制面底座**复用 [gonacos](https://github.com/godeps/gonacos)（存储 / 推送 / 集群 / AI 资源版本）
- **差异化**：AI 资源（prompt / model / skill / AgentSpec / MCP / pipeline）的版本灰度、分层灰度、成本与质量指标、LLM-as-judge

传统 flag 平台灰度的是**代码路径**；golab 灰度的是**决定 AI 行为的资源版本** —— 这是它区别于既有方案的核心。

## 状态

**设计阶段**。当前仓库包含完整技术方案，尚无实现代码。

## 文档

- [技术方案](docs/技术方案.md) —— 完整方案，含：
  - 功能对标矩阵（Unleash / GrowthBook / go-feature-flag 逐项对照）
  - 整体架构与数据模型
  - 评估引擎（分桶、规则匹配、评估原因）
  - 指标与统计引擎（双引擎、CUPED、序贯检验、Bandits、LLM-as-judge）
  - AI 灰度能力（分层灰度、指标驱动的自动推进与回滚）
  - 集成与接口设计、实施阶段、风险与权衡

## 关键设计约束

以下结论来自对 gonacos 与 go-feature-flag 的**源码实测**，实现时须遵守：

| 约束 | 说明 |
|---|---|
| 不可调用 `SetPushFunc` / `SetSyncFunc` | 二者是直接赋值且已被 gonacos 的 `server.New` 占用，调用会静默破坏其推送与集群同步 |
| 授权层须自建 | gonacos 的细粒度鉴权（`Auth.Authorize`）无调用点，非 admin 路径匿名放行 |
| 契约路径用 `WithRoot` | 不是 `WithDataDir`（后者是内嵌 Redis 的 dump 目录） |
| 分桶用固定模 10000 | 不采用 gff 的变模设计（改比例会重排用户、制造 SRM） |
| flag DSL 迁移是单向的 | gff 配置可导入 golab；golab 配置含私有扩展字段，不能被 gff 消费 |
| gonacos 尚无正式 LICENSE | 其 README 声明 MIT（占位待确认），公开发布前须落地 |

## License

Apache-2.0（待添加 LICENSE 文件）