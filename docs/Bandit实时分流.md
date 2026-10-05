# Bandit 实时分流与推断口径

## 注册契约

实验默认 `allocationMode: "fixed-split"`。选择 `"bandit"` 时须同时注册 `bandit`：

```json
{
  "allocationMode": "bandit",
  "analysisMethod": "bandit-beta-bernoulli",
  "bandit": {
    "policyVersion": "bandit-v1",
    "strategy": "thompson",
    "priorAlpha": 1,
    "priorBeta": 1,
    "epsilon": 0.1,
    "feedbackDelay": 950400000000000
  }
}
```

`feedbackDelay` 是 Go `time.Duration` 的纳秒数；示例为 11 天。它必须至少等于 `analysisWindow + 72h`，确保一个成熟曝光的归因窗口和允许的迟到上报都已结束。Bandit v1 仅接收双变体 Bernoulli 比例指标。策略版本、先验、ε、反馈延迟、固定降级比例和护栏随 epoch 注册后不可原地修改；变更需新 epoch。Thompson 用固定 8192 次后验抽样估计“最优臂概率”，再混入 ε 均匀探索；ε-greedy 按后验均值选最佳臂并以 ε 均匀探索。最终向量是实际路由分布，因此事件中选中臂的 `allocationProbability` 是可解释的操作概率。

## 状态更新与求值

后台 tick（`-rollout-interval`，默认 30 秒）在每个运行中的 Bandit 实验上拿 PostgreSQL advisory lock、重载状态、从持久化原始事件重放反馈，再经正常发布锁发布新的不可变旗标 revision。原始曝光、转化和单元级聚合不一致时不更新后验，发布冻结状态。未成熟曝光记作 `pending`；成熟曝光在归因窗口内有正值主指标转化则记一次成功，否则记一次未转化。迟到反馈在下次重放中修正；水位是 `now - feedbackDelay`。没有成熟反馈前保持冻结并走注册的固定比例。

热路径只读取预编译策略向量并哈希单位 ID，分配时间不依赖历史事件数。远程求值的首次分配在 PostgreSQL `bandit_assignments` 唯一键上持久固定；跨副本并发请求与重启后都取回同一变体及首次分配概率。曝光事件记录策略版本、状态版本、概率、客户端和服务端时间。状态版本由后验计数与冻结原因确定。超过 5 分钟的离线快照、冻结状态及不认识 Bandit 字段的旧 SDK 均走注册的固定百分比分桶；旧 SDK 的上报在采集端标记为固定降级并补足注册概率与版本。Go 本地 SDK也固定分桶，Bandit 动态分配由有持久曝光账本的远程求值端承担。

## 安全与分析

自适应 SRM 按每次曝光的**当时**概率累加期望量与方差；偏离超过 4 个标准差、护栏破线、数据不完整、跨臂重复曝光或策略版本异常触发冻结。冻结只停自适应探索，继续按注册固定比例服务；修复健康数据后下轮重放可恢复。想手动回滚流量可暂停或停止实验，正常状态转换会重发旗标。

`bandit-beta-bernoulli` 报告只展示各臂成熟曝光、转化、后验均值、待反馈、未转化与固定降级数，附反馈水位和冻结原因。没有固定样本双臂 p 值、置信区间或自动胜出结论；固定样本发布计划也拒绝绑定 Bandit。控制台使用单独的 Bandit 报告视图。曝光是唯一随机化单位首次使用；定向命中不进入 Bandit 后验。反馈晚于注册归因窗口不计成功；未反馈在成熟后计失败。PostgreSQL 原始事件可重放状态；本地后端若原始事件被容量上限截断则冻结。

## 验证

`go test ./internal/bandit ./pkg/evaluation ./internal/experiment ./internal/platform ./internal/rollout ./internal/api` 覆盖跨 evaluator 确定性、旧/离线快照、后验概率、反馈延迟、乱序重放、偏置冻结与 PostgreSQL 跨副本并发固定分配及重启恢复。`go test ./pkg/evaluation -run '^$' -bench BenchmarkBanditEvaluation -benchmem` 测热路径；生产部署还应按实际事件量监测后台重放时长和发布频率。
