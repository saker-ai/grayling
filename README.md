# golab

GoLab 是通用功能灰度与 A/B 实验平台，为传统后端、Web 应用与 AI Agent 提供变体决策、可信实验分析，以及 AI 资源（prompt/模型/skill/AgentSpec）的版本绑定、会话级配置冻结与紧急撤销。

**状态：首版已实现并可运行。** 单节点闭环（管理 API → gonacos 配置分发 → Go SDK 本地求值 → 事件采集 → 统计报告 → AI 资源灰度）全部落地并有端到端测试；多语言 SDK、OpenFeature、PostgreSQL 后端、自动推进与高级统计尚未实现。逐项完成范围见 [实施状态](docs/实施状态.md)，端点清单见 [API 参考](docs/api.md)，设计依据见 [技术方案](docs/技术方案.md)。

## 快速开始

```bash
# 前置：Go 1.27+，且 godeps/gonacos 位于 ../gonacos（go.mod replace 指向 sibling 仓库）
make build          # 或 GOWORK=off go build ./cmd/golab
./bin/golab -addr 127.0.0.1:8688 -gonacos-root ../gonacos
```

启动日志会打印一次性的 **global owner token**（`glab_admin_...`）。浏览器打开 `http://127.0.0.1:8688/` 进入控制台，填入该 token 即可创建工作空间/环境、编辑与发布 flag、注册实验、查看报告与审计。

> 注意：本仓库处于父级 Go workspace 内，独立构建必须 `GOWORK=off`（Makefile 已内置）。gonacos 底座 HTTP/gRPC 默认仅绑定 loopback，租户流量只应走 GoLab API。

## SDK 接入（Go）

```go
client, err := sdk.New(ctx, sdk.Options{
    BaseURL:      "http://127.0.0.1:8688",
    Token:        sdkToken,      // 环境级 sdk 凭证（工作空间/环境由凭证决定）
    CollectToken: collectToken,  // 独立采集凭证，用于曝光/转化上报
})
client.Start(ctx)
defer client.Close()

enabled, res := client.BoolVariation("checkout-router",
    model.EvaluationContext{Attributes: map[string]any{"user_id": uid}}, false)
if res.TrackEvents && res.Experiment != nil {
    // 已进入实验并分配变体；曝光已由 SDK 自动批量上报
    _ = sdk.TrackConversion(client, evalCtx, res, "conversion", 1, sessionID, "")
}
```

求值在本地快照上完成（无每请求网络调用，均值 ~2µs/次）；SSE 推送 + 轮询兜底同步；快照超过 `MaxStaleness` 自动降级为调用方默认值并给出 `STALE` 原因。AI 应用可用 `POST /api/v1/sdk/ai/session` 冻结会话级配置（flag 求值结果 + 摘要校验过的资源版本），执行前用 `POST /api/v1/sdk/ai/check` 做紧急撤销门禁。

## 架构

```
管理端/控制台 ──Admin API──> GoLab 控制面 ──不可变 revision+manifest──> gonacos Config
                                  │                                      │
                                  ├─ RBAC/二人审批/审计/限额              └─ SSE/ETag/轮询
                                  └─ 平台存储(实验/事件/审计/AI 索引)            │
应用后端 <──Go SDK 本地求值<──已验证快照原子替换<────────────────────────────┘
AI 应用 ──会话冻结──> 授权资源解析(所有权索引+版本 pin+摘要校验+实时撤销)
事件采集 ──去重/伪名化/单元聚合──> Welch/比例检验 + SRM + 固定样本决策
```

- **稳定分桶** `golab-fnv1a-10000-v1`：长度前缀字段拼接 + FNV-1a 64 + %10000；eligibility/variant/layer 三坐标分离；跨语言黄金向量（经独立实现交叉验证）位于 `pkg/bucketing/testdata/`。
- **发布语义**：flag 文档写入 `golab.flag.<key>.r<rev>`（不可覆盖）→ 读回校验 → manifest 更新 → 读回 → SSE 通知。回滚是发布指向已验证旧 revision 的新 manifest revision。
- **实验语义**：预注册（单元/比例/主指标/护栏/α/power/MDE/最短运行/分析窗口），epoch 永不复用，互斥层槽位不重叠；SRM 只统计随机化（split）曝光单元；异常数据只出 `hold`，不出胜负结论。

## 仓库布局

| 路径 | 内容 |
|---|---|
| `pkg/bucketing` | 分桶规范实现 + 黄金向量 |
| `pkg/model` | Flag DSL/segment/快照/manifest 类型与校验 |
| `pkg/evaluation` | 原生求值器（规则/segment/百分比/实验门控/曝光语义） |
| `pkg/sdk` | Go SDK（同步/求值/曝光上报） |
| `internal/platform` | 控制面存储（工作空间/凭证/草稿/revision/实验/事件/审计/AI 索引） |
| `internal/configstore` | gonacos Config 适配（不可变 revision、manifest、读回验证） |
| `internal/admin` | RBAC + 发布流水线 + 实验生命周期 |
| `internal/api` | HTTP 面：admin / sdk config+eval+stream / collect / ai |
| `internal/experiment` | 报告构建（单元聚合→统计→决策门） |
| `internal/stats` | Welch/两比例 z/SRM χ²/固定样本决策（纯标准库，含模拟验证） |
| `internal/ai` | AI 资源门禁（所有权索引/版本 pin/摘要复核/撤销） |
| `internal/gffimport` | gff 严格导入 + legacy-gff-v1 对照求值 |
| `cmd/golab`、`web/` | 服务入口与内嵌控制台（无构建步骤） |
| `test/smoke`、`test/e2e` | 依赖契约冒烟、端到端验收 |

## 测试

```bash
make test       # GOWORK=off go test ./... -count=1
make smoke      # gonacos/gff 依赖契约
```

覆盖：黄金向量与独立实现交叉验证、求值确定性/分布/互斥层/epoch 隔离、发布-回滚-删除-乐观锁-二人审批、离线/乱序/损坏快照、事件去重/迟到/epoch 混入拒绝、统计 I 型错误与 CI 覆盖率模拟、SRM 阻断错误结论、AI 跨租户 404/摘要漂移拒发/撤销阻断、受众凭证分离。

## 关键前置条件（生产多租户前）

1. 固定 gonacos commit 与 gff core 版本并重跑契约测试（当前为 sibling replace）。
2. 验证 gonacos 端口隔离、集群推送与断连恢复；确认 gonacos 正式许可（仓库尚无 LICENSE）。
3. 平台存储切换 PostgreSQL 后端并完成备份/恢复联合演练（当前为文件 JSON 存储）。
4. 以压测确立 P99/收敛 SLO（当前仅有开发机 benchmark 证据，见实施状态）。
