# GoLab API 参考（v1）

> 与实现同步的端点清单（来源：`internal/api/server.go` 路由表）。正式 OpenAPI 草案是 P0 待办（见[实施状态](实施状态.md)）；本文以实际行为为准。

## 认证与受众

所有请求携带 `Authorization: Bearer <token>`。凭证决定作用域（workspace/environment），**任何端点不接受用 query/body 参数选择租户**。三受众互斥：

| 凭证 | 前缀 | 可用端点 |
|---|---|---|
| admin | `glab_admin_` | `/api/v1/admin/**` |
| sdk | `glab_sdk_` | `/api/v1/sdk/**` |
| collect | `glab_collect_` | `/api/v1/collect` |

错误响应统一为 `{"error":{"code":"...","message":"..."}}`。常见 code：`unauthorized`(401)、`wrong_audience`/`access_denied`/`self_approval_denied`(403)、`not_found`(404)、`validation_failed`/`import_failed`/`batch_too_large`(400)、`conflict`/`invalid_state`(409)、`rate_limited`/`quota_exceeded`(429)、revoked 资源(423)、`ai_unavailable`(501)。

管理角色：`owner`（全部+token/配额/AI 原始内容）、`publisher`（flag 全流程+发布+实验操作）、`analyst`（读+实验定义/评审）、`viewer`（只读）。高风险草稿禁止提交人自批（403 `self_approval_denied`）。

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
| `POST /flags/{key}/submit` / `approve` / `reject` | 评审流（approve 可带 `{note}`） |
| `POST /flags/{key}/publish` | 发布已批准草稿 → `{flagRevision, manifestRevision, hash, dataId}` |
| `POST /flags/{key}/rollback` | `{revision, reason}` → 新 manifest revision 指向已验证旧 revision |
| `DELETE /flags/{key}` | 标记删除（仍需 submit→approve→publish 落 tombstone） |
| `POST /flags/{key}/import-gff` | `{document:"<gff YAML/JSON>", key, salt}` → 严格白名单导入；不支持字段报错、绝不静默丢弃；返回 warnings（含"两种算法分配不一致"） |
| `GET /segments`、`PUT /segments/{key}/draft`、`POST submit/approve/publish` | segment 同流程 |
| `POST /debug/eval` | `{flagKey, context:{targetingKey, attributes}}` → 当前快照求值；**永不记录曝光** |

### 实验

| 端点 | 说明 |
|---|---|
| `POST {env前缀}/experiments` | 预注册（flagKey/unit/variants/proportions/primaryMetric/guardrails/alpha/power/mde/minSamplePerArm/minRuntime(ns)/analysisWindow(ns)/hypothesis/layer?/eligibility?）；epoch 自动分配、永不复用；layer 槽位注册即分配 |
| `GET {env前缀}/experiments`、`GET /api/v1/admin/experiments/{id}` | 列表/详情 |
| `POST /experiments/{id}/review` | Draft→Reviewed |
| `POST /experiments/{id}/bind` | 校验 flag 结构与比例一致后注入绑定，生成高风险草稿并转 Scheduled（需二人审批后发布） |
| `POST /experiments/{id}/transition` | `{to, reason}`；状态变更自动重发布绑定 flag（重试收敛） |
| `GET /experiments/{id}/report` | 分析报告：健康位/SRM/per-variant/主指标（Welch 或两比例 z）/护栏/决策门（ship/rollback/continue/hold） |

### AI 资源

| 端点 | 说明 |
|---|---|
| `POST {env前缀}/ai/resources` | 注册 `{type:"prompt"\|"skill"\|"agentspec", logicalId, name}`；physical ID 服务端生成；mcp/pipeline 明确拒绝（无不可变版本语义） |
| `GET {env前缀}/ai/resources` | 索引+当前撤销列表 |
| `POST .../ai/resources/{type}/{logical}/pin` | `{content}` → 发布新版本并 pin（version+MD5，发布后读回验证） |
| `POST .../ai/resources/{type}/{logical}/revoke` / `reinstate` | 紧急撤销（实时生效，不走快照）/恢复 |
| `GET {env前缀}/ai/resolve?type=&logicalId=&version=` | 原始内容读取——**仅 owner**（单独授权） |
| `PUT/GET /api/v1/admin/workspaces/{ws}/model-allowlist` | `{entries:[{provider,model,enabled}]}`；未入白名单的 modelRef flag 无法发布 |

## SDK 数据面（sdk 凭证）

| 端点 | 说明 |
|---|---|
| `GET /api/v1/sdk/config` | 作用域全量快照；`ETag`/`If-None-Match`→304；manifest revision 即 ETag |
| `POST /api/v1/sdk/eval` | `{flagKey, context}` → `{result, value, manifestRevision, exposureEventId?}`；进入实验时服务端自动落幂等曝光 |
| `GET /api/v1/sdk/stream` | SSE：`event: revision`，`id:`=manifest revision，data=`{revision,scope}`；连接即重放当前 revision；30s 心跳；断线用 `Last-Event-ID` 重连（全量重同步语义） |
| `POST /api/v1/sdk/ai/resolve` | `{type, logicalId, version?}` → 摘要校验后的内容（跨租户 404，不泄露存在性） |
| `POST /api/v1/sdk/ai/session` | `{sessionId, context, flagKeys, resources?}` → 会话冻结快照：一次求值+自动发现 promptRef/modelRef+摘要校验；任一资源失败则整体失败 |
| `POST /api/v1/sdk/ai/check` | `{resources:[{type,logicalId}]}` → 执行前撤销门禁；有阻断返回 **423** `{anyBlocked:true, resources:[{blocked,reason}]}` |

## 采集（collect 凭证）

`POST /api/v1/collect` — `{events:[{eventId, type:"exposure"|"conversion", experimentId, epoch, flagKey?, flagRevision?, variant, unit, source?, metricKey?, value?, sessionId?, taskId?, clientTime}]}`。

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
| `GET /healthz` | **liveness**：proxy 自身存活，不探测上游（上游故障不应导致编排系统杀掉 proxy 机队） |
| `GET /api/v1/health` | **readiness**：探测上游 `/api/v1/health`；不可达/不健康 → 503 |

- 凭证原样透传：认证、受众分离、租户作用域与每 token 限流仍全部由上游执行；proxy 不持有任何凭证或状态，可水平扩展、随时重启。
- 管理 API 与 AI 端点**不转发**（404）：proxy 的暴露面就是远程评估。
- 错误映射：上游不可达 → 502 `upstream_unreachable`；超过 `-timeout`（一元请求，默认 10s）→ 504 `upstream_timeout`；其余状态码与响应体按上游原样透传。
- 连接池：`-max-idle-conns-per-host`（默认 100）、`-max-conns-per-host`（0=不限）、`-dial-timeout`（默认 5s）；`-health-timeout` 约束 readiness 探测（默认 3s）。
- SIGTERM/SIGINT 优雅关停。

```bash
GOWORK=off go build ./cmd/golab-proxy
./bin/golab-proxy -addr 127.0.0.1:8689 -upstream http://127.0.0.1:8688
```

## Flag DSL（schemaVersion 1）

见 `pkg/model/types.go` 与技术方案 §5.2。要点：`type ∈ bool|string|number|json`；规则必须有且仅有 `variation|percentage` 之一；约束字段必须在 `fields` 注册（`targetingKey` 隐式注册）；操作符闭集 `eq,ne,in,not_in,gt,gte,lt,lte,time_before,time_after,regex,in_cidr,semver_gt,semver_gte,semver_lt,semver_lte,semver_eq`；`bucketing.algorithm` 固定 `golab-fnv1a-10000-v1`；实验绑定 flag 的 `defaultRule.variation` 必填（未入组回退）。
