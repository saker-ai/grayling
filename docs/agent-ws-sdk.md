# WebSocket Agent Go SDK

状态：客户端实现；服务端 Agent 端点与领域服务尚未随本变更实现。依据[统一实施与验收方案](统一Agent协议实施与验收方案.md)和工作区已有协议契约实现。不能据此声称 P0—P6 或 A01—A28 全部通过。

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
| Register / RenewLease | 带 operationId 的注册和续租，返回具体回复类型 |
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
