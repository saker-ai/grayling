# GoLab 威胁模型

> 状态：首版（对应 [技术方案](技术方案.md) §13 P0 退出条件"威胁模型"）。取证范围为当前工作树代码（main 分支，含本阶段新增的 `internal/rollout` 自动推进引擎、`sdk/python`/`sdk/typescript` 多语言 SDK、`test/perf` 压测与 `openapi.yaml`；状态口径与 `docs/实施状态.md` 同步）。所有"已缓解"结论均指向仓库内可核对的文件、函数或测试名；未实现项按 [实施状态](实施状态.md) 的口径如实记录，不夸大、不缩小。

## 1. 概述

### 1.1 范围与假设

**在范围内**：单节点 GoLab server（`cmd/golab`：HTTP API + 内嵌控制台 + 同进程 gonacos 底座 + 平台存储）、独立无状态 proxy（`cmd/golab-proxy`）、以及二者与 PostgreSQL / 文件存储之间的信任关系。

**对手模型**：

1. 能访问 GoLab HTTP 端口的未认证网络攻击者；
2. 持有某一类合法凭证（admin/sdk/collect token）的租户或租户内人员，试图越权、越租户或污染数据；
3. 持有效凭证的自动化客户端（误用或被盗用），试图耗尽资源或伪造实验数据。

**不在范围内**（属部署基线，见 §2.10）：主机/操作系统加固、物理安全、TLS 终止、拥有 PostgreSQL 或文件系统 root 权限的内部人员直接篡改存储、Kubernetes/网络策略本身的正确性。运营方（operator）视为半可信：其操作经审计留痕，但存储层加密与 WORM 审计不在仓库实现内。

### 1.2 资产清单

| 资产 | 内容与位置 | 首要保护需求 |
|---|---|---|
| 租户配置 | flag/segment 草稿与不可变 revision、manifest（gonacos Config，dataId `golab.flag.<key>.r<rev>`；平台库存元数据） | 完整性（发布即不可变、hash 校验）、租户间机密性 |
| 实验数据 | 实验定义/epoch/预注册要素、原始事件、单元级聚合、SRM/报告（`internal/platform`，文件 JSON 或 PostgreSQL） | 完整性（统计结论可信）、按保留期删除 |
| 凭证 | 三受众 token（明文仅铸造时返回一次，库中只存 SHA-256）、平台 HMAC 密钥环（文件 0600 / PG `hmac_keys` 表）、gonacos `-auth-secret` | 机密性、可吊销、可轮换 |
| AI 内容 | prompt/skill/agentspec 的已发布版本 + MD5 摘要、所有权索引、modelRef 白名单、撤销记录（gonacos AI 底座 + 平台索引） | 机密性（原始内容仅 owner 可读）、完整性（版本 pin + 摘要复核）、可即时撤销 |
| 单元身份 | 原始随机化单元值（仅在请求内存中，不落盘）；HMAC 伪名哈希（事件/曝光集/聚合键） | 隐私（不可逆伪名化、租户隔离密钥） |
| 审计记录 | 顺序 ID 追加日志（管理操作、发布、token 铸造/吊销、AI 撤销、release plan 动作） | 不可变性、可追溯 |

### 1.3 信任边界

```text
─────────────────────────── 租户侧（非可信区） ───────────────────────────
  管理员/CI ──admin token──┐
  应用后端 ──sdk token─────┤        ┌──────────────────────────────┐
  采集器 ──collect token───┼─HTTPS*▶│ GoLab HTTP API（cmd/golab）    │
  浏览器/瘦客户端 ─────────┼───────▶│ 唯一鉴权/授权/限流/配额点       │
        （经 golab-proxy）  │        └──────┬─────────┬─────────┬───┘
                           │               │进程内调用 │         │
───────────────────────────┼───────────────┼─────────┼─────────┼──────
                           │               ▼         ▼         ▼
              gonacos（loopback       平台存储      AI 资源门禁
              127.0.0.1:18848/19848）  file JSON    （所有权索引、
              Config/AI 底座，         或 PostgreSQL 摘要复核、实时撤销）
              无租户鉴权 ──── ╳ 禁止租户直连
```

边界要点（逐项在后文取证）：

- **B1 租户 → GoLab API**：唯一认证入口。Bearer token 先鉴权，受众（admin/sdk/collect）互斥，workspace/environment 一律由凭证导出（`internal/api/server.go` 包注释与 `authenticate`）。*TLS 未在仓库内终止，见 §2.10。
- **B2 GoLab → gonacos**：同进程服务接口（`configstore.NewGonacosStore(bundle.Config)`），不走网络；gonacos 监听默认 loopback，非 loopback 需显式环境变量并告警（`cmd/golab/main.go`）。gonacos 自身无租户授权（技术方案 §2：常规配置读路径允许匿名），故 B2 之外禁止任何租户可达路径。
- **B3 GoLab → 存储**：文件后端（开发默认，密钥文件 0600）或 PostgreSQL（`-pg`，pgx/v5 连接池）。DSN 属运维机密。
- **B4 proxy → 上游 GoLab**：无状态中继，凭证透传，鉴权/限流仍在上游（`internal/proxy/proxy.go` 包注释）。proxy 只注册数据面路由，管理 API 不转发。

### 1.4 证据约定

`路径:符号` 指代码取证点；`Test…` 指仓库内可重复运行的测试（`make test`；PG 相关需 docker 或 `GOLAB_TEST_PG_DSN`，见 `test/pgtest`）。技术方案/实施状态章节号沿用原文。

## 2. 威胁面分析

### 2.1 凭证面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| token 明文泄露（库/接口/日志） | 明文只在铸造响应中出现一次（`internal/platform/store.go:CreateToken` 返回 `CreatedToken`；`internal/api/admin_handlers.go` "secret shown exactly once"）；库中只存 SHA-256 hex（`hashSecret`），`ListTokens` 复制并剥除 `SecretHash`；token 铸造审计只记 token ID 不记 secret | 明文一经泄露不可从库中反查，但**无 token 有效期/TTL 字段**，泄露后只能靠吊销。建议：部署侧定期轮换凭证，仓库后续加过期时间 |
| 暴力猜解 | secret 为 32 字节 `crypto/rand`（256-bit 熵），格式 `glab_<kind>_<43 字符 base64url>`，猜解不可行；admin 端点在鉴权**前**即计入限流（`internal/api/server.go:adminOnly` → `adminLimiterKey` 取 token 尾 16 字符，无 token 归入 `admin:anon`），超限 429 | 限流按 key 分桶，随机伪造 token 会不断产生新 key；窗口表以 4096 条上限+机会式清理防内存增长（`rateLimiter.allow`），但无按源 IP 的失败计数/封禁。建议：边界（LB/WAF）做 IP 级失败限速 |
| 受众混用（sdk token 调管理 API 等） | 三受众严格互斥：`adminOnly`/`sdkOnly`/`collectOnly` 各自只接受对应 `TokenKind`，不匹配返回 403 `wrong_audience`（`server.go:authenticate`）；即使 admin token 也不能调 collect（`TestE2EAudienceSeparation` 覆盖全部三种混用 + 匿名 401）；`admin.Authorize` 再次拒绝非 admin kind（`internal/admin/rbac.go`） | 无已知残余。注意 sdk/collect token 是**服务端凭证**，不得下发浏览器（实施状态 §8 行；浏览器走 `/sdk/eval`） |
| 吊销时效 | `RevokeToken` 置 `Revoked` 并立即提交（文件/PG 同一事务语义）；`ResolveToken` 每请求检查 `Revoked`，无凭证缓存，吊销后下一请求即 401（`store.go`）；作用域 owner 不能吊销全局 token（403，`TestE2ETokenRevokeScoping`） | SDK 已下载的快照在吊销后仍可用到本地过期/陈旧兜底生效——吊销切断的是 API 面，不是已分发数据。kill switch 最大陈旧窗口见技术方案 §5.2 |
| bootstrap owner token | 仅当库中无任何 token 时自动铸造全局 owner，明文**打印一次**到启动日志（`cmd/golab/main.go:bootstrapOwner` 及打印框），同时写审计 `bootstrap.owner` | 启动日志（stderr/容器日志）成为一次性机密载体。建议：首次启动后立即在控制台铸造命名凭证并吊销 bootstrap token；日志收集侧注意该窗口 |
| 日志脱敏 | 全仓库无 secret 打印路径（除上述一次性 bootstrap 展示）；审计与错误消息不携带凭证材料 | 未实现统一脱敏过滤器，防泄露靠"不记录"纪律；见 §2.3 日志注入残余 |

### 2.2 租户隔离面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| 跨工作空间读写（管理面） | 对象级 RBAC：token 的 `WorkspaceID` 与路径工作空间不符即 `ErrAccessDenied`（`internal/admin/rbac.go:Authorize`）；所有 admin handler 先 `resolveScope`/`Authorize` 再触库；`TestRBACCrossWorkspaceDenied`、`TestE2ETokenRevokeScoping` | 全局 owner token 通过一切检查（`IsGlobalOwner`）——它是平台运营凭证，泄露即全租户失守，应视同 root secret 管理 |
| query/body 参数选租户 | 数据面 ws/env **只**取自 token：`/sdk/config` 无任何作用域参数（`internal/api/sdk_handlers.go` 头注释），`/sdk/eval`、`/collect`、AI SDK 端点同样以 `tokenFrom(r)` 定界；`TestE2EAudienceSeparation` 显式断言 `?workspace=other&environment=x` 不改变返回快照的归属 | 无已知残余 |
| gonacos 匿名读路径 | gonacos 常规配置读不执行 `Auth.Authorize`（技术方案 §2 依赖事实表），因此底座监听默认 loopback（`-gonacos-addr 127.0.0.1:18848`、`-gonacos-grpc-addr 127.0.0.1:19848`）；非 loopback 绑定在无 `GOLAB_ALLOW_NON_LOOPBACK_GONACOS=1` 时 `Fatalf` 拒启，设置时打 WARNING（`cmd/golab/main.go:isLoopback` 分支）；GoLab 自身只经进程内 `Services()` 接口使用底座（`configstore.NewGonacosStore`），技术方案 §9 禁止把 gonacos 原始 HTTP/gRPC 暴露给租户 | 显式 opt-in 后的网络策略正确性属运维职责，且技术方案 §12.5 的"生产验证未授权直连被拒"**未完成**（实施状态 §12 行）。多租户生产前必办（§3.2） |
| AI 全局资源 ID 无 namespace | gonacos AI 资源以全局 ID 索引、无 namespace 字段（技术方案 §2），GoLab 自建所有权索引 `(ws, env, type, logical) → physical_id`，physical ID 由服务端生成、调用方不可指定（`internal/ai/service.go:RegisterResource`）；一切 AI 读写走 GoLab 授权入口；跨租户解析统一 404（`ErrUnauthorized` → `mapAIError` → `platform.ErrNotFound`，"never reveal whether it exists elsewhere"）；`TestAICrossTenantUnresolvable`、`TestResolveAuthorization` | 若绕过 GoLab 直连 gonacos AI API 则无隔离——回到上一行的 loopback/网络策略前提 |
| namespace ID 注入/碰撞 | namespace ID 完全由服务端代理 ID 拼成：`"golab-" + ws.ID + "-" + env.ID`（`store.go:CreateEnvironment`），用户提供的 slug 永不进入底座 ID（技术方案 §5.1）；slug 另有 `[a-z0-9-]`、≤64、首字符字母校验（`ValidSlug`）；`TestNamespaceMappingGenerated` | 无已知残余 |

角色-权限矩阵（`rbac.go:Authorize`，供隔离审计对照）：

| 权限 | owner | publisher | analyst | viewer |
|---|---|---|---|---|
| workspace.admin（建租户/环境） | 仅**全局** owner | — | — | — |
| token.manage / quota.manage / ai.content.read（原始 AI 内容） | ✓ | — | — | — |
| flag.write / flag.approve / flag.publish | ✓ | ✓ | — | — |
| experiment.write / ai.write | ✓ | ✓ | ✓ | — |
| flag.read / report.read | ✓ | ✓ | ✓ | ✓ |

补充：高风险变更禁止自批（`platform.ErrSelfApproval` → 403 `self_approval_denied`，`TestHighRiskSelfApprovalBlocked`）；非 owner 角色的 admin token 不能是全局的（`CreateToken` 校验"only owner tokens may be global"）。

### 2.3 注入面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| SQL 注入（PostgreSQL 后端） | **抽查结论：行级 DML 全部参数化，未发现用户输入拼接进 SQL。**抽查点：`commitMeta`/`hmac_keys` upsert（`$1,$2`）、`commitAggs`（`$1..$12`）、`commitCounts`（`$1..$7`）、`commitAudit`（`$1..$9`）、`rehashStored` 重映射（`$1,$2,$3` + 固定 UPDATE…FROM）、`pruneEvents` 边界删除（`$1`）、`KnownEventIDs`（`= ANY($1)`）、`loadFlags`/`loadEvents` 等为无参固定 SELECT；事件批量写入用 `CopyFrom` 临时表 + 固定 CTE（`commitEvents`），无字符串插值。全文件仅两处 `fmt.Sprintf` 进 SQL，均为 DDL 且输入非用户可控：① 建月分区 `events_YYYYMM`，名字与 FROM/TO 边界由服务端 UTC 时钟整数与 RFC3339 生成（`ensureEventsPartition`）；② `DROP TABLE IF EXISTS %s`，表名来自 `pg_class/pg_inherits` 目录查询（父表限定 `events`）并经 `partitionUpperBound` 格式校验（6 位数字、月份 1–12） | 无已知注入路径。分区 DDL 多副本安全性由真实 PostgreSQL 并发事件测试覆盖；管理写入的条件事务与唯一键见[多副本管理写入](多副本管理写入.md) |
| flag DSL / 正则 ReDoS | 正则模式 ≤512B（`MaxRegexLen`）且发布时必须编译通过（`pkg/model/validate.go:validateConstraint`）；求值侧对单次匹配输入截断 ≤4096B（`pkg/evaluation/constraints.go:MaxRegexInputBytes`，`TestRegexInputLengthBound`）；Go `regexp` 为 RE2 线性时间引擎，无灾难性回溯 | 512B 内仍可构造高成本 RE2 模式（如大重复乘积），复杂度线性于输入但常数可大；发布是受 RBAC 控制的管理操作，风险面限于租户自伤。可选：求值超时/成本采样 |
| 约束操作符注入 | 操作符闭集 17 个（`pkg/model/types.go:Operators`），未注册操作符直接拒绝；字段闭集（仅注册字段可被约束，"§5.2 closed field set"）；每类操作符的操作数类型/格式逐一校验（CIDR、semver、RFC3339/epoch、数值、标量）；数量上限：variations 64、targeting 64、constraints 32、segments/rule 16、集合值 512、segment 深度 ≤8 且循环引用拒绝（`validateSegmentTree`，`TestSegmentsNestedAndCycles`）；flag/segment key 受 `KeyPattern`（`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`）约束后才可嵌入 gonacos dataId | 无已知残余 |
| JSON 请求走私/字段污染 | 所有 API body 经 `decodeJSON`：`MaxBytesReader` 字节上限 + `DisallowUnknownFields` 严格解码（`server.go`），未知字段 400；admin body ≤2MiB（`admin_handlers.go:maxAdminBodyBytes`）；gff 导入同样严格解析、不支持字段显式报错（技术方案 §5.2，`internal/gffimport`） | 无已知残余 |
| 日志/审计注入 | 无缓解实现：审计 `Detail`、AI 撤销 `reason`、token 名等用户字符串原样入库并进 `log.Printf`（标准库不转义换行），存在日志伪造（log forging）可能 | **如实标注为未缓解**。建议：日志/审计渲染层转义控制字符，或控制台展示时按纯文本渲染（当前控制台为 SPA，HTML 注入面由前端转义承担，需前端审计确认） |
| 错误信息泄露 | `writeStoreError` 将哨兵错误映射为稳定错误码（404/409/403/429/400），跨租户 AI 一律 404 不泄露存在性 | 默认分支 500 会回传 `err.Error()` 原文（`server.go:writeStoreError` default），可能带内部细节。建议：500 只回关联 ID，详情进服务端日志 |

### 2.4 SSRF / 出站请求面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| 请求侧控制出站 URL | **GoLab 主服务无任何用户可控 URL 拉取路径**：全仓库（除 proxy）无 `http.Get`/外呼客户端；AI 内容取自同进程 gonacos 底座，modelRef 只做白名单查表（`ResolveModelRef`），不代租户调用模型供应商 | 无已知残余 |
| proxy 被用作跳板 | 上游地址仅来自启动参数 `-upstream`（`cmd/golab-proxy/main.go`），构造时校验 scheme ∈ {http, https} 且有 host（`proxy.New`）；转发时只拼接固定 upstream + 请求路径（`newUpstreamRequest`），Host 不可被请求改写；管理 API 路由未注册，不转发（`Handler` 仅 eval/config/collect/stream/健康检查；`TestUnknownRouteNotRelayed`）；hop-by-hop 头双向剥离（RFC 9110 §7.6.1） | transport 使用 `http.ProxyFromEnvironment`（`proxy.go:New`）——环境级 `HTTP(S)_PROXY` 可改变实际出口，属部署环境信任假设，建议在部署基线中固定/清空代理环境变量 |
| gonacos 出站 | 底座绑定默认 loopback（§2.2）；GoLab 对 gonacos 的调用是进程内接口而非网络出站 | 集群模式（外部 Redis）时 Redis 地址为启动参数，同属环境信任 |

### 2.5 统计滥用 / 数据完整性面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| p 值偷看 / 提前宣布胜利 | 固定样本决策门 `internal/stats/decision.go:Decide` 为纯函数、不读时钟，顺序固定：①有效性门（数据健康 + SRM + 预注册最短运行时间，任一不满足 → hold，**不出胜负**）→ ②护栏门（breach → rollback）→ ③样本量门（< MinSamplePerArm → hold）→ ④显著性门（p < alpha 且显著 → ship/rollback 按方向）→ ⑤否则 continue（"absence of significance is not evidence of no effect"）；alpha/power/MDE/最短运行时间预注册于实验记录；模拟验证 I 型错误 0.0475/0.0520、CI 覆盖 95.25%/94.8%（实施状态 §10.3，固定种子可复现） | 决策门不提供序贯/多次查看校正——多次查看报告仍是"看"，只是结论不被门放行；报告 API 无法阻止人引用中间 p 值，属流程纪律（技术方案 §10.3"多次查看固定样本 p 值不得据此随时宣称胜利"） |
| SRM（样本比例失配） | χ² 检验阈值 0.001（`stats.SRMTest`，`internal/experiment/report.go`）；SRM 输入**仅**为 `Source == "split"` 的随机化曝光单元集合（`store.go:SplitExposedUnits` 注释、`events.go:SplitExposedUnitCounts`），targeting/default 臂不进 SRM；SRM 告警 → `Healthy=false` → 决策门 hold（`TestReportSRMBlocksVerdict`）；自动推进引擎遇 SRM 暂停（`TestRolloutPausesOnSRM`）；P2 验收含"异常数据不出胜负结论" | SRM 只检测比例失配，不检测系统性伪造（见"曝光伪造"行） |
| epoch 混入 | 事件 `Epoch` 与实验当前 `Epoch` 不符即拒绝（`events.go:validateEventLocked`"mixed epochs are rejected"；`TestEventDedupAndUnitAggregation` 错 epoch 分支）；epoch 永不复用（`TestEpochsNeverReused`）；改 epoch 即新实验（`TestEpochChangeIsNewExperiment`）；报告按 `(exp, epoch)` 定界聚合 | 无已知残余 |
| 曝光伪造 | 远程求值路径由**服务端**落曝光：`/sdk/eval` 命中运行中实验时服务端自行构造曝光事件，事件 ID 确定性生成 `srv-<expID>-<epoch>-<unitHash[:16]>-<variant>`，重复求值幂等（`sdk_handlers.go:recordRemoteExposure` 及注释"remote clients cannot be trusted to report exposures"）；缺随机化单元的事件被拒（`handleCollect` skippedNoUnit；求值侧 `MISSING_BUCKET_KEY` 返回显式默认、绝不随机，`TestMissingBucketKey`）；调试求值不写曝光（`TestDebugEvaluateNoExposure`、`TestE2EDebugEvalNoExposure`） | `/collect` 仍接受 collect token 客户端自报曝光/转化——collect token 是受信服务端凭证，被盗后可在**本 scope 内**伪造任意 unit 的数据；SRM/数据健康门能拦截部分粗劣伪造，精细伪造不可检测。建议：collect token 最小化发放、纳入凭证轮换，异常摄入速率监控 |
| 重复事件 | 三层去重：批内 `seen` 集；内存 `EventIndex`（20 万条 FIFO 工作集）；PG 持久账本 `event_index` 覆盖**全保留期**（超出内存窗口的重复仍被拒，`TestPGDedupLedgerBeyondMemoryWindow`）；PG 侧单条 CTE 使 payload/曝光集只跟随账本判定，重放不可能双插（`pgstore.go:commitEvents` 注释"replays can never double-insert"） | 文件后端仅有内存窗口去重，重启+FIFO 逐出后旧 event_id 可再次接受——生产用 `-pg`（实施状态已知限制 1） |
| 迟到 / 未来数据 | `clientTime` 硬边界：早于 now−72h 或晚于 now+5min 拒绝（`events.go:maxLateWindow/futureSkew`；`TestEventDedupAndUnitAggregation` 30 天前事件分支） | 72h 是平台级硬边界；**按实验预注册 AnalysisWindow 过滤聚合未做**（实施状态已知限制 5），窗口外、72h 内的数据仍进增量聚合 |
| guardrail 与显著性混淆 | 护栏是运营保护规则，与主指标显著性分离评估（`decision.go` Rule 2 注释、`report.go`"guardrails (operational rules, separate from significance)"）；护栏 breach 独立触发 rollback，不因主指标显著为正而豁免（`TestReportGuardrailRollback`）；护栏也有双臂最小样本要求，数据不足报"insufficient data"而非通过 | 无已知残余 |
| 自动推进失控 | `internal/rollout/engine.go`：仅在报告可得、数据健康、护栏通过、最小样本/最短运行满足时推进；报告不可得/数据健康门失败/SRM → 自动暂停并记原因，**暂停后永不自动恢复**（engine.go 头注释与 `tickPlan`）；人工面提供 Pause（必须给原因）/Resume（步骤时钟重置）/AdvanceNow/SetManualOverride（接管后引擎停手），全部落审计 `release_plan.*`；`-rollout-interval 0` 整体关闭自动化；测试 `TestRolloutManualTakeoverPauseAdvance`、`TestRolloutAutoAdvanceFalseHoldsAtBoundary`、`TestRolloutWaitsForExperimentMinRuntimeAndSample`、`TestReleasePlanAPIValidation` | 技术方案 §9 要求的自动动作"签名或专用服务令牌、冷却时间"中，**冷却时间以观察窗/步骤时钟体现，未见独立的信号端点签名机制**（引擎走进程内调用，不暴露 `/signals` HTTP 面）。单节点进程内 ticker，多副本部署需要选主/分片。上线前建议补发布演练 |
| 未知实验/孤儿数据 | 未知 `experimentId` 的事件直接拒绝（`validateEventLocked`"no orphan buckets"），事件必须归属 token 自己的 ws/env（跨 scope 实验 ID 拒绝，`exp.WorkspaceID != ev.WorkspaceID` 分支） | 无已知残余 |

### 2.6 DoS / 资源耗尽面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| 请求洪水 | 每 token 固定窗口限流：默认 200 req/s、burst=200、窗口 1s（`server.go:NewServer` Options.RequestsPerSecond 默认 200，`newRateLimiter(rps, int(rps))`）；admin 在鉴权前限流（含 `admin:anon` 桶），sdk/collect 鉴权后按 token ID 限流；超限 429 `rate_limited`；限流器窗口表 4096 条上限+机会式清理，内存有界 | 固定窗口在边界处可放行至 2 倍 burst；限流按 token 不按 IP/连接；`http.Server` 只设 `ReadHeaderTimeout: 10s`，无全局并发/连接上限（SSE 长连接需要豁免 WriteTimeout，属设计取舍）。边界限速与连接上限留给部署层 |
| 采集批量/字节洪泛 | `/collect` 传输上限 = `MaxEventsPerBatch × MaxEventBytes`（默认 500 × 16KiB = 8MiB），硬顶 64MiB（`sdk_handlers.go:handleCollect`）；批量超配额 400 `batch_too_large`；admin/SDK body 一律 ≤2MiB（`maxAdminBodyBytes` + `decodeJSON:MaxBytesReader`） | `MaxEventBytes` 仅参与聚合体积上限计算，**无逐事件字节检查**——单事件可占满整个 body 上限。建议：逐事件 size 校验 |
| SSE 连接耗尽 | 每 (ws, env) scope 连接上限 256（`sse.go:NewSSEHub:maxPerScope`），超限 503 `sse_cap`；每连接缓冲 16，慢消费者**丢弃通知**而非阻塞发布路径（`Notify` select/default），SDK 轮询兜底保证收敛；30s 心跳回收死连接 | 上限按 scope 不按 token，单 token 可占满本 scope 256 条（自伤半径限于本租户环境）。无已知跨租户放大 |
| 事件内存耗尽 | 内存原始事件工作集上限 20 万条、FIFO 逐出，聚合永不逐出（`store.go:MaxRetainedEvents` 及注释）；PG 后端原始事件按月 RANGE 分区、保留期整分区 DROP（`pgstore.go:pruneEvents`，`TestPGEventPartitioningAndPrune`），去重账本随保留期收缩 | 聚合表（`UnitAggs`）无上限——高基数 unit × metric 组合可使其无界增长。建议：配额或基数监控（技术方案 §9"维度基数"限制未在聚合侧落地） |
| 快照/配置膨胀 | 配额：MaxFlags 500、MaxFlagBytes 64KiB、MaxSegments 200、MaxExperiments 100（`platform/types.go:DefaultQuotas`；`TestQuotaEnforced`）；DSL 结构性上限见 §2.3；服务端按 namespace 缓存单一 revision 快照，revision 变更即替换 | **无显式"快照总字节"配额**，间接上限 ≈ 500 × 64KiB ≈ 32MiB/环境；`DefaultQuotas` 自述为保守开发默认值，"production values must come from load testing"——`test/perf` 已提供本机回环压测基线（实施状态"性能证据"节），生产配额仍须以生产同构环境复核 |
| SDK 客户端侧反压 | Go SDK 曝光队列上限默认 10,000，溢出丢弃并计数（`pkg/sdk/client.go:ExposureQueueCap`、"Overflow drops"；`TestExposureQueueBounded`），失败批次有限重试后丢弃计数，不无界缓存 | 丢弃曝光会造成 SRM 可见的缺失——SDK Status() 暴露丢弃计数供监控（实施状态 §11 行） |

### 2.7 隐私面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| 原始单元身份落盘 | 原始 unit 明文只存在于请求 DTO 与求值内存中，入库前经 `HashUnit` 伪名化（`sdk_handlers.go` collect DTO 注释"plaintext; HMAC'd server-side, never stored"）；伪名 = 逐层 `hex(HMAC-SHA256(key, ws‖0x00‖v))`，密钥环链式（`store.go:HashUnit/hashUnitLayer`），租户（workspace）参与 HMAC 输入，跨租户不可链接 | HMAC 密钥泄露 + 原始事件库泄露 → 可字典枚举重识别（unit 值域小时尤其）。密钥保护见下行 |
| 伪名化密钥保护与轮换 | 文件后端：store 文件写入 tmp 后 `Chmod 0600`（"contains HMAC key material"，`internal/platform/persistence.go`）；PG 后端：版本化 `hmac_keys` 密钥环表，重启稳定；`RotateHMACKey` 追加新层并在**单事务**内把全部存量伪名（原始事件/曝光集/split 曝光集/聚合键）链式重哈希（内存 `store.go:RotateHMACKey` + PG `rehashStored` 临时表重映射），链式性质保证同一单元跨轮换身份可链接；`TestPGHMACKeyRotation`、`TestFileBackwardCompatSingleKey`（单密钥环与历史方案字节一致） | 轮换目前只有存储层入口，**未暴露管理 API**（实施状态已知限制 1："需要审计流程后再开"）；轮换是同步全量迁移，大库上会长时间持锁。gonacos 侧 `-auth-secret` 为空时每次启动随机，集群模式必须显式设置（`cmd/golab/main.go` flag 说明） |
| 审计被篡改/删除 | 审计只追加：顺序 ID、无更新/删除代码路径（`store.go:Audit`）；PG 侧 `INSERT … ON CONFLICT (id) DO NOTHING`（`pgstore.go:commitAudit`），schema 无审计 UPDATE/DELETE 语句；`TestAuditTrailRecorded` | 无密码学防篡改（hash chain / WORM）——持有 DB 或文件权限者可改。建议：部署基线定期导出审计至只增存储 |
| 报告泄露个体数据 | 报告仅输出汇总统计（单元级充分统计 N/Σx/Σx²，实施状态 §10.2）；对外只展示聚合；原始 prompt/response 不进普通事件（事件 DTO 无自由内容字段，仅 ID/数值/时间） | **低样本切片抑制未实现**（实施状态 §10.2 行"未做：低样本切片抑制（v1 报告不切片）"）——当前报告不提供任意维度切片，故暴露面小，但若引入切片必须先实现抑制；原始事件中保留 `sessionId/taskId` 原样值，属准标识符，受保留期（默认 30 天）约束 |
| 数据保留 | 事件保留期配额（默认 30 天）；PG 按保留期整分区 DROP + 边界删除 + 账本收缩（`pruneEvents`）；内存 `PruneEvents` 同步收缩 | 聚合行不随保留期删除（是充分统计，by design）；"被遗忘权"式单单元删除无 API |

### 2.8 AI 资源面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| 已发布内容漂移（底座被改） | 发布即 pin：`PublishVersion` 取得 gonacos 版本号与 MD5 摘要，并**下载回读比对**（内容不一致即 `ErrDigestDrift` 拒发，`internal/ai/service.go:PublishVersion`）；此后每次 `Resolve` 重新计算摘要比对 pin 值，漂移 fail-closed（`Resolve` 中 `md5Hex(content) != pin.Digest` → 409）；label 不作为版本指针（技术方案 §7；实施状态 §7 行）；P3 验收"已发布版本内容不可漂移 ✅" | 摘要算法是 MD5——这是**镜像 gonacos 内部摘要**的选择（`service.go:md5Hex` 注释），防意外漂移足够，对抗蓄意碰撞攻击偏弱；威胁前提是攻击者已能写 gonacos 底座（需突破 loopback/进程边界）。建议：GoLab 侧索引可加 SHA-256 双摘要 |
| 撤销不生效（快照滞后） | 紧急撤销走实时门禁，不依赖快照同步：`Revoke` 写撤销表 + 审计；`Resolve` 每次先查撤销（`ErrRevoked`）；长任务执行前调用 `/sdk/ai/check`，命中撤销返回 **423 Locked** + `anyBlocked`（`ai_handlers.go:handleSDKAICheck`，§7"执行前校验"）；重启后仍生效（`TestRevocationBlocksImmediately`、`TestAISessionFreezeAndRevocation`；实施状态 §7 行） | 门禁是**契约式**的：冻结快照可在离线/不调 check 的客户端上被执行，服务端无法强制。建议：SDK/Agent 框架把 check 做成执行前必经步骤并监控调用率 |
| 跨租户读取 AI 内容 | 见 §2.2 所有权索引行；管理面读**原始内容**需独立授权 `ai.content.read`，仅 owner 角色（`rbac.go`；`handleResolveAIResource` 注释"§9: 读取原始 AI 内容单独授权"）；SDK 面 resolve 严格以 token 的 ws/env 定界（`handleSDKAIResolve` 注释）；跨 scope 一律 404 不泄露存在性（`mapAIError` 注释"never 403 — existence must not leak"）；会话冻结任一资源解析失败则**整体失败**，不产出部分绑定（`handleSDKAISession` 注释"must never start on partially resolved config"） | SDK token 持有者可读本 scope 全部已 pin 内容——SDK token 属服务端凭证，浏览器场景禁止下发原始内容（技术方案 §7；实施状态 §8 行） |
| 不可托管资源被伪造版本语义 | mcp/pipeline 在 gonacos 中无不可变版本语义，`RegisterResource` 显式拒绝托管（`ErrTypeUnsupported`，"managed hosting is disabled until an adapter provides it (§7)"），不伪造 `name@v1` 验证；`TestUnsupportedTypesRefused` | 需求方会转向外部适配器——适配器接入时需重做本节的 pin/摘要/撤销等价物 |
| 未白名单模型混入变体 | modelRef 白名单在 **flag 发布钩子**强制：`SetAI` 挂 `Admin.FlagPublishHook` → `ValidateFlagModelRefs`，未入白名单的 provider/model 无法发布（`ai_handlers.go:SetAI`；`service.go:ValidateFlagModelRefs`）；会话冻结时 `ResolveModelRef` 再次校验（`ErrModelNotAllowed` → 400）；白名单管理需 owner（`PermAIContentRead`）；`TestModelAllowlist`、`TestValidateFlagModelRefs`、`TestAIPublishRequiresAllowlist` | 白名单是配置治理而非运行时网络管控——GoLab 不代调模型，实际调用方仍可能绕过白名单直连供应商（在 GoLab 边界外） |
| AI 端点未接线被探测 | AI 服务未装配时所有 AI 端点 501 `ai_unavailable`（"never pretends to broker resources it cannot verify"，`requireAI`） | 无已知残余 |

### 2.9 供应链 / 依赖面

| 威胁场景 | 现状缓解（证据） | 残余风险与建议 |
|---|---|---|
| gonacos 依赖漂移/不可复现构建 | 已从 sibling `replace` 固定为确定 commit 伪版本 `v0.0.0-20261006082753-5ac0bf0b6806`（= main@5ac0bf0b6806…，`go.mod` 注释），构建仅依赖模块缓存；契约行为由 `test/smoke:TestEmbeddedGonacosRoundtrip` 在每次测试运行中复核（Publish/Get、AI prompt 版本读写） | gonacos 当前 pin 含 Apache-2.0 LICENSE/NOTICE；旧固定提交的项目原创贡献适用范围由上游新提交明确，许可清单记录哈希；实际第三方文件与分发物仍须人工复核 |
| gff core 依赖 | 固定 `thomaspoignant/go-feature-flag/modules/core v0.7.2`；其所需 `nikunjy/rules` fork replace 已在本仓库 `go.mod` 重述（`=> hairyhenderson/rules v0.0.0-20250704181428-…`）；legacy 求值路径有对照测试 `TestGffCoreLegacyEvaluate`；GoLab 原生算法与 gff 分离，黄金向量 20 条经独立 Python 实现交叉验证（`pkg/bucketing/testdata/golden_vectors.json`，`TestGoldenVectors`） | 传递依赖许可证据已列入 `third_party/licenses.json`，仍需人工复核原文 |
| 依赖升级引入回归/漏洞 | 升级流程要求重跑契约/兼容测试（实施状态已知限制 3；技术方案 §12.1）；`Makefile` 提供 `vet`/`test`/`smoke`、许可复核和发布阻断目标；`.github/workflows/license-audit.yml` 检测依赖或证据漂移 | 尚未配置 govulncheck/SBOM；许可证自动初筛不替代法律复核 |

### 2.10 部署基线（运维职责，仓库未实现）

以下各项在技术方案 §9/§11 中被划入"部署基线"，仓库代码**未实现**，威胁模型如实标注为运维侧缓解，不计入"已缓解"：

| 项 | 现状 | 责任与建议 |
|---|---|---|
| TLS 终止 | `cmd/golab` 与 `cmd/golab-proxy` 均监听明文 HTTP（默认且建议 loopback：`127.0.0.1:8688`/`8689`）；仓库无 TLS 配置代码 | 运维：前置 TLS 终止（LB/Ingress）；租户流量绝不走明文网络 |
| HMAC 密钥轮换 API | `Store.RotateHMACKey` 只有存储层入口，未暴露管理 API（实施状态已知限制 1） | 运维：按迁移流程执行；仓库：待审计流程设计后再开 API |
| 备份 / 恢复联合演练 | gonacos Snapshot、PostgreSQL、密钥须按同一恢复点联合备份与演练（技术方案 §11）；**演练未做**（实施状态已知限制 1） | 运维：多租户生产前完成一次端到端演练并核对 revision |
| 多节点 / 集群 | 双真实进程与共享 PostgreSQL/Redis 的发布对账已验证；普通管理写入由 `meta.control_version` 条件事务保护，冲突返回 409；令牌认证直接查库。其余管理读路径仍依赖对账 tick 收敛（实施状态已知限制 2） | 运维：保留对账任务并监控冲突重试；参见[多副本管理写入](多副本管理写入.md) |
| gonacos 非 loopback 的生产网络验证 | 代码提供 opt-in 闸门（§2.2），但技术方案 §12.5 要求的"生产验证未授权直连被拒"未完成 | 运维+仓库：多租户生产前必办（§3.2） |
| 正式压测基线 | `test/perf` 已提供三项正式压测（本地求值 P99、发布→SDK 可见时延、SSE 并发连接；`-short` 下跳过），结果连同硬件与样本量已记入 [实施状态](实施状态.md)"性能证据"节，§13 待测目标在本机回环基线下达成 | 运维+仓库：本机回环基线不等于跨网络 SLO；生产配额（`DefaultQuotas` 自述为开发保守值）与限流参数仍须以生产同构环境压测复核后设定 |

## 3. 残余风险汇总与生产多租户前必办清单

### 3.1 残余风险汇总表

| # | 残余风险 | 威胁面 | 严重度（多租户生产语境） | 处置 |
|---|---|---|---|---|
| R1 | gonacos 许可文件已补齐；非 loopback 场景的生产网络与租户隔离验证未完成 | 供应链/隔离 | 高（合规+隔离前提） | §3.2 第 1、2 条，未完成不得对外 |
| R2 | 全局 owner token 与 bootstrap token 为平台级单点凭证，无 TTL；bootstrap 明文出现在一次性启动日志 | 凭证 | 高 | 立即轮换/吊销 bootstrap；部署侧凭证保管 |
| R3 | collect token 被盗后可在本 scope 内伪造曝光/转化数据（服务端无法区分） | 统计完整性 | 中（SRM/数据健康门部分兜底） | 最小化发放、监控摄入异常 |
| R4 | 日志/审计注入（用户字符串未转义）；500 错误回传内部错误原文 | 注入 | 低–中 | 仓库待办：渲染层转义、500 收敛 |
| R5 | AI 紧急撤销门禁依赖客户端调用 `/sdk/ai/check` 的契约 | AI | 中 | SDK/框架侧强制 + 调用率监控 |
| R6 | AI 内容摘要为 MD5（镜像 gonacos），抗蓄意碰撞弱 | AI | 低（前提为底座已被攻破） | 可加 SHA-256 双摘要 |
| R7 | 聚合表无基数上限；逐事件字节配额未强制；限流为固定窗口且无 IP 维度 | DoS | 低–中 | 配额生产值由压测给出；边界限速 |
| R8 | HMAC 密钥泄露+事件库泄露 → 伪名可被字典重识别；轮换为同步全量迁移 | 隐私 | 中 | 密钥 0600/PG 表保护；轮换窗口择期 |
| R9 | 低样本切片抑制未实现；按实验 AnalysisWindow 过滤聚合未做；文件后端去重仅内存窗口 | 隐私/统计 | 中 | 与实施状态已知限制 1、5 一致；切片功能前置实现抑制 |
| R10 | 单节点假设贯穿存储/分区 DDL/读路径；多副本未验证 | 部署 | 中 | §3.2 第 5 条 |
| R11 | TLS、备份恢复演练、压测基线、密钥轮换 API 属部署基线，仓库未实现 | 部署 | 见 §2.10 | §3.2 第 3、4、6 条 |

### 3.2 生产多租户前必办清单

与 [实施状态](实施状态.md)"已知限制与生产前必办"及技术方案 §12 验收前置一致；本文（威胁模型）即 P0 退出条件中的威胁模型项，完成后 P0 剩余缺口为正式压测基线：

1. **复核 gonacos 许可与分发内容**（技术方案 §12.5；实施状态已知限制 3）：当前固定版本有 Apache-2.0 LICENSE/NOTICE；每次升级与实际分发仍须核对第三方声明。
2. **生产网络策略验证**：gonacos 底座对租户直连不可达（默认 loopback；如显式 opt-in 非 loopback，须按技术方案 §9 完成"未授权直连被拒"的实测）。
3. **备份/恢复联合演练**：gonacos Snapshot + PostgreSQL + HMAC 密钥按同一恢复点演练，恢复后重建 manifest/SDK 快照并核对 revision（技术方案 §11；实施状态已知限制 1）。
4. **密钥轮换流程**：以 `Store.RotateHMACKey` 为存储层入口制定运维迁移与审计流程；是否暴露管理 API 待审计设计后决定（实施状态已知限制 1）。
5. **多节点决策**：共享 PostgreSQL/Redis 双副本已验证；管理读路径除令牌认证外仍由周期重载收敛，扩容时配置并监控对账间隔（实施状态已知限制 2）。
6. **正式压测与生产配额**：以压测确定 P99/SLO、限流参数与 `Quotas` 生产值（`DefaultQuotas` 为开发保守值；实施状态 P0 行）。
7. **浏览器/瘦客户端接入方式**：只经 `/api/v1/sdk/eval`（可经 `cmd/golab-proxy`），不下发全量快照（实施状态已知限制 6）。
8. **依赖升级纪律**：gonacos/gff 升级必须重跑 `test/smoke` 契约测试与全量测试，并建议补充 govulncheck 类漏洞扫描（当前仓库未配置）。

---

*本文档由代码取证生成，取证时点为撰写时的工作树；代码演进后应复核 §2 各表的证据路径是否仍然成立。*
