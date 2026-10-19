# Agent 协议 ADR 001

日期：2026-10-16。状态：契约实现中，尚未完成 P0 退出验收。

范围与完整门禁以 [实施与验收方案](统一Agent协议实施与验收方案.md) 为准。本文件不缩减 P0—P6 或 A01—A28。

## 传输与版本

首期使用 `GET /api/v1/agent/connect`、WSS、`grayling.agent.v1` 子协议。一个二进制 WebSocket 消息包含一个 `Envelope`，不启用压缩、不承载业务数据。HTTP Authorization 为唯一机器凭证入口；query token、cookie、开发自动登录身份不能认证 Agent。入口默认关闭。

[agent.proto](../api/agent/v1/agent.proto) 是消息与错误目录的权威定义；`protocol_major=1`、首期协商 minor=0。Hello 中范围必须包含服务端支持版本。Welcome 及后续消息采用协商版本；不兼容主版本、未知必需能力拒绝。字段 1—13 为信封元数据，14—19 为未来可选元数据，20 及以上只用于 payload。新增字段不复用已有编号。每帧必须恰好一个 payload，包括未知 payload 在内；重复 oneof 拒绝，禁止 last-one-wins 隐藏命令。未知可选元数据保留，未知 payload 返回 UNSUPPORTED_MESSAGE。

`codec.go` 仅执行信封、大小、版本和命令标识检查，不替代领域校验。处理器必须验证权限、scope、boot、fencing、期限、能力、资源内容和状态机。欢迎/在线不证明就绪，消息回包不证明 Prepared、Applied 或业务验证。

## 授权矩阵

scope 只能从机器凭证获取，请求中的 scope 只能缩小。每条命令在执行事务中重新检查当前会话和凭证撤销状态；不能只在握手时认证。

| 能力 | 客户端消息 | 额外约束 |
| --- | --- | --- |
| 会话基础 | Hello、Heartbeat、Goodbye | 专用 Agent audience、允许的 agent/instance/boot；Heartbeat 不续租 |
| config.read | Subscribe/Resume（policy/bundle/revocation）、FetchResource、ResourceApplied/Rejected | 每次读取及下载均验证 scope 与不可变资源摘要 |
| discovery.read | Subscribe/Resume（discovery） | 恢复游标绑定 principal/scope/resource，不跨租户复用 |
| instance.register | Register、UpdateInstance、Deregister、ReadinessReport、DrainReport | 凭证绑定 instance/service；新 boot 接管另需部署授权及 CAS |
| instance.renew | RenewLease | instance/boot/lease/connection 双 fencing；服务端时钟 |
| release.report | Prepared、Applied、Rejected、ReleaseStatusQuery | 绑定当前 release/Bundle/generation/instance/boot |
| events.write | EventBatch | 每 eventId 去重，有界批次，逐条结果 |
| 订阅所有者 | Unsubscribe | 只能操作当前 principal/scope 的订阅 |

Prepare/Activate/Abort/Revoke/DrainRequested 仅由审批后的控制面状态机发送，不能由 Agent 请求触发。客户端发服务端类型应拒绝。订阅能力按资源逐项验证，拥有一类能力不隐含另一类。

## 幂等、持久化与权限边界

可变更命令携带 operationId，以 `(scope, principal, operationId)` 存储请求摘要和结果；相同 ID 不同请求返回 CONFLICT。去重与治理变更必须在共享存储事务中提交。Naming 跨存储使用意图、幂等写入、读回与 reconcile；确认前不发布可选成员。消息关联 messageId 不能代替 operationId。遥测使用 eventId 的独立保留窗口。

租约、连接 epoch、发布 generation、Prepared/Applied、outbox 和幂等状态必须从第一版支持 PostgreSQL 事务/CAS。JSON/SQLite 不对外宣称多副本能力；生产多副本不使用进程内计数器。发现目录不是部署授权来源。实现前须验证底座 Naming API 的真实语义。

## 签名与恢复决定

采用 Ed25519；密钥只在控制面，Agent 从显式配置的 keyId/公钥集合引导。`SignedResource.claims` 保存签发时的原始字节，签名输入为 ASCII `grayling.agent.v1.signed-resource`、一个零字节及原始 claims 字节。claims 的严格编码与跨语言 fixtures 已按下文“签名 claims 编码 v1”冻结；纯签名验证已实现，持久化与运行时接入尚未完成。payload 字节不经 Protobuf 重新序列化计算摘要。

高水位与缓存原子持久化；签名验证成功不能绕过已保存高水位。控制面恢复 epoch 需要独立恢复授权，不能通过删除缓存降低版本。密钥轮换重叠、退役/撤销、离线期限均须实测。

## 生成与检查

冻结 `protoc 3.21.12` 和 `protoc-gen-go v1.36.11`。插件安装命令：

```sh
GOWORK=off go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
make generate-agent
make check-agent-generated
make test-agent-contract
```

生成代码纳入版本管理；生成器变更单独评审。protojson 将 uint64/int64 输出为十进制字符串；测试覆盖超过 JavaScript 安全整数及 uint64 最大值。不得将 deterministic protobuf marshal 当作规范签名编码。

## 待完成的 P0 证据

- 内置 gonacos 固定模块 `v0.0.0-20261006194453-ea39c83bf6f0`，源代码确认存在进程内 `Server.Services().Naming`；注册/查询/订阅记录/注销与租约 POC 已完成，见下文；真实订阅推送与认证仍待验证。
- 外部 Nacos 精确版本、认证及 Naming 真实能力测试待建立；未运行不计 PASS。
- gogw 固定 `62b1f935f7612dbb730504e8210d8cffe2f4aef7`，本地工作区干净；尚未实现本方案 Gray Service。
- 单端口 WSS、跨实例可达地址、部署证书、信任根、生产 scope 配额与升级回退版本待实测。
- [预算 profile](../profiles/agent/acceptance-v1.json) 在运行前记录方案给定门槛；null 配额不是无限制，运行时启用前必须显式填写。压力 fan-out、网关 body/事件频率/RTT/TLS 仍需在运行前冻结，不能据当前 profile 声称性能通过。

## 签名 claims 编码 v1（已实现）

`GAR1` 四个 ASCII 字节为格式版本；以下字段按表中顺序拼接，所有整数采用固定宽度大端编码，无填充和可选字段。字符串使用 `uint16` UTF-8 字节长度前缀（不含前缀自身），必须非空、最多 512 字节、UTF-8 合法且不含 U+0000—U+001F 或 U+007F。解析器拒绝截断、尾随数据、未知 kind 和未知格式版本。

| 顺序 | 字段 | 编码 |
| --- | --- | --- |
| 1—3 | workspaceId、environmentId、serviceKey | 三个长度前缀字符串 |
| 4 | resourceKind | uint32，proto 中的 1—4 |
| 5 | resourceKey | 长度前缀字符串 |
| 6—7 | controlPlaneEpoch、resourceRevision | 两个 uint64，均必须大于零 |
| 8 | bundleHash | 32 字节 SHA-256 原始值，非全零 |
| 9—10 | issuedUnixMs、expiresUnixMs | 两个 int64 的大端位表示，须 `0 < issued < expires` |
| 11 | keyId | 长度前缀字符串 |
| 12 | payloadHash | 32 字节 SHA-256 原始值，非全零 |

外层 keyId 必须等于签名 claims 中的 keyId。签名输入仍为上述 domain separator 加 claims 原始字节；不是 claims 的 Protobuf 或 JSON 序列化。验签后检查 expected scope/kind/key、payload 摘要、有效期，再检查对应资源的持久化高水位。资源签发和到期时间必须完全包含在信任根的接受窗口内，当前时间采用 `[issued, expires)`，不额外宽限签名到期。

同 revision 的 payloadHash 或 bundleHash 不同视为冲突；任何不同 controlPlaneEpoch 均拒绝由普通更新接管，新的 epoch 只能由后续独立恢复授权流程安装。Verify 为纯校验，不写高水位，也不令客户端就绪；缓存与高水位原子写、恢复授权、资源 schema/依赖编译及 RuntimeView 仍须继续实现。

[Python/Go 公共测试夹具](../test/agent/fixtures/signed-policy-v1.json) 使用公开 TEST-ONLY 密钥。Python 生成器 `scripts/agent_signature_fixture.py --check` 使用 `cryptography==41.0.7`，Go 测试独立解析并验证，版本为 `9007199254740993`。生产代码只依赖 Go 标准 Ed25519 和已存在的 protobuf；未新增运行时加密依赖。

## 内置 Naming POC 实测

`GOWORK=off go test -tags=integration -race ./test/agent -run Naming -count=1` 已验证固定 gonacos 的 `Server.Services().Naming` 进程内注册、带元数据全量查询、订阅记录添加/查询/删除、Heartbeat、注销，以及注销后心跳拒绝。另用可控时钟验证其原生临时租约为 15 秒，过期标记 unhealthy 并通知，保留目录记录；该心跳可将过期实例恢复健康，接口没有 Agent boot/lease/connection fencing。

因此不能将底座心跳当作本方案 60 秒租约或准备证据。后续必须先完成 Grayling 共享权威状态 CAS，才允许触达 Naming；底座 healthy 不直接赋予网关可选资格。POC 的订阅测试只覆盖服务端记录与通知钩子，尚未覆盖真实客户端推送链路、认证或外部 Nacos，不满足 A09 完整验收。

## PostgreSQL 会话与 WSS 骨架（阶段实现）

`internal/agent/sessions.sql` 为独立、只扩展的 Agent schema v1；迁移在 advisory transaction lock 内执行，版本不认识时回滚。复用现有 PostgreSQL 与 pgx，不新增存储依赖。两个独立连接池通过共享作用域准入行和实例行锁串行化 Connect；connectionEpoch 为数据库单调 bigint，溢出拒绝，断线保留记录以免 epoch 复用。

专用凭证使用 `grayling_agent_` 前缀和 256 bit 随机 secret，只存 SHA-256 摘要，绑定 principal、scope、agentId、instanceId、bootId 和权限。IssueCredential/SetScopeLimit 是供后续受 RBAC 管理接口调用的内部 provisioning 方法，目前尚未接入管理 API/CLI。scope 未显式配置连接上限时拒绝 Connect。同 boot 只能由同 agent/principal 重连；换 boot 必须持有控制面新签发的 boot 绑定凭证并声明预期旧 boot，事务内 CAS 检查失败不占用新 epoch。

会话检查点只提供 `session` 能力：Hello/Welcome、Heartbeat/Ack、Goodbye、GoAway、Error。Heartbeat 仅续连接 30 秒有效期，不续实例租约或策略。凭证和会话每次写入重新在主库检查；撤销和写入通过凭证锁排序。旧连接关闭只影响精确 sessionId/epoch，不能关闭继任连接。跨节点主动检查每 2 秒一次，DB 检查/写入期限 2 秒；检测失败即停，不在内存里继续确认。scope 上限及每节点连接/认证/握手总量分别有界。

`Channel` 要求真实 TLS、精确 Host 白名单、明确节点容量、Authorization 和正确子协议；拒绝所有 Origin、cookie、query，默认禁用 WebSocket 压缩。握手超时 5 秒，消息上限 256 KiB，控制队列 64 项和 8 MiB 预算，单 writer，写入超时 2 秒，每连接 50 消息/秒（burst 100）。无资源/遥测能力时不声明其队列或订阅支持。握手失败可在未分配 session 的 Error 中回报；除 Error/Hello 外不能缺少服务端 session。

API Server 可用显式 `Options.AgentChannel` 注入入口，该路由独立机器认证，开发自动登录不包装它；未注入时不存在该路由。App/cmd 尚未接入 Agent 启用、管理与完整生命周期配置，因此此阶段并未在现有部署中默认开放端口。

该会话检查点的 `pkg/agent.Dial` 为早期客户端骨架，固定 WSS URL、拒绝 userinfo/query/HTTP 重定向和 cookie jar，检查 Welcome scope、能力、时间（±5 秒）和限额，提供心跳及 Close。后续已由 805a696 的统一 Go SDK 替换，支持并发请求、事件分发与重连；当前策略对接见下文，P1/P2 仍未全部退出。

实测套件：`make test-agent-channel`。真实 PostgreSQL 下验证 12 次双池并发重连 epoch 唯一、重开不倒退、未授权 boot 接管失败、跨节点撤销、容量限制和精确旧连接关闭；真实 TLS API 共端口验证健康接口与 WSS、双节点 fencing、主动撤销、GoAway、开发 cookie/query/错误受众/Host/Origin 拒绝、非二进制/损坏帧拒绝、连接池不可用时不确认，以及 Go 客户端实际连接与心跳。P1 全部泄漏/风暴/恢复游标等退出项仍待补齐，P3 的实例 lease fencing 也尚未实现。

## 签名策略持久发布与读取链路（P2 阶段实现）

Agent schema v2 新增不可变 `agent_resources`、权威 head 和发布 operation 记录；v1→v2 在同一迁移事务中完成。资源 revision 使用 PostgreSQL `numeric(20,0)`，完整支持 uint64，JSON/URL 以十进制字符串表示。资源 revision 是签名策略版本；payload 中 `model.Snapshot.ManifestRevision` 保留原配置 manifest 版本，二者不混用。

内部控制面 `Publisher.PublishPolicy` 先严格解码并完整编译 snapshot，检查 scope、map key/文档 key 与 schema，再在事务内分配版本并用 Ed25519 签名。它不是机器消息处理器，也尚未接管理 RBAC/审批。调用方必须先取得发布授权、持久化原请求字节和 operationId；重试复用相同字节。`(scope, principal, operationId)` 唯一记录、请求摘要、head CAS、不可变资源及结果 revision 同事务提交；同 ID 不同载荷冲突，旧 expectedRevision 不分配新代次。记录默认保留 24 小时，同一过期 ID 重用前清理；全量保留清理与运维仍待 P6。签发使用 DB 时间，DB 与本机时间相差超过 5 秒时拒绝；epoch 恢复授权仍未实现。

`config.read` 只向持有对应权限的会话声明。Subscribe 验证 scope 与 policy 类型，返回完整 Snapshot；192 KiB 以下内联，较大资源只传不可变 ResourceRef。每会话最多保存 128 个订阅身份（含已取消身份以维持序号），取消后复用 ID 也不倒退 streamSeq；活动订阅不能在未取消时换资源。恢复游标当前一律返回带 subscriptionId 的 ResyncRequired，重新全量同步，不假装支持增量恢复。定期从主库批量读取订阅 head，通知合并为最新 revision，避免依赖事件总线或 sticky session 才能恢复；该机制的 10,000 连接压力预算尚未实测。

订阅的 Snapshot/ResyncRequired 是带 subscriptionId 的主动事件，ReplyTo 留空以兼容 Go SDK 的事件分发；FetchResource 使用关联响应。订阅错误通过带 subscriptionId 的主动 Error 回报。Go SDK 将主动 Error 作为连接错误并按 retryable/RetryAfter 决定重连；NOT_READY 可重试。资源通知有独立 128 项低优先级队列，控制有 64 项队列，总预算 8 MiB；资源不能使用预留给控制的 256 KiB。单 writer 优先检查控制队列，溢出关闭并要求客户端重同步，不确认实际生效。

HTTPS 资源下载入口已接 API 的可选 Agent handler，必须同时提供机器凭证、`X-Grayling-Agent-Session` 和 `X-Grayling-Agent-Epoch`，每次下载重新验证主库权限及 fencing。scope 不在 URL 中，凭证不进入 URL；当前仅 policy。下载独立限制为每节点 16 个，与 WSS 连接槽分开；存储事务在写网络前结束，下载不占 WebSocket writer。资源总线缆字节上限 64 MiB，SHA-256 校验原始 SignedResource 字节，不依赖重新序列化。HTTP 写期限 30 秒；完整下载/流控压力和配额配置化仍需 P6 验收。

`Client.DownloadPolicy` 组合受鉴权下载、引用元数据/签名绑定、scope、高水位和完整 evaluator 编译；返回结果尚不代表缓存已持久化或活动 RuntimeView 已切换。Policy 不暴露底层 Snapshot，JSON variation 返回深拷贝。Go SDK 握手与心跳检查服务端时钟 ±5 秒，普通心跳不延长签名有效期。OpenAPI 已记录可选入口和下载要求，路由一致性检查覆盖 Handle/HandleFunc 两类注册。

已实测真实 PostgreSQL 双节点发布/读取、8 次并发同操作去重、旧 CAS 与载荷冲突、uint64 上界、重开、scope/权限负例、WSS 初始快照、跨节点通知、完整重同步、300 KiB 资源引用/下载、签名验证后本地求值、JSON 输出不可变、错误引用版本与旧连接下载拒绝。命令 `make test-agent-policy`；完整资源/会话套件见验收目录的 `policy-and-channel.jsonl`。

后续仍必须完成：接入 configstore/审批发布源与 App/cmd、可信缓存及高水位原子持久化、离线恢复与陈旧/撤销边界、实例注册/租约与完整 RuntimeView，以及主方案 P4—P6。当前链路不能代替正式发布、实际业务执行或生产验收。

### P2 本地可信缓存检查点

`pkg/agent.PolicyCache` 使用私有目录、显式首次初始化与独立配置的 32 字节 HMAC 密钥，将签名资源、版本水位、已观察时钟上界及撤销集合放在同一 SQLite 事务中提交（WAL、synchronous=FULL）。发布内存视图发生在提交成功后。缓存缺失、损坏或身份绑定不匹配不自动清空水位；真实但过期的缓存保留水位并进入 NOT_READY，等待在线更新。持有文件锁以拒绝第二个缓存所有者；该锁只约束遵守协议的进程，私有目录和主机权限仍是信任边界。

PolicyView 保持一次捕获的 evaluator；普通新版本不修改已有视图，但关闭、签名根退役和 bundle 撤销立即约束已有视图。缓存重开、读取、信任根更新和心跳不刷新 verifiedAt。策略有效期与最长五分钟新鲜度分别检查。时钟小幅回退按已观察上界钳制，超过五秒拒绝；正常关闭持久化已观察上界。尚未证明非正常断电后的全部时钟边界，也不能依赖主机时钟对抗主机管理员。

HMAC 密钥须独立于缓存目录配置。恢复整份旧的、封印有效的缓存，只有外部独立水位或可信防回退存储才能识别；CacheOptions.Minimum 提供独立水位入口，不能将目录内 HMAC 描述成可检测任意备份恢复。正常 Install 拒绝跨控制面 epoch 切换；独立恢复授权链尚未实现。Revoke 当前要求调用者先完成命令身份和 scope 验证，尚未接入 WSS 撤销命令。Linux 上完成组件测试，Windows 文件权限与运行行为尚未验收。

### 独立恢复授权 GRA1

P2 缓存新增受独立授权的 epoch 切换，更新上一检查点“恢复 API 未实现”的状态。GRA1 的唯一编码依次为：magic `GRA1`；uint32 大端 GAR1 目标 claims 长度及原始目标 claims；旧水位（epoch/revision 各 uint64 大端、payloadHash32、bundleHash32）；nonce32；授权签发/到期时间各 uint64 大端毫秒；uint16 大端恢复 keyId 字节长度及 UTF-8 字节。不允许尾随扩展。授权窗口最长 15 分钟；新 epoch 必须严格高于旧 epoch，revision 可因备份恢复低于旧 revision。

独立 Ed25519 签名输入为 `grayling.agent.v1.epoch-recovery\x00` 加 GRA1 原始字节。目标 GAR1 包含普通策略 signer keyId 和策略授权期限；恢复 keyId 只选择恢复公钥。两个 trust ring 的所有公钥必须互斥，防止通过别名复用策略私钥。授权绑定完整旧水位和目标 claims；完整策略仍须通过普通验签、scope/schema/依赖编译及不可清除的撤销集合检查。授权摘要与新资源、水位同事务提交；重试核对持久摘要，不延长 verifiedAt。相同授权在后续正常版本推进后不能再次覆盖该版本。

独立 Python cryptography 41.0.7 fixture `test/agent/fixtures/recovery-v1.json` 由 `scripts/agent_recovery_fixture.py` 产生，Go 测试通过真实 PolicyCache 验证两种签名与完整求值；旧水位 revision 为 9007199254740993，覆盖跨语言精度。该实现是恢复链中的客户端防倒退组件，控制面受审计 epoch 提升、管理员审批及实际备份恢复演练仍待接线，不计完整 A07/P6 通过。

### App/CLI 默认关闭启用与关闭生命周期

P1/P2 通道现通过 `Options.AgentEnabled` 接入真实 App；空的 Agent handler 不再只限测试注入。启用要求共享 PostgreSQL、精确 Host 白名单、显式每节点连接上限；残留的 Agent 参数在开关关闭时拒绝，防止误以为配置已生效。CLI 增加证书/私钥配套参数，启用 Agent 强制 HTTPS，并在打开存储前预加载证书；默认启动仍为原有 HTTP。

App 取消时先关闭 Agent 准入和控制连接，等待 handler 后关闭共享会话池，再完成原有后台任务与平台存储清理。CLI 的 App 关闭使用独立超时，避免 HTTP shutdown 消耗截止时间后跳过 App 清理。真实 App/PG/TLS/SDK 测试覆盖心跳、关闭、重启代次延续、owner token 隔离、伪造 proxy scheme 拒绝与开关关闭路由不存在。恢复 checkpoint 与 Agent epoch 门禁尚未接线，现阶段显式拒绝两者同时启用；必须继续实现恢复流程才能满足完整方案。

### 专用管理凭证与同事务审计

schema v3 以扩展迁移新增 Agent 管理 operation 去重及审计表，不修改既有凭证/会话/资源列。旧 v2 数据升级与重复打开已通过真实 PG 测试，凭证和会话连接代次保留。Agent 与 platform 使用独立连接池，因此管理变更不能调用事后 `Store.Audit` 来冒充原子审计；新路径在 Agent 自身事务内提交业务、结果和审计。审计使用数据库分配 ID，API 以字符串输出，避免与 platform audit 分配器争用。

新增三项显式 owner 权限（credential.manage/quota.manage/audit.read，均带 agent. 前缀），仅开关启用时注册管理路由、公布 capabilities。机器凭证不进入 admin audience，管理 owner 凭证也不进入 WSS。管理端发放所有权限均是授权上限，能力握手仍只开放已经实现的能力。凭证发放要求部署 boot 绑定，可授权有 expected old boot 的接管；激活权限不加入机器权限集合。

管理写入按 scope/actor/operationId 串行去重；原操作结果不可随重试改写后续配额。所有查询、撤销按完整 scope 过滤，不先全局查询 ID。密文只在首次提交后的响应返回，去重结果与审计仅保存 CredentialInfo；响应丢失后重试可取 ID 并撤销/重新发放，不持久化可解密的密文。各变更的审计插入失败回滚业务与 operation；API 对未知 DB 错误统一返回脱敏 503。读取列表/审计均为 100 条 keyset 分页。

## 不可变发布 Bundle（实现增量）

Bundle 使用已有 GAR1 的 BUNDLE kind（3），资源键为 `bundle-<sha256>`。BundleHash 是完整 payload 字节的 SHA-256，同时必须与 claims.BundleHash 一致。`MarshalBundle` 使用 JSON v2 deterministic 编码，build 与每个 build 的资源按键严格递增；接收端重新编码并比较原始字节，拒绝未知字段、重复字段及其他非规范表示。revision、epoch 和毫秒时间均使用十进制 JSON 字符串。此格式不将实例目录或 releaseGeneration 纳入不可变 Bundle。

schemaVersion=1、purpose=release-bundle；scope 使用不可变工作区/环境/服务 ID。policy 固定 key、controlPlaneEpoch、revision 和 payloadSha256；routeFlag 固定一个 string flag，valueBuilds 将该 flag 的所有实际字符串值映射到已声明 build，完整依赖校验拒绝遗漏和多余映射。build 固定 id、imageDigest、protocolVersion 和具名应用资源 SHA-256；资源不含可执行下载 URL。minimumProtocolVersion 和各 build 协议当前只接受主版本 1。最多 256 个 build、每 build 1024 个资源、1024 个路由值，沿用 64 MiB 资源上限。

默认行为必须明确为 reject，或 build 并指定已声明的 defaultBuild；后续运行时必须显式执行它，不能从 evaluator 默认值推导一次成功路由。validUntilUnixMs 是 payload 内的授权硬截止时间，重签同一 Bundle 不能越过它，签名有效期最长 24 小时。Bundle 依赖校验会用当前信任根重新验证保存的原始 Bundle，再校验策略签名、scope、精确 epoch/revision/内容摘要和相同 BundleHash。返回文档是深拷贝，调用方不能修改已验证状态。

本增量仅冻结并实现本地 Bundle 与策略依赖校验；批准配置源、持久分发/缓存、Prepared、Activate、完整 RuntimeView 及真实 gogw 接线仍待完成，不据此声明发布成功。

## 已发布配置源读取（实现增量）

`admin.Service.LoadPublishedSnapshotContext` 是 Agent 发布源接线的内部读取入口。调用方须具有 `flag.publish` 权限，并传入内部 workspace/environment ID 和正的预期平台 manifest revision；不接受显示 slug 代替签名 scope。读取复用发布锁及共享 PostgreSQL advisory lock，锁内重载平台状态并固定 revision；底座 manifest 的 scope/schema/revision 和所有有效文档头须与平台记录一致。该路径只拒绝漂移，不自动修复、不采用底座领先版本。

快照按首次取得的 manifest 逐项读取固定 DataID，不再读取一个可能已变更的 manifest。除底座返回摘要外，还独立计算每份原始文档 SHA-256；重复 key、缺失文档、不一致摘要/文档 key、未知字段和不完整 evaluator 均拒绝。删除 tombstone 不进入完整集合；尚未批准发布的 draft 不进入此源。完整策略字节使用 JSON v2 deterministic 编码，返回独立 payload 和摘要，源 manifest revision 与 Agent artifact revision 保持独立。

这只证明读取时的已发布源。后续仍须将 exact source digest、build/资源映射与 Bundle 审批绑定，再将 Bundle 与策略原子持久化发布；目前未提供对应管理 HTTP 接口或自动签发任务。既有 `acquirePublishLockContext` 的本地 mutex 等待与 `ReloadFromBackend` 不能及时被 context 中断；最终 context 检查能阻止取消后的成功返回，但尚不证明管理请求的硬截止时间。

## Bundle 与策略成对持久发布（实现增量）

`Publisher.PublishBundle` 接受已获独立授权的完整 BundleDocument 和 exact policy payload；它不承担审批、不自行读取草稿，也不分配 releaseGeneration。事务前用 `CompileBundlePolicy` 校验策略摘要、scope、完整 evaluator 及路由值映射，Bundle 内策略 revision 必须等于 expectedPolicyRevision+1。

两份资源复用现有 PostgreSQL agent_resources/heads，Bundle 内部存储键使用 `bundle/<hex>`，与合法 policy key 隔离；对外签名仍使用 `BundleKey` 的 `bundle-<hex>`。现有策略读取和版本头查询拒绝内部 slash 键。每个新 Bundle 的首个 artifact revision 为 1；后续 artifact 由内部 RefreshBundle 分配，原版本保持不可变。

锁序为 operation → policy head → Bundle head → schema 共享锁。两份资源的签名共用一次数据库 epoch/时间，期限取发布器 lifetime 与 Bundle 授权硬截止的较小值；资源写入、版本头推进和幂等结果在同一事务完成，并在提交前重新检查到期时间。关键资源 INSERT、head UPDATE 与幂等结果 UPDATE 必须实际影响一行。第二份资源失败时，不保留第一份资源或空头/操作占位。

幂等操作复用 agent_resource_operations，但请求摘要采用独立域，覆盖完整 Bundle 摘要、policy 摘要及预期版本；不可与旧 PublishPolicy 复用同一 operation。重放先于 CAS，精确读取原 policy revision 和 Bundle revision=1，返回原始字节，不重签、不续期、不回退 latest；24 小时操作记录清理后，旧请求仍受 policy CAS/Bundle 身份约束，不能覆盖。服务端依赖持久化摘要和结构身份检查读取历史签名资源，Agent 仍须用当前可信 keyring 做密码学校验。

目前这是控制面内部 API。管理审批与 exact source digest 的绑定、HTTP 发布入口、Bundle Fetch/HTTPS/缓存、Prepare/Activate、RuntimeView 及发布探测均未因此自动完成。

## Bundle 固定版本网络交付（实现增量）

`Sessions.BundleResource` 在同一事务中先检查 ConfigRead 与有效 session，再以凭证 scope 和正 revision 映射内部 Bundle 存储键；不锁资源头、不回退 latest。返回前检查 claims 的 kind、外部键、scope、版本、BundleHash、实际 payload 内容摘要、签发/存储期限和当前控制面 epoch，并再次检查 session 与数据库当前期限。读取不要求本节点配置 Publisher，历史资源仍由 Agent 的可信 keyring 做最终签名校验。

WSS Fetch BUNDLE 与 HTTPS kind=3 共用该读取入口。ConfigRead 凭证获得 bundle.read 协商能力，其他权限不隐式授予它；Bundle Subscribe 不支持。固定版本不存在与外 scope 的错误一致。资源引用的截止时间裁剪到 reader 凭证截止；较短 reader 凭证不改写已签 Bundle，HTTPS 每次重新鉴权，旧连接 epoch 不能继续下载。

SDK `DownloadBundle` 在通用有界、同源、无重定向下载之上绑定目标内容 Hash、引用 kind/key/revision/期限和 GAR1 验签。它不替代 Bundle 缓存、水位原子持久化、Prepared 或 RuntimeView。

## 固定 Bundle 可信缓存（实现增量）

BundleCache 复用资源与高水位原子封印事务，新增独立 `bundle-cache.sqlite` 和 GBC1 MAC 域，保留既有策略/发现数据库格式与身份绑定。read 阶段检查固定 Hash、sealed mark 和原始 payload 摘要；install/restore/verifier 重建以私有 typed 验证结果承载 policy/discovery/Bundle，防止种类混淆。

同 artifact identity 只允许完全相同的签名资源字节，完全重放不更新 verifiedMs；更高 artifact revision 经完整验签后原子更新水位和验证时间。BundleView 不导出裸 Bundle，只返回受动态守卫保护的文档深拷贝，或对受守卫 PolicyView 做精确依赖校验。MaxAge 最多五分钟，不越过签名和授权硬截止。

Bundle payload 绑定 policy epoch，所以提高 epoch 必须改变 BundleHash；固定 Hash 缓存不接受 recovery install，GRA1 未新增 BUNDLE kind。管理恢复流程、新 Bundle/外部水位部署与自动周期性 artifact 刷新调度仍待完成。该缓存不替代 Prepared、发布状态或 RuntimeView 原子切换。


## Bundle 授权范围内刷新（实现增量）

RefreshBundle 的请求固定 scope、BundleHash、expected artifact revision、principal 与 operation，使用独立请求摘要域。锁序为 operation → Bundle head → schema 共享锁，策略依赖只做精确版本普通 SELECT，避免反转成对发布的锁序。重放先于 CAS，只读原操作保存的版本；操作记录清理后，旧 expected revision 仍不能再次推进。

新签发必须使用部署可信 keyring 重新验证原 artifact 1 的签名、完整 Bundle 及精确 policy 依赖，并检查当前数据库 epoch/时钟；也验证新签发根当前受信。只增加 Bundle artifact revision，不改变 payload/BundleHash、策略 revision 或发布代次。新期限为当前时间+issuer lifetime、Bundle 授权硬截止、原 policy 签名截止三者的最小值，原策略过期或原根退役会阻止继续签发。

新资源 INSERT、head 推进和 operation 结果同事务提交，关键写入检查 RowsAffected，提交前重验期限；原版本不改写。历史操作重放不重新证明当前信任或期限，也不 mint，新使用仍需客户端当前验签/guard。原 artifact 1/策略的保留与其刷新授权、引用和去重窗口相协调。此处为内部控制面组件；审批/撤销门禁、周期调度及发布编排接线尚未完成。


## Prepared 持久化确认扩展

新增 Envelope oneof 字段 69 `PreparedAck`，由能力 `release.prepare` 显式协商，
保持 v1.0 基础会话兼容。旧客户端不声明该能力时不会收到新命令或确认消息。
Prepare 的 operationId 固定为持久化定向 command ID；Prepared 使用相同 ID 回报。
确认带不可变 evidence revision 与首次接收/截止时间，仅证明持久化，不推断发布状态。
回报处理使用当前会话权限、boot、租约、目标 head、发布代次和可信资源根重新校验。
不复用 ReleaseStatus 表示普通 ACK，避免把接收与容量/生效混为一谈。


## Prepare 负向确认扩展

Envelope 字段 74 为 PrepareRejectedAck，仅在额外能力 release.prepare.reject 协商后发送。
它确认当前 Prepare 的不可变负向回报，不等同于 Prepared，不把应用健康失败解释为准备成功。
同一个 command 的正负结果互斥，共用 catalog revision；新尝试须推进目标 revision。
容量晋级按全部当前目标的最坏失败率（含 pending）与逐 build/AZ 最低成功容量同时判断。


## 签名 activation-view

资源 kind 5 为独立激活授权快照，公共键 activation-{generation}，私有存储键
activation/{generation}。它和不可变 Bundle、候选发现资源分离；正文同时固定有效准备成员
及 required sources，不能把普通候选目录当作准备授权。claims 沿用 GAR1 的固定签名编码，
新增资源类型只在 release.activation.read 的精确请求中返回，不向旧客户端主动推送。
artifact revision 与 release generation、成员 revision、Prepared catalog revision 分别保存。
提交 ACTIVATING 与签名快照/来源命令同事务；源端实际安装与 Applied 必须另行提供证据。


## Activate 回报与状态分页

release.activate 为新完整能力，显式协商后才发送 Activate 与字段 75 ActivationReportAck。
Activate 携带精确 kind5 快照引用；Applied 绑定 artifact revision、control-plane epoch 及
所有实际视图版本。负回报补充 instance/boot，不因 Ready=false 被丢弃。回报正负互斥，
独立 report catalog 与 release revision 同事务提交，状态最多进入 VERIFYING。
ReleaseStatus 是历史诊断，按源分页并使用两种 revision 检测跨页变化；过期历史可查，
不能据此延长签名授权或绕过实际探测门槛。
