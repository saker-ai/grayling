# GoLab API 参考（v1）

> 与实现同步的端点清单（来源：`internal/api/server.go` 路由表）。机器可读规范见仓库根 [`openapi.yaml`](../openapi.yaml)（由 `internal/api/openapi_test.go` 与路由表机械化双向核对）；本文以实际行为为准。

## 认证与受众

所有请求携带 `Authorization: Bearer <token>`。凭证决定作用域（workspace/environment），**任何端点不接受用 query/body 参数选择租户**。三受众互斥：

| 凭证 | 前缀 | 可用端点 |
|---|---|---|
| admin | `glab_admin_` | `/api/v1/admin/**` |
| sdk | `glab_sdk_` | `/api/v1/sdk/**` |
| collect | `glab_collect_` | `/api/v1/collect` |

错误响应统一为 `{"error":{"code":"...","message":"..."}}`。常见 code：`unauthorized`(401)、`wrong_audience`/`access_denied`/`self_approval_denied`(403)、`not_found`(404)、`validation_failed`/`import_failed`/`batch_too_large`(400)、`conflict`/`invalid_state`(409)、`rate_limited`/`quota_exceeded`(429)、revoked 资源(423)、`ai_unavailable`(501)。

管理角色：`owner`（全部+token/配额/AI 原始内容）、`publisher`（flag 全流程+发布+实验操作）、`analyst`（读+实验定义/评审）、`viewer`（只读）。所有草稿与高影响操作均禁止提交人自批（403 `self_approval_denied`）；审批有效期 24 小时，凭证被吊销或降权、内容或基线 revision 改变后须重新申请。详见 [审批与紧急操作](approval-policy.md)。

## Admin

`{ws}`/`{env}` 接受 slug 或内部 ID。

| 方法与路径 | 用途 |
|---|---|
| `GET /api/v1/health` | 健康检查（无需认证） |
| `POST/GET /api/v1/admin/workspaces` | 创建（仅全局 owner）/列出工作空间 |
| `POST/GET /api/v1/admin/workspaces/{ws}/environments` | 创建/列出环境 |
| `POST/GET /api/v1/admin/workspaces/{ws}/tokens` | 铸造（secret 仅返回一次）/列出凭证 |
| `DELETE /api/v1/admin/tokens/{id}` | 吊销（作用域 owner 只能吊销本工作空间；全局凭证仅全局 owner） |
| `GET /api/v1/admin/workspaces/{ws}/audit?limit=` | 审计（新→旧） |
| `PUT /api/v1/admin/workspaces/{ws}/quotas` | 配额（owner） |

### Flag 生命周期

| 端点（前缀 `/api/v1/admin/workspaces/{ws}/environments/{env}`） | 说明 |
|---|---|
| `GET /flags` / `GET /flags/{key}` | 列表 / 详情（含草稿、当前 revision、历史） |
| `PUT /flags/{key}/draft` | 保存草稿 `{content:<Flag DSL>, risk:"normal"\|"high", baseRevision}`；`content.key` 必须等于 `{key}`；`baseRevision>0` 时启用乐观锁（409 冲突） |
| `POST /flags/{key}/submit` / `approve` / `reject` | 评审流（submit 必填 `{reason}`；approve 可带 `{note}`） |
| `POST /flags/{key}/publish` | `{note}` 必填；发布已批准草稿 → `{flagRevision, manifestRevision, hash, dataId}` |
| `POST /flags/{key}/rollback` | `{revision, reason, approvalId}` 计划回滚；owner 可用 `{revision, reason, emergency:true}` 紧急回滚，生成事后复核记录 |
| `POST /flags/{key}/emergency-disable` | owner `{reason}` 立即发布 disable=true 新 revision，生成独立事后复核记录 |
| `DELETE /flags/{key}` | 标记删除（仍需 submit→approve→publish 落 tombstone） |
| `POST /flags/{key}/import-gff` | `{document:"<gff YAML/JSON>", key, salt}` → 严格白名单导入；不支持字段报错、绝不静默丢弃；返回 warnings（含"两种算法分配不一致"） |
| `GET /segments`、`PUT /segments/{key}/draft`、`GET /segments/{key}/preview`、`POST submit/approve/publish` | segment 同流程，submit 必填 `{reason}`，publish 必填 `{note}` |
| `GET /flags/{key}/preview` | 发布前差异、环境、上限受影响用户、回滚目标、护栏及提交 hash |
| `POST /debug/eval` | `{flagKey, context:{targetingKey, attributes}}` → 当前快照求值；**永不记录曝光** |

### 高影响操作审批

| 端点 | 说明 |
|---|---|
| `POST {env前缀}/approvals` | `{action, object, payload, reason}` 申请；支持 `flag.rollback`、`release_plan.create`、`ai.pin`，返回不可变 payload hash、基线 revision、影响与回滚信息 |
| `GET /api/v1/admin/workspaces/{ws}/approvals` | 本工作空间申请与事后复核列表 |
| `POST /api/v1/admin/workspaces/{ws}/approvals/{id}/approve` | 不同的 owner/publisher `{note}` 批准；`breakglass.review` 用同一端点事后复核 |

申请 `release_plan.create` 的 `object` 为实验 ID、`payload` 为 `{steps:[...]}`；申请 `ai.pin` 的 `object` 为 `{type}/{logical}`、`payload` 为 `{content}`；申请 `flag.rollback` 的 `object` 为 flag key、`payload` 为 `{revision}`。批准后在原执行端点传 `approvalId` 和同一 payload。AI 原始内容只用来计算 hash，不存入审批记录。

### 实验

| 端点 | 说明 |
|---|---|
| `POST {env前缀}/experiments` | 预注册（flagKey/unit/variants/proportions/primaryMetric/guardrails/alpha/power/mde/minSamplePerArm/minRuntime(ns)/analysisWindow(ns)/hypothesis/layer?/eligibility?/holmFamily?）；epoch 自动分配、永不复用；layer 槽位注册即分配。`primaryMetric.type ∈ proportion\|mean\|ratio\|quantile\|funnel\|retention`；ratio 需 `denominatorKey`（`numeratorKey` 缺省用 `key`），quantile 需 `quantileLevel ∈(0,1)`，mean 可带 `covariateKey` 启用 CUPED；funnel/retention 的预注册结构见 `docs/漏斗与留存.md`（见下"高级统计"） |
| `GET {env前缀}/experiments`、`GET /api/v1/admin/experiments/{id}` | 列表/详情 |
| `POST /experiments/{id}/review` | Draft→Reviewed |
| `POST /experiments/{id}/bind` | 校验 flag 结构与比例一致后注入绑定，生成高风险草稿并转 Scheduled（需二人审批后发布） |
| `POST /experiments/{id}/transition` | `{to, reason}`；状态变更自动重发布绑定 flag（重试收敛） |
| `GET /experiments/{id}/report` | 分析报告：健康位/SRM/per-variant/主指标（welch\|two-proportion-z\|ratio-delta\|quantile\|cuped）/护栏/决策门（ship/rollback/continue/hold）；显式注册多重比较时另有 `holm`，并保留分离呈现的序贯（`sequential`）与贝叶斯（`bayesian`）视图 |

### Holm 假设家族（固定样本）

旧的单主指标双臂实验不传 `holmFamily`，原始 p 值、CI 和决策保持原样。需要多个确认性主指标或多个变体比较时，在**创建实验前**把所有将用于发布决策的比较放入同一个 `holmFamily`。目前一个 epoch 只支持一个确认性家族、每家族 2–100 项，成员指标限 `mean`（Welch）或 `proportion`（双比例 z）；CUPED、ratio、quantile、funnel、retention 可继续用于单主指标实验，但尚不进入 Holm 家族。第一项必须与 `primaryMetric` 完全一致，且比较 `variants[0]` 与 `variants[1]`；每个额外变体至少有一项注册比较。每项需唯一 `id`、完整 `metric`、`control`、`treatment`。同一指标跨多个变体的比较是多个家族成员，重复的指标/有序臂对拒绝注册。与共享 control 的比较可以相关；Holm 仍控制家族错误率。

```json
{
  "primaryMetric": {"key":"conversion", "type":"proportion", "direction":"increase"},
  "variants": ["control", "treatment", "third"],
  "holmFamily": {"id":"launch", "hypotheses":[
    {"id":"conversion-B", "metric":{"key":"conversion", "type":"proportion", "direction":"increase"}, "control":"control", "treatment":"treatment"},
    {"id":"engagement-B", "metric":{"key":"engagement", "type":"proportion", "direction":"increase"}, "control":"control", "treatment":"treatment"},
    {"id":"conversion-C", "metric":{"key":"conversion", "type":"proportion", "direction":"increase"}, "control":"control", "treatment":"third"}
  ]}
}
```

示例仅示意相关字段；完整创建请求还需 flag、比例、分析窗口等预注册字段。护栏是独立运营阈值，针对每个非基线变体检查，**不进入 Holm**；未注册的事后比较只能标为探索性，不产生确认性结论。Bandit 不使用固定样本 Holm。`primary`、`sequential`、`bayesian` 均描述首组变体比较；后两者分别是任意时刻有效推断与后验视图，不并入 Holm 或固定样本决策门。

创建返回的 `registrationVersion` 是含 epoch、变体、主指标、护栏、家族、alpha 与假设的 SHA-256 指纹；创建审计含版本及家族，报告重复输出版本。API 无修改假设或家族成员的端点；需变更须注册新 epoch。报告发现注册内容与版本不一致时拒绝生成，防止看数后静默改族。`holm.comparisons` 按注册顺序展示 `rawP`、`adjustedP`、`effectiveAlpha`、`rank`（按原始 p 排序，1 起）、`rejected`、差值与**未调整**的单项 CI。Holm 按排序后 `alpha/(m-rank+1)` 逐步拒绝，调整 p 值取前缀最大值并截断到 1；等 p 值按注册顺序稳定排序。缺失任何成员的有效检验时整个家族不给确认性判定，报告 hold。所有成员调整后显著且方向有利才 ship；任一调整后显著且方向不利则 rollback；SRM、护栏、最短运行时间和最小样本门仍优先执行。固定样本 p 值不可用于反复偷看后的提前成功宣告。

### 高级统计与报告字段（P5，§10.3/§10.4）

主指标类型扩展（均可预注册选用，SRM/护栏阻断对其同样生效——决策门顺序不变：数据健康/SRM/最短运行 → 护栏 → 最小样本 → 显著性）：

| `primary.type` | 方法 | `primary` 附带明细字段 |
|---|---|---|
| `welch` | 均值 Welch t 检验 | `welch` |
| `two-proportion-z` | 两比例 z 检验 | `proportion` |
| `ratio-delta` | ratio 指标 delta-method 方差 + 正态检验（R=Σ分子/Σ分母，逐单元配对） | `ratio`（ratioA/B、varA/B、se、z、ci） |
| `quantile` | 分位数：每臂分布式无关二项/阶次统计 CI + 两样本渐近检验（KDE 密度） | `quantile`（level、quantileA/B、每臂 ci、se、z、ci diff） |
| `cuped` | mean + 实验前协变量的 CUPED 方差缩减（效应无偏，方差↓≈ρ²） | `cuped`（theta、correlation、rawDiff、`varianceReduction`） |

报告另含两个**与频率派决策分离**的视图（绝不并入 ship/rollback 门，§10.3：不能将"默认 Bayesian"与"固定频率派 95% CI"混成同一决策规则）：

- `sequential`（仅 mean 指标）：always-valid 序贯推断（Gaussian mixture mSPRT）。字段 `pValue`（anytime-valid，多次查看不膨胀 I 型错误）、`csLow/csHigh`（时间一致置信序列）、`logLambda`、`tau`（混合尺度，默认取 MDE）。这是持续监控/自动推进每 tick 查看的正确口径；固定样本 `primary` 仍是预注册决策。
- `bayesian`：后验/期望损失/胜出概率。`bayesian.mean`（postMeanDiff/postSDDiff、credibleLow/High、`probBSuperior`、`expectedLossB/A`、priorSDUsed）或 `bayesian.proportion`（Beta 后验 postAA/AB/BA/BB、postMeanA/B、`probBSuperior`、`expectedLossB/A`、credibleLowDiff/High）。

**LLM-as-judge（§10.4）**：judge 经可插拔 provider（本地确定性 mock 用于离线测试；真实 provider 为 config-only 接入点，无 endpoint/key 时拒绝构建，绝不伪称已接真实模型）按预注册采样概率抽样、成对比较**位置随机化**、记录每条**入样概率**（设计加权）。离线校准对人工标注集给出 accuracy/Cohen's κ/位置偏置/长度偏置与 `calibrated` 位；judge 质量指标经 `QualityGuardrail` 接入决策门——**校准不达标时只作诊断（`breached=false`），绝不驱动放量/回滚**。judge/bandit 为库能力，经报告护栏与（bandit）分配策略接入，bandit 按 §10.3 不与固定比例 A/B 共用传统 p 值结论。

### 自动推进发布（release plan，§5.3 P4）

预注册的分步放量计划绑定在实验上，由服务端自动推进引擎（`internal/rollout`，`-rollout-interval`，默认 30s，0 关闭）按计划步骤/观察窗/决策门结果推进。步骤只 ramp **eligibility**（独立 hash 坐标的前缀区间，提升比例不会移动已入组单元）；比例/单元/盐属 epoch 预注册，引擎绝不触碰。

| 端点 | 说明 |
|---|---|
| `POST /api/v1/admin/experiments/{id}/release-plan` | 预注册计划 `{steps:[{name, ratio, minRuntime(ns), observationWindow(ns), autoAdvance}], approvalId}`；ratio 必须严格递增 ∈(0,100]，minRuntime>0；实验须处于 reviewed/scheduled/running；计划唯一（重复创建 409） |
| `GET /api/v1/admin/experiments/{id}/release-plan` | 计划详情 + 动作历史（每个自动/手动动作都记录 actor 与原因） |
| `POST .../release-plan/pause` | `{reason}` 必填；armed/running → paused，引擎停手 |
| `POST .../release-plan/resume` | paused → running（**步骤时钟重置**：干预后须重新服满观察窗）；从未起跑的计划回到 armed |
| `POST .../release-plan/advance` | `{reason}` 人工推进下一步（最后一步则完成计划）；不被决策门拦截（人工发布决策），但当前 verdict 记入历史 |
| `POST .../release-plan/takeover` | `{enabled, reason}` 人工接管开关：开启后引擎对该计划不做任何自动动作 |

引擎语义（全部动作幂等，先记状态后发布、失败下一 tick 收敛修复）：

- 实验 Running 后启动计划：应用 step 0 的 ratio（重发布绑定 flag）。
- 每步服满 `minRuntime + observationWindow` 才查询决策门（绝不提前偷看 p 值）：报告不可得或数据不健康（SRM/无曝光等）→ **自动暂停**并记原因，人工恢复前不再动作；verdict=rollback（护栏破线或主指标显著变差）→ 实验转 Stopped（重发布停止入组）+ 计划 rolled_back（终态）；verdict=hold（样本/最短运行不足）→ 继续等待；verdict=continue/ship → `autoAdvance` 步骤进入下一步；最后一步 ship → 计划 completed（**flag 停留在最后一步 ratio，全量放开是人工发布决策，引擎不自动宣称成功**）。
- 计划状态机：armed → running → paused/completed/rolled_back；所有自动动作以 `rollout-engine` 系统身份落审计（`release_plan.*`）。

### AI 资源

| 端点 | 说明 |
|---|---|
| `POST {env前缀}/ai/resources` | 注册 `{type:"prompt"\|"skill"\|"agentspec", logicalId, name}`；physical ID 服务端生成；mcp/pipeline 明确拒绝（无不可变版本语义） |
| `GET {env前缀}/ai/resources` | 索引+当前撤销列表 |
| `POST .../ai/resources/{type}/{logical}/pin` | `{content, approvalId}` → 发布新版本并 pin（version+MD5，发布后读回验证） |
| `POST .../ai/resources/{type}/{logical}/revoke` / `reinstate` | owner 带 `{reason}` 紧急撤销（实时生效，生成事后复核）；恢复须事后复核已完成并由 owner 带 `{reason}` 操作 |
| `GET {env前缀}/ai/resolve?type=&logicalId=&version=` | 原始内容读取——**仅 owner**（单独授权） |
| `PUT/GET /api/v1/admin/workspaces/{ws}/model-allowlist` | `{entries:[{provider,model,enabled}]}`；未入白名单的 modelRef flag 无法发布 |

## SDK 数据面（sdk 凭证）

| 端点 | 说明 |
|---|---|
| `GET /api/v1/sdk/config` | 作用域全量快照；`ETag`/`If-None-Match`→304；manifest revision 即 ETag |
| `GET /api/v1/sdk/revision` | **受限投影**：仅返回 `{revision, etag}`（无 flag 内容）；远程求值 SDK（Python/TS）以此轮询失效本地求值缓存；未发布环境返回 `revision:0` |
| `POST /api/v1/sdk/eval` | `{flagKey, context}` → `{result, value, manifestRevision, exposureEventId?, randomizationUnit?}`；进入实验时服务端自动落幂等曝光，并回显曝光归属的规范化随机化单元（供转化上报落在同一 unit hash） |
| `GET /api/v1/sdk/stream` | SSE：`event: revision`，`id:`=manifest revision，data=`{revision,scope}`；连接即重放当前 revision；30s 心跳；断线用 `Last-Event-ID` 重连（全量重同步语义） |
| `POST /api/v1/sdk/ai/resolve` | `{type, logicalId, version?}` → 摘要校验后的内容（跨租户 404，不泄露存在性） |
| `POST /api/v1/sdk/ai/session` | `{sessionId, context, flagKeys, resources?}` → 会话冻结快照：一次求值+自动发现 promptRef/modelRef+摘要校验；任一资源失败则整体失败 |
| `POST /api/v1/sdk/ai/check` | `{resources:[{type,logicalId}]}` → 执行前撤销门禁；有阻断返回 **423** `{anyBlocked:true, resources:[{blocked,reason}]}` |

## 采集（collect 凭证）

`POST /api/v1/collect` — `{events:[{eventId, type:"exposure"|"conversion", experimentId, epoch, flagKey?, flagRevision?, variant, unit, source?, metricKey?, value?, sessionId?, taskId?, clientTime}]}`。`sessionId` 在服务端哈希后保存；funnel/retention 使用预注册的 `metricKey` 事件（见 `docs/漏斗与留存.md`）。

- `unit` 必填明文，服务端做租户内 HMAC-SHA256 伪名化后存储（原始 unit 不落盘）。
- `eventId` 幂等去重；重复计入 `duplicates`。
- epoch 与实验注册不符、clientTime 超出 72h 迟到窗口或超前 5min → `rejected`（带原因）。
- 缺 unit 的事件跳过并计入 rejected（无随机化单元即无归因）。
- 响应：`{accepted, duplicates, rejected, rejectMessages}`。
- 批量与字节上限来自工作空间配额；超限 400 `batch_too_large`。

## 独立 proxy（cmd/golab-proxy）

把远程评估数据面打包为**独立部署的无状态中继**（技术方案 §3：proxy 可选，用于浏览器/瘦服务，不要求全部应用请求经过）。转发到上游 GoLab server（`-upstream`）：

| 端点 | 说明 |
|---|---|
| `POST /api/v1/sdk/eval` | 远程求值中继（服务端曝光在上游落库） |
| `GET /api/v1/sdk/stream` | SSE 中继：逐块 flush，事件边界不缓冲；客户端断开即取消上游订阅 |
| `POST /api/v1/collect` | 曝光/转化中继 |
| `GET /api/v1/sdk/config` | 快照拉取中继（ETag/304 透传） |
| `GET /api/v1/sdk/revision` | 受限新鲜度探测中继（仅 revision，供远程求值 SDK 轮询失效缓存） |
| `GET /api/v1/gateway/forwardauth` | **Traefik ≥3.6 multi-layer routing 求值服务**（proxy 自有端点，非中继）：见下节 |
| `GET /healthz` | **liveness**：proxy 自身存活，不探测上游（上游故障不应导致编排系统杀掉 proxy 机队） |
| `GET /api/v1/health` | **readiness**：探测上游 `/api/v1/health`；不可达/不健康 → 503 |

- 凭证原样透传：认证、受众分离、租户作用域与每 token 限流仍全部由上游执行；proxy 不持有中继凭证或状态，可水平扩展、随时重启。唯一例外是网关求值端点使用 `-gateway-token` 配置的自有 SDK 凭证（终端用户流量不可能携带 GoLab 凭证）。
- 管理 API 与 AI 端点**不转发**（404）：proxy 的暴露面就是远程评估。
- 错误映射：上游不可达 → 502 `upstream_unreachable`；超过 `-timeout`（一元请求，默认 10s）→ 504 `upstream_timeout`；其余状态码与响应体按上游原样透传。
- 连接池：`-max-idle-conns-per-host`（默认 100）、`-max-conns-per-host`（0=不限）、`-dial-timeout`（默认 5s）；`-health-timeout` 约束 readiness 探测（默认 3s）。
- client_ip 推导：`-trusted-proxy-cidrs`（逗号分隔 CIDR/裸 IP，如 `10.0.0.0/8,172.16.5.5,fd00::/8`；flag 为空时读环境变量 `GOLAB_TRUSTED_PROXY_CIDRS`，默认空）声明允许位于 proxy 之前的可信反向代理网段。两条**服务端求值路径**（forwardAuth 端点与 `POST /api/v1/sdk/eval` 中继）从连接对端（RemoteAddr）出发、仅在当前 IP 落于可信网段时逐跳向左回退 `X-Forwarded-For`，第一个不受信 IP 即 `client_ip`，其左侧条目一律丢弃（防伪造自选灰度组）；信任链中出现非法条目则 fail closed（不注入）。推导值以保留属性名 `client_ip` 注入求值上下文——中继路径上**调用方显式传入的 `attributes.client_ip` 优先，不被覆盖**；未配置时不信任任何 XFF，`client_ip` 即直连对端（现网行为不变）。安全语义与链路图见 [traefik-golab-architecture](traefik-golab-architecture.md)"按 client IP 灰度"节。
- SIGTERM/SIGINT 优雅关停。

```bash
GOWORK=off go build ./cmd/golab-proxy
./bin/golab-proxy -addr 127.0.0.1:8689 -upstream http://127.0.0.1:8688
```

## Traefik 网关集成（§4，P5）

Traefik ≥3.6 multi-layer routing 链路：父 router（无 service，独占 entryPoints）先剥离外部 `X-Golab-*` header，再经 forwardAuth 调用求值服务；子 router 按回写的 `X-Golab-Variant` 分流。完整链路与约束见 [traefik-golab-architecture](traefik-golab-architecture.md)；端到端验证证据见 [实施状态](实施状态.md)。

**求值服务 = golab-proxy 的 forwardAuth 端点**（`-gateway-token` 启用）：

```
GET /api/v1/gateway/forwardauth?flagKey=<key>&defaultVariant=<v>&unitField=<f>&unitHeader=<h>&secret=<s>
```

| 参数 | 默认 | 说明 |
|---|---|---|
| `flagKey` | 必填 | 求值的 GoLab flag |
| `defaultVariant` | 必填 | fail-open / 缺 bucketingKey 时回写的默认变体 |
| `unitField` | `targetingKey` | bucketingKey 写入的求值上下文字段（应与 flag 的 `bucketing.unit` 一致；非 targetingKey 时同时写入 attributes 与 targetingKey）。取保留值 `client_ip` 时为**按 IP 分桶**：分桶单元=proxy 从可信代理链派生的 client IP（`-trusted-proxy-cidrs`，见上节），`unitHeader` 不被读取——客户端无法用 header 自选灰度组；派生失败（如信任链断裂）按缺 bucketingKey fail-open |
| `unitHeader` | `X-Bucketing-Key` | 携带 bucketingKey 的请求 header |
| `secret` | — | 路由共享密钥（proxy `-gateway-secret` 配置后强制校验，常数时间比较）；不匹配的请求**绝不上游求值**——fail-open 回默认变体（`X-Golab-Fail-Open: unauthorized`），防止任何能触达 proxy 的调用方把它当作任意 flag 探测/曝光污染 oracle |

- Traefik forwardAuth 以 GET 探测该地址（address 的 query 原样保留），原始请求信息在 `X-Forwarded-*` header 中。
- 求值上下文恒携带（可推导时）保留属性 `client_ip` = 可信代理链派生的客户端 IP（`-trusted-proxy-cidrs`，见"独立 proxy"节；请求 header 输入永不喂给该值），flag 在 `fields` 注册 `client_ip` 即可用 `in_cidr` 按 IP 段定向；作为分桶单元允许但**实验慎用**（IP 漂移破坏粘性/归因，技术方案 §6）。端到端证据：`test/gateway/clientip_e2e_test.go`。
- 响应恒为 200（**fail-open 契约**）：`X-Golab-Variant`（求值变体或默认变体）、`X-Golab-Flag-Key`、`X-Golab-Reason`；内部故障（上游不可达/超时/非 200/求值失败/缺 bucketingKey/未配置 token）附加 `X-Golab-Fail-Open: <reason>` 并回写默认变体——网关绝不因 GoLab 故障产生 5xx。求值服务进程整体不可用需 HA 部署兜底（Traefik forwardAuth 对不可达的 auth 服务默认拒绝请求）。
- 曝光在决策点集中记录：求值走上游 `/api/v1/sdk/eval`，运行中实验的入组曝光由服务端落库（幂等 event ID）。
- 每次求值超时 `-gateway-eval-timeout`（默认 2s，热路径宜短），超时即 fail-open。

**router 配置生成器**：`internal/gateway` 由 `RouteSpec`（flag、变体→后端、entryPoint、求值服务地址、可选 `Secret`）生成三目标的动态配置——file（YAML/JSON）、KV（Consul/etcd/Redis/ZooKeeper 键值对）、Kubernetes IngressRoute CRD；所有输出先通过层级规则校验（根 router 独占 entryPoints、中间 router 无 service、叶子必须有 service、parentRefs 存在且无环——环检测覆盖多 parent 图）。比例调整只改 GoLab 分桶参数，**不触碰任何生成产物**；增删变体才需要重新生成。KV 目标的 header 剥离由 forwardAuth `authResponseHeadersRegex: ^X-Golab-` 承担（Traefik KV 解码器丢弃空值，headers middleware 置空语义在 KV 不可表达——file/IngressRoute 仍生成显式 headers middleware，两机制并存）。`RouteSpec.Secret` 会嵌入 forwardAuth address 的 `secret` 参数，与 proxy `-gateway-secret` 配对（强烈建议：proxy 网络可达即配置；仅回环/可信网格可省略，proxy 启动时对"有 token 无 secret"打 WARNING）。

```bash
# 网关求值服务（Traefik forwardAuth 目标）
./bin/golab-proxy -addr 127.0.0.1:8689 -upstream http://127.0.0.1:8688 \
  -gateway-token glab_sdk_... -gateway-secret <与 RouteSpec.Secret 相同> -gateway-eval-timeout 2s
```

## Flag DSL（schemaVersion 1）

见 `pkg/model/types.go` 与技术方案 §5.2。要点：`type ∈ bool|string|number|json`；规则必须有且仅有 `variation|percentage` 之一；约束字段必须在 `fields` 注册（`targetingKey` 隐式注册）；操作符闭集 `eq,ne,in,not_in,gt,gte,lt,lte,time_before,time_after,regex,in_cidr,semver_gt,semver_gte,semver_lt,semver_lte,semver_eq`；`bucketing.algorithm` 固定 `golab-fnv1a-10000-v1`；实验绑定 flag 的 `defaultRule.variation` 必填（未入组回退）。
