# WebSocket Agent Go SDK

状态：客户端、默认关闭的服务端 Agent 通道及部分领域服务已实现，整体实施与验收尚未完成。依据[统一实施与验收方案](统一Agent协议实施与验收方案.md)和工作区已有协议契约实现。不能据此声称 P0—P6 或 A01—A28 全部通过。

## 工作区与契约来源

本 SDK 在独立分支 `feat/agent-ws-sdk`、worktree `grayling-ws-sdk` 开发，基线为 `41b16d7`。`api/agent/v1`、资源验签代码及签名 fixture 从 Grayling 原工作区已有未提交实现复制，以便客户端基于同一契约编译和测试；原工作区未改动。同步服务端分支时，应先合并这组公共契约，避免两份生成文件漂移。

契约入口为 [agent.proto](../api/agent/v1/agent.proto)。WSS 使用 `grayling.agent.v1` 子协议，一条二进制 WebSocket 消息承载一个 Envelope，压缩关闭。传输使用仓库已有 `github.com/coder/websocket v1.8.15`，重连使用 `github.com/cenkalti/backoff/v5 v5.0.3`；不自行实现 WebSocket、Protobuf 或加密算法。运行时 protobuf 版本保持原仓库固定值。

## 使用方式

导入 `github.com/saker-ai/grayling/pkg/agent`。完整可编译示例见 [example_test.go](../pkg/agent/example_test.go)。

```go
client, err := agent.Dial(ctx, agent.Options{
    URL:   "wss://grayling.example.invalid",
    Token: agentToken,
    Hello: &agentv1.Hello{
        AgentId:    "checkout-agent",
        InstanceId: "checkout-1",
        BootId:     bootID,
    },
})
if err != nil {
    return err
}
defer client.Close()

err = client.Subscribe(ctx, "checkout-policy", &agentv1.Cursor{
    Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY,
    Key:  "checkout-policy",
})
```

空 URL 路径自动补 `/api/v1/agent/connect`，其他路径拒绝。默认必须 WSS；测试可显式 `AllowInsecure: true` 使用 WS。使用自定义 HTTPClient 配置受信 CA 或 mTLS；不得关闭证书验证。SDK 复制 HTTPClient 并禁止重定向，避免凭证跨 origin 转发。URL 中的 userinfo、query、fragment 和 token 换行拒绝。

`Dial` 的 ctx 控制整个会话，不只是握手。希望会话长期运行时传进程/服务生命周期 ctx；握手超时用 `HandshakeTimeout` 单独设置。`Close` 幂等，关闭连接、唤醒请求并等待内部 goroutine 退出。不要从 SDK 内部创建无法取消的后台 context。

## API 与责任

| API | 行为 |
| --- | --- |
| Dial / Welcome / Done / Err / Close | 会话协商、状态、取消和资源回收；Welcome 返回副本 |
| Events | 有界的服务端主动消息通道；关闭代表会话终止 |
| Call / Send | Envelope 级请求关联或单向发送，自动填 session、epoch、message ID 和 deadline |
| Message / CallMessage / SendMessage | 接收生成的 typed payload，使用 proto descriptor 构造信封，避免手写第二套消息目录 |
| Metadata | 显式设置稳定 operationId、subscriptionId 和资源定位 |
| Register / RenewLease / UpdateInstance / Deregister | 带 operationId 的注册和续租，返回具体回复类型 |
| Subscribe / Resume / Unsubscribe | 订阅和恢复；后续 Snapshot/Delta/Invalidated 等交给 Events 消费 |
| FetchResource / Download | 获取引用及同 origin、有限大小的 SignedResource 下载 |
| ReleaseStatus / Collect | 发布状态查询、带逐事件结果的事件采集 |
| NewVerifier / Verify | 复用原有 Ed25519 校验、scope/期限/摘要与高水位检查 |
| Run | 重新建连并再次调用会话回调，调用方恢复订阅和已应用游标 |

通过 `SendMessage(ctx, body, Metadata{OperationID: stableID})` 发送 UpdateInstance、Deregister、ResourceApplied/Rejected、Prepared、Applied、Rejected、ReadinessReport、DrainReport 等所有客户端消息；如服务端契约要求关联回包，使用 CallMessage。不得由客户端发送 Prepare/Activate/Revoke 等控制面命令。缺少必需 operationId 在写入前拒绝。

默认握手和请求期限各 10 秒，事件队列 32 项（上限 32，每帧最大 256 KiB），pending 调用 64（可设至 1024）；满 pending 等待 ctx，满事件队列明确断开并报告 ErrBackpressure，不静默丢配置/发布命令。发送没有无界后台队列，调用者直接承担背压。心跳使用独立 pending 名额，避免普通请求耗尽名额后停止心跳；共享 writer 的单次写入受请求期限限制。此客户端不实现方案中服务端的多优先级调度队列。

一个请求的消息会复制，调用期间调用者仍不得并发修改传入 protobuf。返回消息和事件的所有权交给调用者；Welcome 单独复制。不得依赖 Error 字符串解析故障，使用 errors.Is 或 errors.AsType 检查 ErrProtocol、ErrBackpressure、HandshakeError、RemoteError 等。

## 重连、续租和幂等

Run 使用 1 秒初始、2 倍递增、带抖动的退避；常规等待上界 30 秒，服务端 RetryAfter 可要求更长等待。认证 4xx（408/429 除外）、协议错误、背压或非 retryable 远端错误停止重试。503/429 等按 HTTP Retry-After 的秒数或日期等待。GoAway 按 retry_after_ms 恢复。

options 回调每次连接前执行，用来刷新 token 和已应用 cursors。serve 回调消费 Events，或监听 Done，终止后返回 client.Err() 表示会话故障；返回 nil 表示业务主动完成，Run 不再重连；应用自身错误为永久失败。serve 必须遵守 Done/ctx，SDK 不会强杀用户回调。

断线立即失败所有 pending 调用，不自动重放、自动重注册、自动续租或自动上报 Applied。注册/续租由实例生命周期 owner 按服务端期限安排，并显式使用 boot、leaseEpoch、连接 fencing；心跳仅证明链路。默认不自动恢复变更，可避免把“已执行但响应丢失”变为重复操作。重试时保留 operationId/eventId，查询实际结果，禁止把 MessageId 当业务幂等键。

## 配置下载与可信应用

Download 只接受与 Agent 同 origin 的 HTTPS 地址（显式 WS 测试对应 HTTP），拒绝跳转、userinfo、query 与 fragment。资源存储或 CDN 需要其他 origin 时，后续应新增独立且显式授权的下载适配器；首期不向第三方地址附加 Agent token。

ResourceRef.size_bytes 表示序列化 SignedResource 的完整 wire 字节长度；sha256 是这些 wire 字节的摘要。SignedResource claims 内的 payloadHash 则只覆盖资源 payload，二者不能混淆。下载上限是服务端 max_resource_bytes 与本地 64 MiB 的较小值；引用到期与请求超时均生效。服务端资源下载接口尚需按此编码约定实现。

Download 不代表验签成功。调用方必须继续以预置公钥运行 Verifier.Verify，并对照来自已授权引用的 scope/kind/key/revision/Bundle，完成 schema/依赖校验、整视图构造与缓存/高水位原子持久化。Verify 不写文件、不修改高水位，不隐式处理恢复授权。仅在业务成功生效后发送 ResourceApplied/Applied。SDK 不自动把策略保存到任意文件，也不冒充网关 RuntimeView 或发布执行器。

## 验证与边界

```sh
make test-agent-sdk
GOWORK=off go vet ./pkg/agent ./api/agent/v1
make check-agent-generated
make license-check
make release-check
```

生成代码复用已固定的 protoc 3.21.12 / protoc-gen-go v1.36.11；插件安装方式与生成脚本见 `scripts/generate_agent_proto.sh`。SDK tests 启动真实 TLS WebSocket 测试服务，覆盖并发、握手、心跳、断线、fencing、背压、取消、重定向防泄漏、typed API、资源下载及原签名 fixtures。此测试服务只验证协议客户端，不是生产服务端实现。

仍需服务端/网关阶段完成：事务幂等、共享租约 CAS、真实 Naming 底座、多副本、签名包下发、完整视图应用、发布容量门槛、实际 Prepared/Applied、业务曝光与 SSE/HTTP2 排空。没有真实服务端的端到端验收不能记作通过。

## 可信策略缓存与备份恢复

`InitializePolicyCache` 只供显式首次部署使用，`OpenPolicyCache` 用于后续启动。不要在打开失败时删除数据库或改用初始化：缺失、身份绑定错误、损坏和低于外部水位都需要调查。目录使用仅所有者可访问的权限；AuthenticationKey 为独立部署的 32 字节非零密钥，不保存在缓存目录。`Identity`、Scope 和资源 Key 属于封印绑定，不能复用其他实例的缓存。每个目录只允许一个 PolicyCache 所有者。

通过已鉴权通道取得完整 SignedResource 后调用 `Install`，再由 `Capture` 取得 PolicyView；请求捕获一次视图后调用其 `Evaluate`。没有可信策略、新鲜度过期、签名过期、已撤销或时钟异常都会返回错误，由业务显式处理，不隐式把默认值视作成功决策。缓存读取、重开、信任根更新和控制心跳不刷新新鲜度。`SetVerifier` 允许密钥轮换的重叠期；退役签名根会约束已捕获视图。控制面 bundle 撤销须先鉴权并校验 scope，再调用 `Revoke`；独立 PolicyCache 不自动消费 WSS；RuntimeManager 的签名投递入口见后节。

正常 `Install` 不能切换已有水位的控制面 epoch。恢复备份时，独立恢复授权机构先核对该实例保存的完整 Watermark 和拟恢复的签名策略，再授权一个更高 epoch。`RecoveryClaims` 精确绑定旧水位、目标策略的全部 claims、非零 32 字节 nonce、恢复 keyId 及至多 15 分钟授权窗口。控制面专用 `internal/agent.SignRecovery` 产生授权；普通 Agent 包只含验证逻辑。恢复根通过 `CacheOptions.RecoveryVerifier` 独立部署，与策略根不得共享公钥（包含不同 keyId 别名）。普通资源签名不能充当恢复签名。

`InstallRecovery(ctx, signedResource, grant)` 完成双重验签、目标完全匹配、旧水位 CAS、完整编译及撤销检查后，原子提交资源、新水位和授权摘要；不清空历史撤销。重复同一授权是幂等重试，不刷新新鲜度。执行后普通旧 epoch 更新仍被拒绝。恢复机构的管理审批、控制面 epoch 提升、授权分发和审计仍须由部署侧流程接入，目前没有完成这些管理端入口，不能仅凭该库 API 宣称整套备份恢复已经验收。

整个旧缓存备份被恢复时，目录内 HMAC 无法独立检测这种回退。可用 `Minimum` 传入目录外独立保存的可信水位；不得把水位和缓存一起恢复旧版本。进程退出后的 WAL 恢复已有自动测试；宿主机断电和存储设备持久性仍需部署环境验收。当前缓存运行验收覆盖 Linux，其他平台的权限与锁行为不从编译结果推导。

## App 和命令行启用入口

Agent 为实验功能，默认关闭。嵌入式应用显式设置 `grayling.Options.AgentEnabled`、`AgentAllowedHosts`、`AgentMaxConnections` 和 `PostgresDSN`。Host 是精确匹配的 listener Host（使用非默认端口时包括端口），不接受通配符。App 不拥有 HTTP listener，调用者必须在同一个 listener 终止 TLS；仅设置 `X-Forwarded-Proto` 不满足要求。没有 TLS 的请求会被通道拒绝。`App.Close` 会先停止 Agent 准入、关闭 WSS 并等待 handler，再关闭专用共享会话连接池。

命令行参数：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-agent` | false | 启用实验 Agent 通道 |
| `-agent-hosts` | 空 | 逗号分隔的精确 Host 白名单，不带空格 |
| `-agent-max-connections` | 0 | 必须显式指定，范围 1—100000 |
| `-tls-cert` | 空 | listener PEM 证书链 |
| `-tls-key` | 空 | listener PEM 私钥，与证书配套 |

CLI 启用 Agent 必须同时设置 `-pg` 和上述全部配置。证书在 App 打开存储之前加载；内置 HTTPS listener 最低使用 TLS 1.3。未设置 TLS 参数且未启用 Agent 时，原有 HTTP 启动方式不变。TLS 参数也可独立用于现有 API。Agent/listener 参数与离线备份、恢复验证及密钥轮换命令互斥，避免离线命令忽略入口配置。

这些参数接通传输和生命周期，尚不等同于完整发布能力：凭证和 scope 配额通过下节管理 API 配置，批准策略源接线仍待完成。启用通道不会自动创建机器凭证、授予权限或生成策略。现有 `-recovery-point` 尚未接入 Agent epoch 恢复授权，因此不能与 Agent 同时启用；这是待补齐的恢复门禁，不能通过清空水位或直接改库绕过。完整实例注册、Prepare/Activate 与 gogw 仍按实施方案继续建设。

## 机器凭证、配额和管理审计

Agent 启用后，workspace owner 可通过现有管理身份调用以下 TLS API；publisher、analyst、viewer、SDK/collect 及机器凭证不能调用。三项权限分别为 `agent.credential.manage`、`agent.quota.manage`、`agent.audit.read`。权限取自认证上下文和路径 workspace/environment；请求不能传入替代 scope 或 actor。开关关闭时不注册这些路径，也不向既有 capabilities 响应添加新权限。

基础路径：`/api/v1/admin/workspaces/{ws}/environments/{env}/agent/services/{service}`。ws/env 可使用 slug 或内部 ID，服务端发放的凭证仅绑定内部 ID。管理请求体上限 32 KiB，拒绝未知字段、重复字段和尾随 JSON。

| 方法与后缀 | 请求 / 返回 |
| --- | --- |
| `PUT /limits` | `operationId`、`expectedMaxConnections`、`maxConnections`；期望值 0 仅创建不存在的配额，其他情况按原值 CAS；最大值 1—100000 |
| `GET /limits` | 当前 maxConnections；尚未配置返回 404，不自动开放准入 |
| `POST /credentials` | operationId、principalId、agentId、instanceId、bootId、permissions、expiresAt；可选 takeoverFromBootId |
| `GET /credentials?after=...` | 元数据 items 和可选 nextCursor，每页最多 100；不包含密文或摘要 |
| `POST /credentials/{id}/revoke` | `operationId`；成功或同操作重试均为 204 |
| `GET /audit?before=...` | 当前 scope 的管理审计倒序页，最多 100；ID/nextCursor 使用十进制字符串 |

permissions 必须为非空、不重复的精确权限名集合：config.read、discovery.read、instance.register、instance.renew、release.report、events.write。发布激活不是机器权限。当前通道提供 session/config.read/instance.register/instance.renew 能力，并在配置签名发布器时提供 discovery.read，按凭证权限声明。instance.register 允许注册、更新就绪/排空状态与注销；instance.renew 独立控制续租。其余权限不会令未实现能力自动可用。expiresAt 使用 RFC 3339，必须晚于数据库当前时间且最多 24 小时。部署身份和 operationId 为非空 UTF-8 标识，不含控制字符或首尾空白，至多 128 字节。

所有变更要求稳定 operationId，去重键为完整 scope + 已认证管理 token ID + operationId。同一键改变载荷/动作返回 409；同一配额操作重试只返回原操作结果，不覆盖后来已更新的配额。过期操作 ID 拒绝复用，保留期至少 24 小时；后台清理与更长期审计策略仍待运维收尾。低于现有连接数量的配额只约束新准入，不因改配额隐式驱逐已有连接。

发放首次响应为 201，包含 credential、secret 和 replayed=false；提交成功之后才返回密文。相同请求重试为 200，返回同一个 credential、replayed=true，省略 secret。服务端只持密文摘要；密文不写入去重结果、审计或列表。如果首个响应丢失，通过同一 operationId 获取已发放的 ID，再按受控流程撤销该 ID、使用新 operationId 重新发放；不能要求服务端重放密文。重试的元数据表示原操作结果，当前撤销状态以列表为准。

凭证/配额变更、去重结果和 Agent 专用审计在同一 PostgreSQL 事务提交，审计不可写则业务变更回滚；读取审计使用上述 scope 路径。其他 scope 的凭证 ID 与不存在的 ID 均返回相同 404。正常连通时凭证撤销通过共享主库检查关闭 WSS；网络隔离时仍按既定有效期边界处理。连接在线仅证明通道可用，不证明资源已准备、视图已生效或业务已验证。管理控制台入口及四种状态的展示仍待实现。


注册通道已接入治理事务：`Register` 需要预置的登记策略及与精确实例描述绑定的部署授权；`RenewLease` 保持租约身份，`UpdateInstance` 只修改 ready/draining，`Deregister` 撤销资格。Welcome 公布租约 60 秒、建议每 20 秒续租。每次变更需要稳定 operationId，重连重试保留该值，修改载荷必须使用新值。命令执行预算最多 2 秒，并受信封中的更早 deadline 限制；超时后仍按同一 operationId 重试核实结果。

Registered/LeaseRenewed/InstanceChanged 是治理持久化响应，不表示 Naming 回读、Prepared 或网关可路由。策略与部署授权可由下述管理 API 预置；嵌入式 App 已自动运行 Naming 对账；外部 Nacos Naming 适配及签名发现通道尚待接入，不能宣称注册发现整体验收通过。


登记策略和部署授权沿用同一 scope 基路径与 `agent.credential.manage` 权限（仅 workspace owner）：

| 方法与后缀 | 用途 |
| --- | --- |
| `GET /registration-policy` | 读取登记策略及其 revision；未配置返回 404 |
| `PUT /registration-policy` | `{operationId, expectedRevision, policy}`，revision 必须为十进制字符串，首次 CAS 为 `"0"` |
| `POST /deployments` | `{operationId, principalId, agentId, policyRevision, expiresAt, instance}`，返回非 bearer 的部署授权引用 |

policy 包含 allowedNetworks（1–32 个规范 CIDR、禁止 `/0`）、forbiddenAddresses（最多 128 个精确 IP）、allowLoopback（默认 false）、maxInstances（1–100000）。修改策略会使原有登记失去资格，必须重新授权后注册。

instance 包含 instanceId、bootId、buildId、imageDigest（`sha256:` 加 64 位小写十六进制）、address（规范 IP 字面量）、port、tlsServerName（小写 DNS SNI）、zone、protocolVersion（1）。scope 来自管理路径；不得在 body 加入 scope、actor、lease、ready 或 draining。部署授权绑定 principal/agent 和精确描述、策略 revision，期限必须晚于数据库当前时间且最多 24 小时。机器注册时将返回的 id 放入 deployment_authorization，并继续使用独立机器凭证认证；该引用不授予其他机器身份访问能力。两个变更接口都以 operationId 去重，审计失败会回滚整个变更。部署授权成功及重试均返回 200。


嵌入式 App 的 Naming 对账按 scope 主键游标分页（每页 100），最多四个 scope、八次底座调用并发；周期处理持久意图，恢复已确认但因底座重建而丢失的投影。原生 Naming 仍只是投影，原始全量列表不能代替治理资格。对账日志按页聚合。外部 Nacos 模式尚无对应 Naming worker。

关闭顺序为取消应用任务、关闭 WSS、等待包括 Naming 在途回调的所有应用任务、关闭 Sessions 池、停止底座，最后关闭平台存储。App.Close 按调用者 context 返回；若底座回调不响应取消，后台 drain 会保留其依赖并等待，不会通过提前关闭依赖制造访问已关闭资源的竞态。该情况下资源释放和进程硬退出时限尚未证明，仍属验收缺口。


发现载荷组件现使用 `DiscoveryDocument` schemaVersion 1，purpose 固定为 `registered-candidates`。scope 与完整成员集合、membershipRevision 进入签名载荷；成员按 instanceId 字节序排序，instance/lease ID 不重复。membershipRevision 和 lease epoch、毫秒期限都以十进制 JSON 字符串编码。控制面编码器仅纳入 NamingConfirmed 成员，但该标记不授予 Prepared 或路由资格。

`VerifyDiscovery` 校验现有 GAR1 签名、scope、`bundle-<sha256>` 资源键及目标 Bundle、独立的 artifact revision 高水位、最长 60 秒期限、每个成员租约及地址/SNI/schema；返回的 Discovery 保持内部集合私有，Candidates 返回深拷贝。调用者须部署可信时钟；原子持久化可使用下述 DiscoveryCache，RuntimeView 完整资格组合仍待实现。artifact revision 必须与 membershipRevision 分离：同一成员集合续新鲜度时也须分配新不可变 artifact 身份，不能在同一身份下重签不同字节。持久版本分配和 WSS/HTTPS 发布已接通；发现可信缓存已有独立组件与下载集成测试，Prepared 组合尚未接入。未配置签名发布器时不声明 discovery.read。


内部 `Publisher.Discovery` 已使用 v5 PostgreSQL artifact 存储：在 credential/session、registration scope、schema epoch 与 artifact head 的同一事务屏障中捕获并保存签名。membershipRevision 与每个 scope/Bundle 的 artifact revision 分离；相同新鲜集合复用数据库中原始 envelope 字节，近过期、成员/epoch/密钥或有效期上限变化时分配新 artifact revision。历史 artifact 不 UPDATE。头和签名 INSERT 原子提交，失败不返回局部结果；64 位计数器耗尽返回冲突，不回绕。

签发时间来自数据库，数据库与本机偏差超过 5 秒或旧签发时间晚于当前数据库时间时失败关闭；提交前再次检查会话和 artifact 有效期。签名期限不超过 60 秒、成员租约及当前读取凭证期限。配置签名发布器后可使用发现网络传输，但仍不等同于 Prepared、路由授权或完整运行视图。artifact 历史的长期清理/容量治理仍待实现。


嵌入调用者可通过 Options.AgentSigningKeyID 和 Options.AgentSigningKey（Ed25519 私钥）配置发现签名；私钥在控制面克隆保存，客户端必须独立预置验证公钥。CLI 的文件配置见文末“签名密钥与独立信任根的 CLI 配置”。Channel 配置的 Publisher 必须使用相同 Sessions 实例，避免误接另一套权威存储。未配置发布器或未授予 discovery.read 的凭证都不会声明该能力。

使用 `Subscribe(ctx, id, &Cursor{Kind: RESOURCE_KIND_DISCOVERY, Key: DiscoveryKey(bundleHash)})` 建立发现订阅。每连接最多一个活动发现订阅，总订阅记录仍受 128 上限约束。首次同步、集合变化与签名刷新均发送完整 Snapshot；小资源内联，大资源通过同 origin HTTPS 引用。resource_revision 是 artifact revision，membershipRevision 保留在签名载荷中。重连建立新全量屏障，stream_seq 从 1 开始；同一新鲜 artifact 在不同副本返回相同字节。

FetchResource 支持 discovery kind 及精确 artifact revision；revision=0 读取/生成当前快照。固定版本下载使用 `/api/v1/agent/resources/2/{key}/{revision}`，只取该版本，不回退 latest；权限、会话 fencing、scope、epoch 和期限均重新校验。discovery.read 不依赖 config.read，不能用配置权限访问发现资源。未知与外 scope 的版本返回相同不可用错误。Download 仍须配合 VerifyDiscovery、可信时钟和后续原子缓存安装，不将传输成功视为就绪。


## 可信发现缓存

`InitializeDiscoveryCache` 用于显式首次部署，后续启动使用 `OpenDiscoveryCache`；选项嵌入 `CacheOptions`，另需非零 `BundleHash`。资源键由 Bundle 派生，缓存采用独立的 `discovery-cache.sqlite` 与封印域，不能替换策略缓存。身份、scope、Bundle 和目录外部署的 AuthenticationKey 共同约束缓存身份。安装下载的完整 SignedResource 后，调用 `Capture`，再用 `DiscoveryView.Candidates` 读取深拷贝候选集合；每次读取都会检查期限、撤销、信任根和时钟。

缓存将签名资源、artifact 水位及 payload 内的 membershipRevision 在同一 SQLite 事务中封印。相同 epoch 下，成员水位不能下降；成员水位相同时，成员内容必须相同。重复安装同一 artifact 不刷新本地新鲜度，旧捕获视图也不会因新资源安装而续期。MaxAge 默认 60 秒且不能超过 60 秒，成员租约和签名期限可能进一步缩短可用时间。过期返回 `ErrDiscoveryStale` 或相应期限错误，不把空集合或过期集合当作可路由结果。

`Minimum` 和 `MinimumMembershipRevision` 可携带目录外保存的可信下界；成员下界与 `Minimum.ControlPlaneEpoch` 配对。跨 epoch 恢复须通过独立恢复根签发的精确 GRA1 授权，kind 为 DISCOVERY，随后才允许成员水位在新 epoch 重新起算。整目录旧备份回退仍依赖这些外部下界检测。SQL 写入未实际更新行时缓存进入不可用状态，防止向调用者发布未持久化的新视图。

Linux 测试覆盖进程退出后的 WAL 恢复、密钥轮换、撤销、时钟回退、封印域隔离和真实 WSS/HTTPS 下载后重开。宿主机断电、自动资源应用驱动、完整 RuntimeView/Prepared 组合及网关路由资格仍待验收；DiscoveryView 仅证明已登记候选集合。


命令期限在服务端开始派发时固定：鉴权、注册和资源读取共享最长两秒预算；Envelope.deadlineUnixMs 可进一步缩短它，不能延长。已过期或超过当前时间 120 秒的期限被拒绝。期限错误返回 DEADLINE_EXCEEDED；命令超时本身不关闭仍有效的会话。Subscribe 仅在首个资源读取成功后建立订阅，失败不占用订阅 ID 或推进流序号，调用方须显式重试；其错误通过 subscriptionId 关联，包括鉴权等待超时。

## 发布 Bundle 的依赖校验

`MarshalBundle` 生成不可变 Bundle payload，完整字节的 SHA-256 用作 BundleHash。`VerifyBundle` 校验 kind=BUNDLE、派生资源键、内容身份、schema、build/资源顺序、协议、显式默认行为和授权硬截止。`Bundle.Document` 返回深拷贝；Bundle 不包含可选成员目录或发布代次。

`Bundle.VerifyPolicy` 在当前信任根下重新校验 Bundle，并验证精确 policy key、epoch、revision、payload 摘要及 BundleHash。routeFlag 必须是 string flag，valueBuilds 必须覆盖所有不同字符串输出且没有多余值。返回的 evaluator 仅代表依赖校验通过；缓存安装、Prepared、激活和实际处理链切换仍须完成。服务端已提供内部成对持久发布及固定版本下载；批准源管理入口与完整运行视图接线仍待完成。

控制面内部现在可用 `Publisher.PublishBundle` 原子持久化 Bundle/策略对；`CompileBundlePolicy` 供签名前检查完整依赖，不代表审批或生效。两份签名使用相同 epoch、数据库时间和有界截止；幂等重试返回原始字节。Bundle 固定版本交付和可信缓存已接通；管理发布入口以及 Prepare/Activate 尚未接通，SDK 调用方仍不能据此进入就绪或选路。


Bundle 下载使用 `FetchResource(kind=BUNDLE, key=BundleKey(hash), revision>0)` 获取引用，HTTPS 路径为 `/api/v1/agent/resources/3/{key}/{revision}`；不支持 revision=0 或 latest 回退。具有 config.read 的机器凭证协商到 bundle.read 能力，不要求该节点持有签名私钥或发布器。Subscribe 尚不支持 Bundle，后续 Prepare 将携带精确引用。

可用 `Client.DownloadBundle` 组合已鉴权下载、引用元数据和 Bundle 验签；它要求外部给定目标 BundleHash、可信时间和水位，并绑定 kind/key/revision/期限。ResourceRef 截止取签名资源与 reader 凭证截止的较小值；下载时仍重新检查凭证、scope、当前 session/connectionEpoch 和控制面 epoch。资源签名期限与 reader 凭证期限分别验证，不改写已签资源。跨节点重连保持原始 artifact 字节；外 scope 与不存在版本返回相同不可用响应。DownloadBundle 本身不持久化 Bundle 和水位；可将完整资源交给下述 BundleCache，仍不自动进入就绪或激活。


## 可信 Bundle 缓存

每个 BundleHash 使用独立部署目录，以 `InitializeBundleCache` 显式首次初始化，后续以 `OpenBundleCache` 启动。BundleCacheOptions 嵌入 CacheOptions 并要求非零 BundleHash；Key 可留空由 Hash 派生。缓存文件为 `bundle-cache.sqlite`，使用独立封印域；身份、scope、资源键与目录外 AuthenticationKey 共同约束缓存身份，文件锁保证单所有者。MaxAge 默认且最多为五分钟，同时受签名和 payload 授权硬截止约束。

通过鉴权下载取得完整 SignedResource 后调用 Install，再 Capture 取得 BundleView。Document 检查动态守卫后返回深拷贝，ValidatePolicy 检查 Bundle 与 PolicyView 两方的关闭、撤销、时钟、信任根和新鲜度，以及精确 scope/key/epoch/revision/payloadHash/BundleHash 和完整路由映射。完整 RuntimeView 仍须一次构造并原子切换，随后每次请求保持这些守卫；缓存依赖校验本身不授予 Prepared 或可选成员资格。

签名资源与水位在同一 SQLite 事务中封印。完全相同的 artifact 重放不刷新新鲜度；同一 artifact identity 的签发/到期时间、密钥或其他字节改变返回 ErrRollback；更高 artifact revision 才能更新本地验证时间。已捕获旧视图不会随新 artifact 安装续期。新鲜度过期返回 ErrBundleStale，Bundle 撤销目前复用 ErrPolicyRevoked。Revoke/SetVerifier 会约束已捕获视图；调用方仍须鉴权并接通控制消息。

Minimum 可传入目录外可信水位以检测整目录旧备份回退，不能与缓存一同回退。Bundle payload 包含 policy epoch，跨 epoch 必然产生新的 BundleHash；固定 Hash 缓存不提供 InstallRecovery，也不允许改写旧 Bundle。独立恢复流程须管理新 Bundle 与外部水位，完整控制面恢复接线仍待完成。控制面内部 RefreshBundle 可在原授权范围内分配更高 artifact；撤销门禁已接，自动周期调度与审批流程接线仍待实现。普通心跳或重复旧资源不会延长此缓存新鲜度。

Linux 测试覆盖重启、进程退出后 WAL 恢复、空缓存跨种类封印隔离、写入被抑制、密钥轮换/退役、撤销、时钟回退和真实 WSS/HTTPS 下载后的重开及策略依赖校验。宿主机断电、自动应用驱动、Prepared 与完整运行视图仍待验收。


控制面 `Publisher.RefreshBundle` 需要固定 scope/BundleHash、预期 artifact revision、principal/operation ID 和部署方可信 keyring。它验证仍有效、仍受信的原 artifact 1 和精确策略依赖，分配更高版本，保留相同 payload 和 BundleHash。新签名期限受 issuer lifetime、Bundle 硬截止及原 policy 签名期限共同限制；原策略过期后不能靠刷新 Bundle 续权。原 artifact 1 和策略依赖须保留至刷新授权结束及相关幂等/引用保留期结束。

该内部方法没有 Agent/HTTP 能力入口，也未接入定时任务；控制面调用方仍须先落实审批和撤销门禁。重放只返回原操作的精确字节，即使当前已过期或根已退役，也不会重新签发；必须继续经过正常下载与当前验签/缓存守卫，不能将重放返回成功当作当前可用性证明。

### Bundle 审批发布服务与 HTTP 接口

`admin.Service.RequestBundlePublication` 在现有环境发布锁内读取指定已发布
manifest 的完整快照，复验实际摘要和 Bundle 策略依赖，将确定性提案保存为
`agent.bundle.publish` 审批。配置草稿不会进入提案。审批者仍通过现有审批流程审阅。

`PublishApprovedBundle` 在同一发布锁协议下消费审批，使用审批 ID 摘要派生固定
principal 和 `publish` operation；首次消费必须匹配当前已发布 source revision。
回执重试保留原提案字节，允许恢复已经提交但响应丢失的签发结果。新签发在 Agent
事务中持有当前调用者、审批者的生产 PostgreSQL 凭证行共享锁，与撤销和角色变更排序，
并在事务提交前检查原审批截止时间。历史结果重放不会重新签发或延长有效期。

审批回执、签发和审计不是一个跨存储原子事务。消费回执后失败可重试同一个操作；
`agent.bundle.outcome_unknown` 表示调用未确认结果，不能据此判断事务未提交。
`agent.bundle.published` 记录已经确认的 Bundle 摘要和版本。审计写入失败会返回错误，
重试可能产生重复的审计事件。签发不产生发布 generation、Prepared、Applied 或激活路由。

App 同时启用 Agent 并配置签名密钥后，注册两个真实 TLS 管理接口，均要求当前
workspace 的 `flag.publish` 权限和 `Cache-Control: no-store`：

- `POST /api/v1/admin/workspaces/{ws}/environments/{env}/agent/services/{service}/bundles/approvals`：请求精确提案审批，返回 201；使用现有 workspace 审批接口批准。
- `POST /api/v1/admin/workspaces/{ws}/environments/{env}/agent/services/{service}/bundles/publications`：提交 `{"approvalId":"…"}`，返回 200 和固定 Bundle 摘要、key、artifact/policy revision、签名期限；同审批重试返回相同结果。

请求审批的 JSON 最多 32 KiB，包含 `sourceRevision`、字符串形式的
`expectedPolicyRevision`（首次必须显式填 `"0"`）、`policyKey`、`routeFlag`、
`valueBuilds`、`minimumProtocolVersion`、`defaultBehavior`、可选 `defaultBuild`、
字符串形式的 `validUntilUnixMs`、按 ID 排序的 `builds` 和 `reason`。
`valueBuilds` 必须完整映射路由开关实际字符串输出值；build 固定镜像摘要、协议版本和
排序后的资源摘要。服务端生成 scope、当前 epoch、下一 policy revision 和实际配置摘要，
拒绝客户端注入这些字段。签名实际有效期还受 issuer lifetime 限制。

未批准、过期审批、source 漂移或策略 CAS 冲突返回 409；权限和真实 TLS 不满足返回
403；未知或多余 JSON 字段返回 400。无法确认存储、事务或审计结果时返回 503；调用方
保留原 approvalId 重试，不能因 503 假定签发没有提交。

审批请求若已创建、随后请求审计失败，503 响应包含 `approvalId`，应先查询并核对该审批；
若审批创建本身结果未知，先查询已有审批再决定是否重新提交提案。签发后的审计失败也优先
返回 503，不能当作普通 CAS 冲突处理。
未配置签名密钥或 Agent 关闭时，上述路由不存在。控制台操作页和发布状态机仍待接入。

### 发布代次持久化核心（内部能力）

`Publisher.StartRelease` 使用独立于 Bundle artifact/policy revision 的 scope generation。
请求固定精确 BundleHash、正 artifact revision、预期当前 generation 和完整容量规则，
并传入控制面可信验签 keyring。创建前重新验证 Bundle 及其精确 policy；不跟随 latest。
同一事务分配 generation、保存不可变定义、PREPARING 状态、状态事件、精确资源引用和
24 小时操作回执。保留的 release 通过外键固定 policy 和 Bundle 的具体版本，GC 不能
直接删除这些依赖；引用退役、保留上限和清理编排仍待实现。

容量规则必须显式选择 `production-v1` 或 `demo-v1`，提供排序且唯一的非空
`requiredZones`。每个 Bundle 承载 build 在每个要求 AZ 的最低准备数，生产至少为 2，
独立 demo profile 至少为 1；`maximumFailureBasisPoints` 为 0–10000，0 表示不容许
准备失败。这些规则被完整保存和去重摘要绑定，显式容量判定见下节。
Prepare 期限上限 120 秒，并裁剪到精确 Bundle/policy 签名及 Bundle 硬期限；定义记录
Prepared TTL 上限 300 秒，内部证据处理见下节。

新发布遇到仍有效的 PREPARING/PREPARED 前代时拒绝分配。仅当准备期限已过或 epoch
被替代，并且新 Bundle/授权校验全部通过时，才在同一事务将该前代标记 FAILED 并创建
更高 generation。失败的创建也回滚前代状态变更。ACTIVATING/VERIFYING 前代会阻止
新发布，不推断它们已经完成。ACTIVE 前代可以启动更高代发布，但不会因此被静默撤销。

`Sessions.AbortRelease` 要求当前 generation 和状态 revision，并检查当前控制面 epoch；
仅允许 PREPARING/PREPARED → ABORTED。ACTIVATING 或更后阶段不能用 Abort 倒退，
恢复必须走更高代的正式发布/回滚流程。状态、事件和回执同事务提交。过期审批不能产生
新的变更；历史回执重放可以返回原结果，既不延长期限，也不证明当前授权可用。

创建回执始终保留最初 PREPARING 结果，即使该发布后来已 Abort 或已有更高代；查询
当前状态须用 `Sessions.Release`，调度器还必须在发送命令的决策点检查当前 head、epoch、
状态与期限，不能依据创建回执重新发送已终止的 Prepare。operation 保留过期后仍受 durable
head/revision CAS 约束，不能重新分配旧代次。uint64 generation 采用精确十进制存储和
JSON 字符串编码，达到上限后拒绝再递增。

这些方法当前仅供可信控制面内部使用，尚未将容量意图绑定至外部审批入口，也没有发布
发布 HTTP 能力、Applied 回报、激活、探测或自动超时任务；Prepare 传输见下节。
没有这些证据时只创建 PREPARING，不能据此宣称发布成功或业务已切换。数据库组件版本
升级为 v6；旧二进制不能直接打开该版本，生产升级/回退演练仍待验收。


### 定向 Prepare 与 Prepared 证据（内部能力）

数据库 v7 增加持久定向意图、每实例目标 revision 和不可变 Prepared 回执。
`QueuePrepare` 要求当前发布 generation、目标 revision CAS、有效实例租约、当前注册策略、
已确认 Naming、Ready 且非 Draining，以及精确 Bundle 中匹配的 build/image/protocol。
意图绑定 credential、principal、agent、instance、boot 和 lease ID/epoch；期限裁剪至
Prepare 截止时间、租约和凭据期限。重复 operation 返回原意图，不刷新期限；该历史返回值
不能作为重新投递的授权。

`PendingPrepare` 在当前会话、当前目标和可信资源根下重新检查上述条件，返回尚未回报的
定向快照。已接受的命令不再投递；证据到期后需要显式创建更高目标 revision 的新命令。
快照本身不证明网络送达，接收回报时仍须重新检查。

`RecordPrepared` 要求真实会话具备 config.read 和 release.report，回报关联当前 command ID，
精确匹配 release/hash/generation、instance/boot/build/protocol。旧连接、租约轮换、目标替换、
Draining、过期意图和过期签名资源均不能继续贡献证据。客户端时间只在受限窗口内接受；
服务端正向证据 TTL 受 300 秒、客户端声明、凭据和资源期限约束；命令期限用于首次接收，
实时资格另检查当前租约。证据 TTL 不等于路由授权期限，签名激活成员仍裁剪到当前租约。
计数 revision 与证据同事务提交；并发重放返回完全相同回执，不延长 receivedAt/expiresAt。

内部持久化和 WSS/SDK 传输均有真实 PostgreSQL 组件测试；传输约定见下节。
证据存在不等于当前可用容量。回报本身保持发布状态，只有下述显式容量决策可晋级
PREPARED；仍不宣称 ACTIVE。
数据库升级到 v7 的旧版本兼容迁移有组件覆盖，生产升级/回退演练仍待完成。


### Prepare WSS 投递与持久化确认

嵌入 App 的 `Options.AgentReleaseVerifier` 或直接创建 Channel 的
`ChannelOptions.ReleaseVerifier` 必须提供独立可信 keyring；不从当前签名私钥隐式建立信任。
未配置时不提供 `release.prepare`。凭据还必须同时具备 config.read 和 release.report。
客户端须在 Hello 的 capabilities 或 requiredCapabilities 显式声明 `release.prepare`，
否则不会收到 Prepare，Prepared 回报也被拒绝。

每条连接通过共享数据库每 2 秒检查当前定向意图。Prepare 的 operationId 是持久 command ID，
正文固定 release ID/generation/BundleHash、精确 artifact 的 HTTPS ResourceRef 与意图期限。
同一连接每命令发送一次；断线后其他副本会重新校验实时资格并恢复未确认命令。网络重投
不会刷新意图期限。客户端须下载验签、检查自身 build/boot 和实际健康后自行形成 Prepared；
SDK 不自动声称已完成准备。

`Client.ReportPrepared(ctx, prepare.OperationId, report)` 返回新增的 oneof 字段 69
`PreparedAck`，包含相同 release、command ID、持久 evidence revision、首次 receivedUnixMs
和裁剪后的 expiresUnixMs。该消息只证明证据已落库，不表示当前可选、容量达标或 ACTIVE。
写出后未获响应可能结果未知，须重试同一 command ID 和完全相同的回报；改变内容返回冲突。
已接受命令不再重投。旧会话、旧租约或旧目标的历史回报仍被实时校验拒绝。

Prepared 命令与鉴权共享 2 秒处理预算，受更短信封 deadline 裁剪。SDK 保留调用方显式
信封 deadline 与调用 context deadline 中较早值，不能因默认请求超时延长命令授权。
当前尚无外部批准容量意图到发布的编排或实际 RuntimeView/探测闭环；本节
传输与持久化确认不能用于宣布发布成功。


### NACK 与准备容量决策

数据库 v8 增加负向证据与不可变准备决策。客户端除 release.prepare 外须显式声明
`release.prepare.reject`，才能调用 `ReportPrepareRejected` 并收到字段 74
`PrepareRejectedAck`。旧客户端没有该能力时收到标准 UNSUPPORTED_MESSAGE，避免发送
它无法解析的新确认类型。NACK 绑定同一当前定向 command，仍检查真实会话、身份、租约、
发布与资源；Ready=false 或 Draining 不能使负向回报消失。每个命令只能保存 Prepared 或
Rejected 中的一种终态，重放保持原 revision/时间，改变内容或反向替换返回冲突。
恢复须显式创建更高 target revision；旧命令的结果不计入当前目标。

可信控制面内部的 `EvaluatePreparation` 要求 generation/状态 revision CAS、操作 ID、
可信验签器，以及可选的事务内授权/授权期限。它按凭据排序加共享锁，再锁 admission、
registration scope、release head 和 schema；admission 屏障阻止判断期间的 boot 接管，
scope 屏障阻止租约/策略/Naming 与目标变更。预取后出现未锁凭据的新目标时整事务返回
可重试冲突，不在持 scope 锁时补锁凭据。目标数量最多读取 100000，超界明确拒绝决策。

当前每个 instance 的 target head 只计一次。成功必须具备仍有效的 Prepared、credential、
boot/lease、注册策略、Ready/非 Draining 与 Naming 确认。NACK、身份失效、过期证据和
过期未回报目标计失败；仍有效但未回报的目标计 pending。对每个 Bundle 承载 build 与
required AZ 单独检查最低准备数，其他格子的容量不能替代。

失败率采用保守上界 `(failed + pending) / targets`，与 maximumFailureBasisPoints
用整数交叉相乘比较，等于上限允许通过。pending 单列展示但不能稀释已知失败；例如
1 成功、1 待定时上界为 50%，0% 默认门槛拒绝，显式 50% 门槛仍须满足全部 build/AZ。
无目标永远不能通过。

满足门槛后同事务保存 PREPARED 状态 revision、事件、证据 catalog revision、目标摘要、
容量矩阵及决策期限。期限裁剪至所有成功证据及关联授权期限，提交前再次检查数据库
时间、时钟偏差和有效期；写入失败或审批过期回滚整个晋级。未满足门槛不缓存操作结果，
同一操作重试可以观察后续回报。已完成操作返回原历史观察，不刷新 TTL；Prepared 状态
本身也不是持续容量保证。后续激活必须在自身原子决策点完整重算，不能把历史 Eligible
或 PREPARED 当作激活授权。Applied 传输和持久化见下节；本阶段仍没有 ACTIVE 或业务探测闭环。


### 激活决策与签名准备快照（内部控制面能力）

`Publisher.StartActivation` 要求当前 generation/状态 revision CAS、PREPARED 状态、
独立可信验签器及非空、排序、去重的 requiredSourceIDs。来源集合必须由控制面审批意图
明确提供，不能按收到的回报临时缩小。每个来源必须属于本次仍有效的 Prepared 成员，
并具备 config.read、discovery.read、release.report。外部审批到该入口的编排仍待接入。

激活事务采用与容量决策相同的凭据/admission/scope/release 屏障，重新验签精确
Bundle/policy，并重算逐 build/AZ 容量和失败率上界。历史 PREPARED、历史 Eligible 或已过期
证据都不能替代本次检查。通过后冻结当前有效 Prepared 成员、身份/地址/SNI/build/image、
boot/lease、准备 command/revision/期限，以及每个必需来源的稳定 activate command ID。

资源类型 5 `RESOURCE_KIND_ACTIVATION` 承载 schema 1、purpose activation-view 的规范 JSON。
claims 固定 scope、control-plane epoch、BundleHash、activation-generation 公共键和 artifact
revision；正文固定 release generation、精确 Bundle artifact/policy revision、当前权威成员
revision、Prepared catalog revision 和完整成员/来源集合。DiscoveryRevision 在此指成员目录
revision，和独立 discovery artifact revision 有区别。RevocationEpoch 来自同事务锁定的累计撤销目录（无目录时为 0）；
累计撤销状态与 WSS 传播见后节；签名激活视图的持续刷新仍待实现。

同一事务保存 ACTIVATING 状态 revision、事件、签名资源及 private head、精确资源引用、
activation 行、来源命令和操作回执。资源上限 60 秒，并裁剪至签发器 lifetime、所有使用到
的准备证据/租约/凭据/Bundle/policy 和授权期限；提交前复查数据库时间未回拨且未到期。
任意写入失败回滚整个激活意图。重复 operation 返回原历史回执，不重签、不刷新期限，
也不证明已投递或已 Applied。ACTIVATING 后不能用 Abort 静默倒退。

配置 ReleaseVerifier 且具备上述权限时，欢迎消息提供 release.activation.read。
只支持精确 FetchResource(kind=5,key=activation-G,revision>0) 与同源 HTTPS 下载，没有 latest。
每次下载重新检查当前发布代次/epoch、指定源 credential、当前会话/boot/lease、注册策略、
Naming、Prepare head 和签名资源。其他实例即使知道键也不能下载该源授权。
SDK `DownloadActivation` 还检查本客户端 instance/boot 位于 sources；`VerifyActivation`
检查规范编码、版本、成员证据期限和来源绑定，`ActivationView.VerifyBundle` 对接精确
Bundle 及 build/image/protocol 约束。返回值的成员与来源集合均为独立副本。

快照只保证有界签名授权，不保证冻结后所有成员仍然在线；持续发现/撤销及接收端
boot/mTLS 校验仍须由 RuntimeView 完成。Activate/Applied 传输与 VERIFYING 记录见下节；SDK 实际安装见后节；仍无 gogw 接入、ACTIVE 或探测闭环，下载不会自动发送生效回报。该激活快照组件引入 v9，
真实迁移与事务测试不代替生产升级/回退和完整 A16 验收。


### Activate 投递、源回报与诊断状态

新增能力 `release.activate`，仅在独立 ReleaseVerifier 和 config.read/discovery.read/
release.report 权限齐备时提供，客户端必须在 Hello 显式声明才会收到 Activate。
服务端每 2 秒从共享数据库读取当前未回报的源命令；同连接每命令发送一次，跨副本重连
恢复同一 operationId，期限不更新。投递点重新检查当前 generation/epoch、源身份、
boot/lease、注册/Naming 和签名快照。任一种终态回报持久化后不再重投。

Activate 在原 release/policy/prepared 字段之外提供 kind5 view 引用、discovery revision、
revocation epoch 和 expiresUnixMs。源端须校验快照、下载全部精确依赖并实际安装完整
RuntimeView，才可调用 `ReportApplied`。Applied 新增 activationArtifactRevision 和
controlPlaneEpoch，和实际 policy/discovery/prepared/revocation 版本一起逐项核对，不能仅
回显 release ID 或声明收到消息。SDK 下载本身仍不触发安装或自动回报。

安装失败可调用 `ReportActivationRejected`，正文必须含当前 instanceId、bootId、精确 activationArtifactRevision 和明确错误。
Prepare 拒绝不使用该版本字段；激活拒绝缺少版本在写入前拒绝，SDK 在发送前拒绝。
负回报允许 Ready=false 或 Draining，不要求 Naming 仍确认，但仍检查当前凭据、会话、
boot/lease、注册策略和签名期限。正回报继续要求当前 Ready/非 Draining 与 Naming 确认。
同一源命令只允许一个不可变 Applied/Rejected 结果；改变类型或内容返回冲突。

字段 75 `ActivationReportAck` 返回持久 reportRevision、首次 receivedUnixMs、结果类型和
当时 VERIFYING/releaseRevision。v10 中回报目录独立于 Prepared catalog；每个新源回报
同事务推进两种 revision，保存 VERIFYING 状态事件、完整回报和 ACK，VERIFYING→VERIFYING
也保留事件。重复回报返回原 ACK，不新增事件、不延长授权。即使所有源都 Applied，也不
自动进入 ACTIVE；部分成功或 NACK 保持明确 VERIFYING，等待后续探测/恢复策略。

写出后未获 ACK 可以在授权仍有效时重试完全相同回报；旧连接、旧 boot、版本不符、
已过期视图均不能产生新确认。回放也要求当前身份与视图仍有效，过期后应查询历史状态，
而不是把旧 ACK 当成新的路由授权。报告、ACK、目录或状态事件任一写入失败都整体回滚；
鉴权和领域操作共享 2 秒预算，并受更短信封 deadline 限制。

`Client.ReleaseStatus` 当前支持精确 releaseId，不支持 operationId 别名。只有该发布的
已授权源可查询；结果是诊断历史，不检查视图仍新鲜，也不表示当前可选或业务成功。
查询按 instance ID 分页，pageSize 默认 64、最多 128，返回 Applied、Rejected 和
pendingSourceIds，以及 requiredSources、reportRevision、releaseRevision 和 nextInstanceId。
继续分页必须传 afterInstanceId 及首屏的 expectedReportRevision、expectedReleaseRevision；
期间回报或状态变化返回 RESYNC_REQUIRED，客户端应从首屏重新取，避免混合快照。
状态页只返回有界已知字段，未知可选字段保留用于原回报精确重放，不占用状态页预算。

真实 PostgreSQL/WSS 测试覆盖跨副本投递/回放、精确版本、正负竞争、Ready=false 的失败
回报、部分成功、分页重同步、短信封 deadline、写入抑制回滚、v9→v10 保留激活意图、
证据过期后保留诊断事实以及完整 uint64 计数上限。SDK RuntimeView 见后节；仍待实际 gogw 接入、签名视图
持续刷新、撤销投递见后节；VERIFYING 超时/恢复、业务探测和 ACTIVE 门槛，不能据组件测试宣称
完整 A16 或实际网关生效验收通过。

### SDK RuntimeView 的实际安装与封存记录

`InitializeRuntime`/`OpenRuntime` 使用独立的 `runtime-cache.sqlite`。配置固定 scope、
policyKey、instance/boot/build/image、独立 MAC 密钥、签名根、可信时钟和应用 Builder。
`Install` 同时验证激活视图、精确 Bundle 和策略，并检查源身份、配置的 build/image、
全部远端连接池，以及本地资源字节的 SHA256。Builder 必须交付该视图独占的 handler、
transports 和 Close；handler 如何使用这些资源仍由应用适配器负责。

构建在提交锁外执行，提交前重新验证当前授权和全部独立高水位，慢旧构建不能覆盖
较新的发布。签名输入和高水位以同一 MAC 封存行原子持久化，提交后再次检查期限，
然后一次替换活动指针并生成 Applied。普通取消不毒化健康视图，完整性损坏或未知提交
结果使当前管理器停止提供授权。`Install` 本身不发送网络回报；可使用下述 `Client.InstallActivation` 编排回报。安装失败
不会自动产生不可撤销的负回报。

重启只打开持久记录，不恢复就绪或生成 Applied。`Restore` 必须重新构建本进程实际
handler/连接池并完成切换；旧 boot、过期签名、退休根和低版本不能借恢复获得授权。
重放和重建都保留首次验签时间。外置 `Minimum` 用于抵抗整个数据库文件回放；目录
中的 MAC 本身不能证明文件没有被整体回滚。跨 epoch 恢复目前拒绝，等待独立授权流程。

每个请求先 `Capture` 并在结束时 `Release`；该 lease 的求值、端点和 handler 都来自
同一个视图。`Select` 先固定 build，再在该 build 的已准备成员间均衡；无容量时返回
错误，不切换到其他 build。正常更新允许旧捕获排空，最后一个引用释放后才关闭旧资源。
请求截止时间受签名授权和首次验签最大年龄限制；响应体 EOF/Close 或取消释放流引用。
`Close(ctx)` 可按调用方期限返回，后台继续持有数据库和资源所有权，直到实际使用者退出。

可信本地 `Revoke` 先持久化累计拒绝集合，再覆盖已有捕获，可取消在途 handler/响应流。
此入口需要上层认证控制面命令；WSS 撤销见后节；实际 gogw/mTLS/单次转发适配器、签名视图
持续刷新、恢复授权、业务探测和 ACTIVE 门槛仍待实现。SQLite 崩溃测试验证进程直接退出
后的已提交 WAL 恢复，不代表硬件断电耐久性或完整 A16/A17 验收通过。


`Client.InstallActivation(ctx, event, runtime)` 接收调用方从 `Events()` 选择的 Activate，
核对信封期限、源 instance/boot/command、generation/Bundle 和全部版本。它先下载并验签
激活快照，再 Fetch/下载其中锁定的 Bundle artifact 和 policy revision；不会查询最新策略
来补齐依赖。管理器完整构建并安装后才调用 ReportApplied，服务端 ACK 仍只表示 VERIFYING。

返回的 `RuntimeActivationResult` 区分 `Applied` 和 `Receipt`。即使方法返回错误，非空
Applied 仍表示已发生实际安装；例如连接在构建期间断开，安装完成但报告无法发送。
调用方应保留原 commandId 和精确 Applied，在授权仍有效时重连回放，或重复处理原 Activate；
相同输入不会重新构建资源或延长首次验签时间。超过期限只能查询历史状态，不能把历史
回执用于恢复路由。方法不会因下载、构建或回报错误而自动发送 Rejected，也不会回滚已经
安装的进程资源。应用仍需自行调度事件处理、就绪与排空，并接入真正的网关业务处理链。

真实 PostgreSQL/WSS 测试已覆盖上述安装与回报链路、构建失败保持 pending、已安装后
连接丢失的恢复，并验证策略 head 前进后仍按激活快照使用旧的精确依赖。SDK 另验证同策略
发现版本可独立推进，高 generation 不能重置发现高水位。这些组件证据不代表 gogw、
mTLS/最终票据、ACTIVE 探测、自动刷新或完整发布验收完成。


### 累计撤销目录、WSS 投递与证明刷新

`release.revoke` 需独立 ReleaseVerifier 和 config.read，并在 Hello 显式声明。服务端每两秒
从共享 PostgreSQL 读取当前 scope 的 kind4 累计目录，以 `revoke-<epoch>-<artifactRevision>` 稳定命令标识投递；
同连接同 artifactRevision 发送一次，重连重新投递完整目录。未成功应用时调用方可主动重新 Fetch
当前资源或重连。新增 Revoke.view 为精确 HTTPS 引用；帧里的 hash/取消摘要不能代替验签。
`Client.ApplyRevocation` 验证 scope、epoch、期限、签名及帧元数据后，调用 RuntimeManager
持久化完整拒绝集合与逐 Bundle 的取消集合。目录和封存记录都拒绝缩减或取消降级。

签名目录的 `cancelledHashes` 为取消活动流的精确子集，帧的 cancelActiveStreams 只表示
该子集非空。SDK 不会因后来撤销其他 Bundle 而取消先前被允许排空的流。提交后的拒绝
覆盖旧捕获；取消也覆盖旧 handler 和响应体。投递证明过期不会清除已经接受的拒绝。
`RevocationFloor()` 返回独立撤销高水位，应保存在缓存目录之外，并在重开时传入
`RuntimeOptions.MinimumRevocation`，防止整体回放数据库而丢失最近的拒绝。空初始化在
恢复该高水位前不能安装路由授权。跨控制面 epoch 清理/恢复仍需后续独立恢复协议。

控制面 `Publisher.RevokeBundles` 是 owner 管理动作，按 expectedEpoch CAS 推进目录，
保留已有拒绝和取消标记，最多 128 个 Bundle；达到上限明确拒绝，不静默移除旧项。
目录 head、不可变签名资源、幂等结果和审计同事务提交。首次空目录用永久零 head 串行化，
读取和新授权持共享锁，撤销持独占锁，避免不存在目录时无锁放行。当前数据库为 v11；
复用私有资源键 `revocation/state`，普通策略不能覆盖它。

新 policy/Bundle 签发、Bundle 刷新、Prepare/Prepared、容量判断、激活及 Applied、策略/
Bundle/发现资源读取都检查撤销。激活视图携带当前撤销 epoch；目录变化后旧激活视图需
重新同步，不能继续产生新的生效确认。`Client.InstallActivation` 会先恢复视图锁定的
非零撤销目录，再安装正向资源，因此冷启动不依赖应用先处理主动通知。

启用签名密钥与独立 ReleaseVerifier 时，新增两个真实 TLS 的 owner 管理 API：

- `POST .../agent/services/{service}/revocations`：operationId、expectedEpoch（uint64 字符串）、bundleHashes、reason、可选 cancelActiveStreams。
- `POST .../agent/services/{service}/revocations/refresh`：operationId、expectedEpoch。

权限为 `agent.revocation.manage`，新操作在签发事务内锁定并重新检查生产管理员令牌行。
请求最多 32 KiB，未知字段拒绝，响应 no-store。相同 operationId 返回原始结果，历史回执
不代表当前投递授权。`RefreshRevocation`/refresh API 仅续签累计拒绝的投递证明并推进 artifactRevision，保持 payload 和撤销 epoch 不变，
不会清除拒绝或延长 policy、Bundle、lease、Prepared、激活视图等正向授权；到期后仍可按
保留的可信签名恢复投递证明。根轮换顺序、目录保留扩容、网关实际取消、
网络隔离和规模化五秒收敛验收仍待完成。当前测试只证明组件链路，不能作为完整 A22/A26。

证明 artifactRevision 与撤销内容 epoch 独立；回执同时返回两者。管理请求可附
expectedArtifactRevision（uint64 字符串）约束精确证明版本。SDK 持久化证明版本及精确
签名字节摘要，独立 MinimumRevocation 可检测同一内容 epoch 下的旧证明整库回放。
仍有效的历史证明仅在 payload 与当前目录完全一致时允许精确下载，续签不会破坏已取得
的下载引用；撤销内容变化后旧证明不再可下载。

App 的 AgentRevocationRefresh 默认关闭；显式启用要求 AgentEnabled、签名密钥和独立
ReleaseVerifier。后台在剩余寿命三分之一时续签，用固定系统主体审计；每页最多 100 个
scope、最多四个任务、整页两秒预算，并按已开始的前缀推进游标，避免前部锁阻塞长期
饿死后部 scope。双副本使用稳定操作号与内容/证明双 CAS 去重。停机取消并等待工作器后
才关闭数据库。它只维持拒绝证明投递，不续租任何正向授权；CLI 配置已接线，见文末；自动根轮换仍待完成。


### 激活 artifact 与报告版本化（数据库 v11）

agent_activations 保存当前 artifact 指针，agent_activation_versions 保留各版精确资源和
发布事件引用。源命令与报告的身份都包含完整 scope、generation、artifactRevision；
命令在 scope 内唯一，报告复合外键同时绑定版本、实例和命令。v10 升级复制原报告、
摘要和 ACK 字节，不能重新编码历史回执。报告 catalog 保持 generation 内全局单调，
状态集合仅包含当前 artifact。

PendingActivation 只投递当前版本尚未回报的命令，并在取得发布屏障后重新核对当前指针。
旧版成功不会抑制新版命令；新回报只能写入当前版本，仍有效的历史命令可重放已存在的
精确回执。精确资源下载使用对应版本的源身份，不替换为当前资源。

ReleaseStatus 新增 activationArtifactRevision；第二页及以后必须同时带
expectedActivationArtifactRevision、expectedReportRevision 和 expectedReleaseRevision，
任一变化要求重新全量分页。SDK 拒绝混合不同激活版本的状态响应。

旧 v10 激活 Rejected 的版本字段为零，只允许与已经保存的原报告完全匹配的重放；
不能新增零版本结果。新 SDK 使用 ReplayLegacyActivationRejected 恢复这一历史回执，
新的失败回报使用带精确版本的 ReportActivationRejected。

此前版本隔离测试中的第二版使用不延长期限的签名 fixture；正式续签入口与边界见下节。
Prepared 重新准备见后节；后台续签、探测及 ACTIVE 门槛尚待实现。


### 重新核验后的激活续签

部署侧可信调用方可用 `Publisher.RefreshActivation(ctx, StartActivationRequest,
expectedArtifactRevision, verifier)` 签发下一版。必须同时提供当前 release revision 和
artifact revision，以及与初版冻结集合完全相同的 RequiredSourceIDs。刷新使用独立的
operation 摘要域，双副本重放返回原 artifact 和回执，不重签或延长期限。

初版及当前激活证明均按原签发时间验签，用于验证冻结源集合；旧证明到期不自动阻止
重新授权，但绝不能替代当前凭证、租约、Prepared、容量、策略、Bundle、可信根和撤销
检查。来源集合与初版签名及持久化记录交叉核对，不允许借续签增删必需源。当前来源的
boot/lease 来自本次重新验证的 Prepared，不能凭旧源身份跳过验证。

新签名期限仍受当前所有有效证据、固定资源依赖、审批期限、issuer lifetime 和 60 秒上限
共同约束。新资源、版本、来源命令、保留引用、双 head、发布事件和幂等结果同事务提交。
提交前再次检查数据库时钟和期限。任一写入被抑制、根不受信、证据失效或批准失败均保持
旧 head。刷新回到 ACTIVATING；旧 Applied 留作历史，不能证明新版已安装。WSS 会发送
新命令，SDK 重新安装完整 RuntimeView 后才发送对应版本 Applied，并进入 VERIFYING。

真实 PostgreSQL/WSS/HTTPS 测试已覆盖正式续签和实际处理器切换。此入口本身不刷新
Prepared 证据、租约或固定 policy/Bundle，不提供自动调度。初始 QueuePrepare 保留准备阶段约束；激活阶段可通过后述 QueueReprepare 明确重新准备，
自动调度仍待接通，不能声称已完成长期持续授权。
管理审批/API/UI、探测/ACTIVE、实际 gogw 和完整验收继续待完成。


### Prepare 命令期限、Prepared 证据与重新准备

Prepare 命令最长 120 秒；正向 Prepared 证据最长 300 秒。第一次接收必须发生在命令期限
内，并要求当时租约/会话仍有效。正证据可晚于命令及首次租约截止时间到期，但重复回报
只返回首次回执，不重新计算 TTL。命令过期后禁止首次回报；读取正证据须确认原接收时间
在命令期限前，报告时间跨度/身份/摘要/资源上限仍合法。负证据维持原命令期限约束。

容量与激活每次重查当前 lease ID/epoch、boot、credential、Ready/Draining 和 Naming 对账。
同一 lease 续租后必须完成 Naming 对账才能恢复资格；续租不延长 Prepared TTL。更换
lease 或 boot 不能继承旧证据。激活成员有效期和签名期限均裁到当前租约，不把 300 秒
证据直接当作 300 秒远端授权。初始 PREPARING/PREPARED 仍受原 PrepareDeadline 限制；
ACTIVATING/VERIFYING/ACTIVE 的新授权按当前证据、租约和固定资源期限重新计算。

`Sessions.QueueReprepare` 是部署侧可信控制面动作，接收 QueuePrepareRequest，采用独立
操作摘要域和原 target revision CAS，仅允许上述激活阶段。每次新操作产生新的定向命令，
期限最多从本次时间起 120 秒，并受 credential、固定资源和可选 AuthorizationExpiresAt
约束。期限存在性单独进入操作摘要，Unix 零时刻不能混同未设置期限。旧 operation 只返回
历史意图；不能用重试获取新期限。QueuePrepare 的原始无期限请求编码保持兼容。

重新准备会推进当前 target head；旧证据保留历史，但不能在新目标 pending/NACK 时继续
用于新激活。旧激活的固定下载/回报也仍受当前 target fencing 约束；更换目标后不能把
历史回执当作当前授权，状态接口保留诊断事实。新命令可通过现有 WSS Prepare/Prepared/NACK 通道投递回报，NACK 不能在
同一命令下改成成功。明确创建下一 target 后才可重新尝试；Fresh Prepared 后还必须调用
RefreshActivation 并重新安装/Applied，不能仅凭 Prepared 把发布推到 ACTIVE。

真实 PostgreSQL/WSS 测试覆盖初始窗口与命令到期后的证据使用、期限不滑动、首次过期
回报拒绝、并发替换去重、审批过期回滚、NACK→新命令→正证据→新版激活。测试用缩短的
初始 fixture 窗口验证真实时间跨越，未替代默认 120/300 秒的长期运行或规模验收。自动调度、
管理审批/API/UI、实际 agent 资源准备驱动、探测/ACTIVE 和真实 gogw 仍待完成。


### 签名密钥与独立信任根的 CLI 配置

Agent 仍默认关闭。已有 `-agent`、`-pg`、`-tls-cert`、`-tls-key`、`-agent-hosts` 和
`-agent-max-connections` 配置上，增加：

- `-agent-signing-key-id`：显式 key ID，1—128 个 ASCII 字母、数字、点、横线或下划线。
- `-agent-signing-key-file`：单个未加密 PKCS8 Ed25519 `PRIVATE KEY` PEM 块；普通文件，
  权限 0400 或 0600，最多 4096 字节。多块、前缀杂文、PEM headers、其他算法和符号链接拒绝。
- `-agent-trust-roots-file`：由部署信任流程独立供应的 schemaVersion 1 JSON，最多 32 KiB、
  1—32 个不同 ID；普通文件，不允许 group/world 写入或执行位，也不允许符号链接。
- `-agent-revocation-refresh`：默认 false；启用 App 的拒绝证明维护工作器，要求私钥和
  独立信任根同时配置。它不续租任何正向资源、Prepared 或激活视图。

信任根文件示例（publicKey 必须替换为独立信任流程提供的原始 32 字节 Ed25519 公钥的
标准带填充 Base64，不是整个 SPKI/PEM 的 Base64；日期按实际部署有效期填写）：

```json
{
  "schemaVersion": 1,
  "keys": [
    {
      "id": "agent-signing-2026-10",
      "publicKey": "<base64-of-raw-32-byte-Ed25519-public-key>",
      "notBefore": "2026-10-01T00:00:00Z",
      "notAfter": "2026-11-01T00:00:00Z"
    }
  ]
}
```

私钥 ID 必须在该文件中存在，且公钥匹配；加载时该根必须已生效并至少还有超过一分钟的
有效期。文件可包含为轮换保留的旧根和尚未生效的下一根，但必须至少有一个当前可接受根；
未来根不能提前验签。省略私钥而仅配置信任根可运行不负责签发的验证副本。配置私钥则
必须同时显式提供根文件，不从私钥、数据库或收到的资源自动生成信任。

典型附加启动参数：

```sh
-agent-signing-key-id agent-signing-2026-10 \
-agent-signing-key-file /run/secrets/grayling-agent-signing.pem \
-agent-trust-roots-file /etc/grayling/agent-trust-roots.json \
-agent-revocation-refresh
```

文件加载和匹配检查在持久化存储打开前完成，错误不回显密钥内容。私钥及解码后的 PEM
临时缓冲在解析后清理；这是尽力减少副本，不承诺运行时内存完全擦除。上述参数均要求
`-agent`，并与离线备份/恢复验证/HMAC 轮换命令互斥。输入在启动时读取，替换文件不会
热更新正在运行的实例；根轮换/退役和跨副本部署顺序仍需独立流程及后续验收。

`make test-agent-cli` 验证加载边界及真实二进制：不安全私钥在 PostgreSQL 建表前拒绝；
正确配置后 TLS 管理 API、WSS 能力、独立验签、自动证明续签和 SIGTERM 排空均实际执行。
这不代表自动正向续签、管理发布编排、实际 agent/gogw 或完整生产验收完成。


### SDK 实际准备候选与 Runtime 资源交接

`Client.PrepareRelease(ctx, prepareEvent, PreparationOptions)` 显式处理调用者选中的 Prepare。
它检查 release/command/期限和当前 instance/boot，下载精确 Bundle 与固定 policy，验证
独立信任根、scope、epoch、资源依赖及本地 build/image/protocol，然后调用本地 Builder。
SDK 不决定应用资源的具体存储位置；Builder 负责取得并编译实际应用资源。返回的
PreparationResources.Artifacts 必须完整对应签名 build 的资源键/SHA-256，无缺项、多项，
总字节受 MaxResourceBytes 限制。Builder 必须转交资源独占所有权，并提供线程安全的
CheckHealth、Instantiate 和 Close；回调须响应 context，不能保留可变输入别名。

SDK 在构造后、健康检查后及实例化前后重新验签/检查字节，成功后才生成固定 Prepared
并发送。PreparationResult 将 Workload、Prepared 与远端 Receipt 分开保留；构建、摘要或
健康失败不会发送 Prepared，也不会自动发送可能与未知结果冲突的 NACK。失败后若返回了
Workload，仍须 Close 等待其清理。回报失败但 Prepared 非空时保留该句柄，并通过
`Workload.RetryPrepared` 在重连后重试原报告；不要再次调用 PrepareRelease 重造时间戳。

PreparedWorkload 是进程内候选资源，不是活动视图，也不持久化“已就绪”。报告时间和
最长 300 秒期限固定，同时受 Bundle/policy 签名期限约束；重试不续 TTL。可信时钟和
单调期限同时生效，到期自动退役。UpdateTrustRoots 会影响后续重验，不会自动给旧根
续权。丢失实际候选或 workload boot 改变时，必须明确重新准备，不能仅从 ACK 恢复就绪。

`Workload.BuildRuntime` 可用作 RuntimeOptions.Builder。它要求激活的 Bundle/policy 摘要、
资源版本、generation、scope、本地身份以及 source 的 Prepare command 精确匹配，才调用
Instantiate 创建拥有完整 handler/传输池的独立 RuntimeResources。最终安装仍经过
RuntimeManager 的原子持久化/交换和资源校验；仅此后发送 Applied，不能把 Prepared 当作
已生效。对多次重新准备，使用 PreparationSet.Prepare 管理候选，并将 set.BuildRuntime
固定配置为 RuntimeOptions.Builder。

Retire 阻止新的准备回报和实例化，并释放候选 owner 引用，活动 RuntimeViews 保留其
引用直到最后请求排空。Close(ctx) 额外等待全部引用和资源清理，可超时后再等待；不要在
仍需关闭 RuntimeManager 时先无限等待 Workload.Close。通常先关闭 RuntimeManager，
再关闭候选；或先 Retire、排空 Runtime，再等待 Close。应用还需把健康变化映射到自身
Ready/Draining，不能依赖旧 Prepared 证据表达当前健康。

真实 PostgreSQL/WSS 测试覆盖资源摘要/健康失败无终态、固定回报重连重试、根退役、错误
boot 不继承候选、Prepared 阶段无活动视图、随后完整 Runtime 安装执行实际资源构造的
handler，以及候选到期自动清理和活动视图排空前资源不提前关闭。构造回调采用测试内实际
字节提供器；应用专用资源下载器、完整注册续租/调度循环、实际 gogw、探测及
ACTIVE 仍待完成，此 SDK 组件不代表完整 Agent 或 A01—A28 已验收。

PreparationSet 使用已认证 Welcome 的 controlPlaneEpoch、release generation 和服务端
Prepare.targetRevision 选择进程内候选；要求 targetRevision 非零，拒绝 epoch 跳变、旧版本
及同版本不同命令/资源。新 target 到来先退役旧候选并取消旧构建，迟到结果不能重新成为
当前候选。相同命令并发去重，重连复用固定 Prepared，不重新构建或延长证据期限。

最多同时构建 4 个、保留 8 个尚未完成清理的候选/构建；回调不响应取消时返回
ErrBackpressure，不无限开新任务。清理失败会锁存首个错误并停止新构建/选择，Close 返回
该错误。已安装 RuntimeViews 仍持有自己的引用。Close(ctx) 可超时，后台继续拥有并等待
构建、回报与资源排空，通常应先关闭 RuntimeManager 再等待 set.Close。

服务端明确返回冲突、fenced、权限等拒绝时退役该命令候选；网络错误或未知回执仍保留
本地 Prepared 供精确重试。只有匹配签名激活通过 RuntimeManager 安装后才成为活动视图。
UpdateTrustRoots 会阻止旧根快照下的在途构建发布；应用仍须同步更新 RuntimeManager
的信任根。选择顺序仅在进程内保持，不能恢复磁盘 ACK 为 Ready，也不替代注册/续租、
自动调度、应用资源下载或控制面的持久权限检查。

`NewInstanceRegistration(register)` 与 `RunRegistered(ctx, options, registration, serve)`
为一个明确授权的 deployment/boot 自动维护注册和租约。request 被克隆，lease 必须为空；
实例 scope/instance/boot/build/image/protocol 固定。每次连接刷新机器凭证/端点的 options
仍须保持 agent/instance/boot；自动要求 instance.register 和 instance.renew 能力。Welcome
scope 与控制面 epoch 必须匹配，epoch 跳变停止，不能靠重连绕过独立恢复授权。

首次注册固定操作 ID；回执未知时重放同一请求和 ID。已注册后重连先恢复未决续租，或为
当前 lease 创建新的续租操作，绝不因重连另做注册或更换 lease epoch。续租独立于 serve
回调，使用服务端间隔（最多 20 秒），并根据剩余租期提前；TTL 最大 60 秒。临时错误及
请求超时重试原操作，明确权限/身份拒绝停止。有效性同时受 UTC 截止和带 5 秒保守时钟
余量的单调期限约束，读取 State 不续租；相同到期时间回执不延长原单调期限。

State 返回克隆的 Instance、discovery revision、Connected 和 LeaseValid，仅表示登记与
链路状态，不代表应用健康或路由 Ready。Ready/Draining 必须来自实际应用生命周期，
不能由连接/租约/磁盘 ACK 推导；当前 owner 不自动更新健康、注销或重新授权。租约过期
会停止，须由调用方明确取得当前授权后建立新 owner，不能静默换身份恢复。

serve(ctx, client) 消费事件并处理准备、激活及撤销；此次新增仅自动维护连接/注册/续租。
serve 返回 nil 正常结束，应用错误结束；会话断开取消 serve 后按原 Run 退避重连。每个
owner 同时只允许一个 RunRegistered。取消会关闭连接并等待回调退出；回调不响应取消
时保有 owner 并阻止重连/替换，不能绕过旧回调启动无限新任务。调用方仍负责关闭自己的
RuntimeManager/PreparationSet。自动事件编排、应用资源下载器及完整 Agent 进程待补齐。

make test-agent-instances 已包括注册循环测试。真实 PostgreSQL/WSS 双副本覆盖重连不
重新注册、默认续租配置（20 秒上限，按剩余租期提前）、Ready 不被制造和关闭排空；SDK 故障注入覆盖丢失注册与
续租回执后精确重试、异常租约/身份拒绝、重复 runner 和不响应取消回调的所有权保留。
