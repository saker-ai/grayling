# golab

GoLab 是通用功能灰度与 A/B 实验平台，为传统后端、Web 应用与 AI Agent 提供变体决策、可信实验分析，以及 AI 资源（prompt/模型/skill/AgentSpec）的版本绑定、会话级配置冻结与紧急撤销。

**状态：首版已实现并可运行。** 单节点闭环（管理 API → gonacos 配置分发 → Go SDK 本地求值 → 事件采集 → 统计报告 → AI 资源灰度）全部落地并有端到端测试；生产化扩展已落地：PostgreSQL 存储后端（真实 PG 测试）、独立 proxy 部署形态（`cmd/golab-proxy`）、OpenFeature Go Provider（`pkg/ofprovider`）、gonacos 依赖固定为确定 commit 伪版本；阶段二已落地：**Python / TypeScript SDK**（远程求值 + 缓存/轮询 + collect 契约上报，集成测试连真实 golab 二进制）、**自动推进发布**（预注册计划步骤/观察窗/决策门驱动的 eligibility 分步放量，幂等、可暂停、人工接管开关）、**P0 收尾**（`openapi.yaml` 与路由表机械化核对、`docs/threat-model.md`、`test/perf` 正式压测——P99/可见延迟/SSE 并发达成 §13 待测目标）。高级统计（P5）尚未实现。逐项完成范围见 [实施状态](docs/实施状态.md)，端点清单见 [API 参考](docs/api.md) 与 [openapi.yaml](openapi.yaml)，设计依据见 [技术方案](docs/技术方案.md)，安全分析见 [威胁模型](docs/threat-model.md)。

## 快速开始

```bash
# 前置：Go 1.27+（gonacos 依赖已 pin 到确定 commit，从模块缓存构建，无需 sibling 仓库）
make build          # 或 GOWORK=off go build ./cmd/golab
# 契约根目录直接取自被 pin 的 gonacos 模块：
./bin/golab -addr 127.0.0.1:8688 \
  -gonacos-root "$(GOWORK=off go list -m -f '{{.Dir}}' github.com/godeps/gonacos)"
```

启动日志会打印一次性的 **global owner token**（`glab_admin_...`）。浏览器打开 `http://127.0.0.1:8688/` 进入控制台，填入该 token 即可创建工作空间/环境、编辑与发布 flag、注册实验、查看报告与审计。

生产存储使用 PostgreSQL（自动执行 schema 迁移；事件按月分区、保留期按分区删除、HMAC 密钥轮换有迁移路径）：

```bash
./bin/golab -pg "postgres://golab:secret@127.0.0.1:5432/golab?sslmode=require"
```

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

## SDK 接入（Python / TypeScript）

多语言 SDK 首版为**远程求值 + 受限投影**（技术方案 §8）：不下发全量快照——求值走 `POST /api/v1/sdk/eval`（可指向 `cmd/golab-proxy`），新鲜度走 `GET /api/v1/sdk/revision`（仅 revision 数字），内部规则/segment/AI 内容永不出服务端，适合脚本、Web 后端薄客户端与边缘运行时。曝光由服务端在远程求值时自动落库（幂等 event ID）；转化经 collect 契约批量上报（独立凭证）。任何失败（传输/鉴权/类型不匹配/未知 flag）一律返回**调用方默认值** + failed Result，绝不猜测。两语言契约一致，且各自带 `golab-fnv1a-10000-v1` 分桶独立实现（逐条通过黄金向量）。

```python
# sdk/python — 纯标准库，Python ≥ 3.9
from golab_sdk import GolabClient, Options

client = GolabClient(Options(
    base_url="http://127.0.0.1:8688",
    sdk_token="glab_sdk_...", collect_token="glab_collect_...",
))
value, details = client.bool_variation(
    "checkout-router", False, attributes={"user_id": uid})
if details.result.experiment is not None:      # 进入实验（曝光已由服务端记录）
    client.track_conversion(details, "conversion", 1.0)
client.close()
```

```ts
// sdk/typescript — 零运行时依赖，Node ≥ 22.6（原生运行 .ts）
import { GolabClient } from "./src/client.ts";

const client = new GolabClient({
  baseUrl: "http://127.0.0.1:8688",
  sdkToken: "glab_sdk_...", collectToken: "glab_collect_...",
});
const { value, details } = await client.boolVariation(
  "checkout-router", false, { attributes: { user_id: uid } });
if (details.result.experiment) client.trackConversion(details, "conversion", 1);
await client.close();
```

缓存语义：结果按 (flagKey, 规范化 context) 缓存并绑定 manifest revision；后台轮询（默认 30s）与每次 eval 响应都会更新 revision 视图，revision 前进即整表失效——活跃客户端即使轮询失败也能收敛。集成测试（`sdk/python/tests` pytest、`sdk/typescript/test` node:test）连**真实 golab 二进制**运行，详见各 SDK README。

## 自动推进发布（staged rollout）

实验可预注册**发布计划**（`POST /api/v1/admin/experiments/{id}/release-plan`）：步骤列表，每步 `{name, ratio, minRuntime, observationWindow, autoAdvance}`；ratio 是 eligibility 百分比且必须严格递增（eligibility 是独立 hash 坐标的前缀区间——升比例不移动已入组单元；改比例/单元/盐属 epoch 变更，引擎绝不触碰）。服务端引擎（`-rollout-interval`，默认 30s，0 关闭）按 tick 推进：

- 步骤服满 `minRuntime + observationWindow` 才查询固定样本决策门（绝不提前偷看 p 值）；
- 数据健康 + verdict continue/ship → 进入下一步（重发布绑定 flag 的 eligibility）；最后一步 ship → 计划 completed（**flag 停留最后一步 ratio，全量放开是人工发布决策，引擎不自动宣称成功**）；
- verdict rollback（护栏破线/主指标显著变差）→ 实验 Stopped（停止入组）+ 计划 rolled_back；
- 报告不可得 / SRM / 无曝光等数据不健康 → **自动暂停**并记原因，人工 `resume` 前不再动作（恢复后步骤时钟重置，重新服满观察窗）；
- 全部动作**幂等**（先记状态后发布，失败下一 tick 收敛修复）、落审计与计划内历史（actor 区分 `rollout-engine` 与人工）；
- 人工面：`pause`（必须给原因）/ `resume` / `advance`（手动推进）/ `takeover`（人工接管开关——开启后引擎对该计划完全停手）。

详见 [API 参考](docs/api.md#自动推进发布release-plan53-p4) 与 `internal/rollout/engine.go` 头注释；行为测试见 `internal/rollout/engine_test.go`（fake clock：时钟推进、SRM 阻断、护栏回滚、接管/暂停/手动推进、幂等）。

## OpenFeature 接入（Go）

`pkg/ofprovider` 实现官方 OpenFeature Go SDK（v1.19）的 Provider 契约，在 golab 本地快照上求值：

```go
import (
    "github.com/godeps/golab/pkg/ofprovider"
    "github.com/godeps/golab/pkg/sdk"
    "github.com/open-feature/go-sdk/openfeature"
)

provider := ofprovider.NewProvider(ofprovider.Options{
    ClientOptions: sdk.Options{BaseURL: baseURL, Token: sdkToken, CollectToken: collectToken},
})
_ = openfeature.SetProviderAndWait(ctx, provider) // 阻塞至 READY；ctx 控制等待上限
client := openfeature.NewClient("my-app")

details, _ := client.BooleanValueDetails(ctx, "checkout-router", false,
    openfeature.NewEvaluationContext("user-42", map[string]any{"tenant": "acme"}))
// details.Reason ∈ DEFAULT/TARGETING_MATCH/SPLIT/DISABLED/ERROR；
// details.Variant=变体名；FlagMetadata["golabReason"] 保留 golab 原生 reason
```

targetingKey 自动镜像为随机化单元（默认写入 `user_id` 字段，可用 `Options.UnitField` 改）；缺单元返回调用方默认值 + `TARGETING_KEY_MISSING`（绝不随机分组）；曝光仅在**真实进入实验**时由底层 SDK 批量上报；provider 状态随快照健康迁移（NOT_READY→READY→STALE）。

## 独立 proxy 部署

`cmd/golab-proxy` 把远程评估数据面（`/sdk/eval`、SSE `/sdk/stream`、`/collect`、`/sdk/config`、`/sdk/revision`）打包为**独立部署的无状态中继**，适合浏览器/瘦服务与 forwardAuth 类网关场景：

```bash
GOWORK=off go build -o bin/golab-proxy ./cmd/golab-proxy
./bin/golab-proxy -addr 127.0.0.1:8689 -upstream http://127.0.0.1:8688
```

凭证原样透传（鉴权/受众分离/限流仍由上游执行）；`GET /healthz` liveness 不依赖上游，`GET /api/v1/health` readiness 探测上游；一元请求超时（默认 10s，超时 504/不可达 502），SSE 逐块 flush、客户端断开即取消上游订阅；连接池（`-max-idle-conns-per-host` 等）可调；管理 API 不转发。详见 [API 参考](docs/api.md#独立-proxycmdgolab-proxy)。

## 架构

```
管理端/控制台 ──Admin API──> GoLab 控制面 ──不可变 revision+manifest──> gonacos Config
                                  │                                      │
                                  ├─ RBAC/二人审批/审计/限额              └─ SSE/ETag/轮询
                                  └─ 平台存储(实验/事件/审计/AI 索引)            │
                                        (文件 JSON 或 PostgreSQL)              │
应用后端 <──Go SDK 本地求值<──已验证快照原子替换<────────────────────────────┘
浏览器/瘦服务 ──远程求值/SSE/collect──> golab-proxy(无状态中继，可独立部署) ──> GoLab API
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
| `sdk/python`、`sdk/typescript` | Python / TypeScript SDK（远程求值+缓存/轮询+collect 契约上报+分桶黄金向量；集成测试连真实二进制） |
| `pkg/ofprovider` | OpenFeature Go Provider（官方 go-sdk 契约：五类型求值/状态事件/reason 与错误码映射） |
| `internal/platform` | 控制面存储（工作空间/凭证/草稿/revision/实验+发布计划/事件/审计/AI 索引）；持久化后端：文件 JSON（开发默认）与 **PostgreSQL**（`pgstore.go`：schema 迁移/聚合表/事件分区/密钥轮换迁移） |
| `internal/configstore` | gonacos Config 适配（不可变 revision、manifest、读回验证） |
| `internal/admin` | RBAC + 发布流水线 + 实验生命周期（含 eligibility ramp 幂等应用） |
| `internal/rollout` | 自动推进发布引擎（计划步骤/观察窗/决策门 → advance/pause/rollback/complete；人工接管开关） |
| `internal/api` | HTTP 面：admin / sdk config+eval+revision+stream / collect / ai / release-plan |
| `internal/proxy` | 独立 proxy 中继（eval/SSE/collect/config/revision + liveness/readiness + 超时/连接池） |
| `internal/experiment` | 报告构建（单元聚合→统计→决策门） |
| `internal/stats` | Welch/两比例 z/SRM χ²/固定样本决策（纯标准库，含模拟验证） |
| `internal/ai` | AI 资源门禁（所有权索引/版本 pin/摘要复核/撤销） |
| `internal/gffimport` | gff 严格导入 + legacy-gff-v1 对照求值 |
| `cmd/golab`、`cmd/golab-proxy`、`web/` | 服务入口、独立 proxy 入口与内嵌控制台（无构建步骤） |
| `openapi.yaml` | 机器可读 API 规范（`internal/api/openapi_test.go` 与路由表 AST 双向核对） |
| `test/smoke`、`test/e2e`、`test/boot`、`test/pgtest`、`test/perf` | 依赖契约冒烟、端到端验收、二进制启动、真实 PostgreSQL、正式压测（P99/可见延迟/SSE 并发）基座 |

## 测试

```bash
make test       # GOWORK=off go test ./... -count=1（含 test/perf 压测与 openapi 核对）
make smoke      # gonacos/gff 依赖契约（gonacos 契约根取自被 pin 的模块缓存）

# Python SDK（连真实 golab 二进制）
cd sdk/python && python3 -m venv .venv && .venv/bin/pip install pytest && .venv/bin/pytest tests/ -q

# TypeScript SDK（连真实 golab 二进制；node ≥ 22.6 原生跑 .ts）
cd sdk/typescript && npm install && npm test && npm run typecheck
```

PostgreSQL 后端测试连**真实数据库**：默认用 docker 自动起一次性 `postgres:16-alpine` 容器；也可 `GOLAB_TEST_PG_DSN=<admin dsn>` 指定现成服务器（每测试建独立库）；两者都没有时相关测试显式 skip 并说明原因。

覆盖：黄金向量与独立实现交叉验证（Go/Python/TS 三实现同向量）、求值确定性/分布/互斥层/epoch 隔离、发布-回滚-删除-乐观锁-二人审批、离线/乱序/损坏快照、事件去重/迟到/epoch 混入拒绝、统计 I 型错误与 CI 覆盖率模拟、SRM 阻断错误结论、AI 跨租户 404/摘要漂移拒发/撤销阻断、受众凭证分离、PG roundtrip/分区删除/密钥轮换迁移/去重账本/并发（-race）、proxy 中继与健康检查（单元+e2e+双进程 boot）、OpenFeature 契约（值/变体/reason/错误码/状态/曝光归因）、本地/远程求值同值（`TestE2ELocalRemoteParity`）、自动推进（时钟推进/SRM 暂停/护栏回滚/接管/幂等，fake clock）、OpenAPI 与路由表双向核对、多语言 SDK 集成（pytest 21 例 / node:test 18 例）、正式压测（P99/发布可见延迟/1000 并发 SSE，`-short` 跳过）。

## 关键前置条件（生产多租户前）

1. ~~固定 gonacos commit~~ **已完成**（`v0.0.0-20260706000526-514208bd40a1`，smoke/boot 契约测试重跑全绿）；后续升级需重跑契约测试。gonacos 正式许可仍需确认（仓库尚无 LICENSE）。
2. 验证 gonacos 端口隔离、集群推送与断连恢复；GoLab 多副本对账演练。
3. ~~平台存储切换 PostgreSQL 后端~~ **已完成**（真实 PG 测试）；备份/恢复联合演练（gonacos Snapshot + PostgreSQL + 密钥同一恢复点）仍待做。
4. ~~以压测确立 P99/收敛 SLO~~ **本机回环基线已完成**（`test/perf`：本地求值 P99=3.68µs、发布→SDK 可见 max=3.77ms、1000 并发 SSE fan-out p99<6ms，硬件/样本量见[实施状态](docs/实施状态.md)性能证据节）；生产配额与限流参数仍须以生产同构环境压测复核后设定。
