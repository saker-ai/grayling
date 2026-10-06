# Nacos 原生客户端兼容性验证（Issue #11）

## 结论与支持边界

在固定的 gonacos `v0.0.0-20261006082753-5ac0bf0b6806` 上，官方 Go 客户端 `github.com/nacos-group/nacos-sdk-go/v2 v2.3.5` 已通过真实服务测试：首次读取、`ListenConfig` 收到 manifest 更新、重新获取文档并本地求值、缺文档时保留旧值、漏通知后同一客户端定期复核 manifest 并跳过中间 revision 收敛，以及新客户端按当前 revision 恢复。测试入口是 `GOWORK=off go test ./test/native -count=1`。

`TestNativeSnapshotAtomicityAndStatus` 还通过真实 gonacos 验证了多文档候选：一个新文档可读、另一个缺失或 hash 错误时，生效 revision 和两个求值结果都留在旧版本，错误状态可见；修复为更高的完整 revision 后两个值一起切换。测试随后将先前读取的旧候选延迟提交，确认 revision 和两个求值结果均不回退。`TestOfficialNacosClientReconnectsAfterServerRestart` 保留同一个官方客户端及其订阅，关闭真实 gonacos 服务并在原端口重启，发布新 revision 后靠 manifest 复核收敛；旧求值在收敛前不变。该重启用例覆盖已建立 gRPC 连接断开，不覆盖网络黑洞或跨节点故障切换。

`TestOfficialNacosClientConvergesAfterNetworkBlackhole` 在官方客户端与真实 gonacos 之间放置 HTTP/gRPC 双向 TCP 代理，故障期间静默丢弃流量而不主动关闭连接。期间发布两个新 revision，同一客户端保持旧的已验证求值；恢复网络后按 manifest 复核收敛到最新 revision。此测试已连续运行三次通过，覆盖单节点网络黑洞恢复。

`TestOfficialNacosClientFailsOverAcrossGolabNodes` 在共享真实 PostgreSQL 与 Redis 的两个独立 GoLab 进程上验证跨节点恢复。客户端配置 B、A 两个地址，B 先离线，首次从 A 读取完整 revision；B 恢复且两个 GoLab SDK 副本确认共享状态后，终止 A，改从 B 发布新 revision。保留同一个官方客户端和 manifest 订阅，反复获取 manifest 及其引用文档并校验 hash、key，最终从 B 读到新 revision。用例连续运行两次通过。它验证原生客户端的连接切换与新版本可读性；客户端侧的多文档原子换入由 `TestNativeSnapshotAtomicityAndStatus` 单独验证。

**原生订阅不是 GoLab 的正式支持接口。** gonacos gRPC 配置查询允许客户端自选 namespace，且无需租户凭证。测试证明：客户端用 B 的 namespace 不能直接看到 A 的文档，但任何能连接 gonacos 端口的客户端，只要指定 A 的 namespace ID，就能读取 A 的 manifest 和不可变配置文档，并订阅 A 的 manifest 更新。这不满足跨租户隔离验收要求。`cmd/golab` 默认仅将 gonacos HTTP/gRPC 绑定到 loopback；不得把端口开放给租户或把 GoLab SDK token 当作 Nacos 凭证。上游 gonacos 需要对 gRPC 查询、订阅和推送执行可验证的租户 ACL，完成后再做拒绝跨租户读取的真实客户端测试，才能考虑正式支持。`go test -race ./test/native -count=3` 曾在 gonacos 的双向 gRPC 流写入与 flush 之间检测到数据竞争（模块缓存的 `pkg/protocol/grpc/server.go`）；跨租户订阅用例的 race 运行还检测到官方 `nacos-sdk-go/v2 v2.3.5` 在 `CloseClient` 清理时与后台任务并发访问。`go test -race ./test/native -run TestOfficialNacosClientReconnectsAfterServerRestart -count=1` 还检测到旧订阅仍活跃时再次 `server.New` 会与 gonacos 的进程级全局状态写入发生竞争。上游并发缺陷需修复并复测。gonacos 已补 Apache-2.0 LICENSE/NOTICE；许可文件更新不解除这里的租户 ACL 阻断。

## 试验协议

| 项目 | 当前映射 |
| --- | --- |
| 客户端 | 官方 `nacos-sdk-go/v2 v2.3.5`；其他版本、Java/Python 客户端未验证 |
| 连接 | gonacos HTTP 端口及其独立 gRPC 端口；试验限定本机可信进程 |
| 认证 | 原生 gRPC 配置接口目前未实施 GoLab workspace/environment 鉴权；不能用 GoLab SDK token 登录 |
| 命名空间 | 每个 `(workspace, environment)` 使用平台生成的独立 namespace ID，不能用用户输入的名字推导 |
| Group | 固定 `GOLAB_FLAGS` |
| 订阅入口 | `golab.manifest`；`schemaVersion=1`，`revision` 单调递增，`flags` / `segments` 条目给出 `key`、`revision`、`dataId`、SHA-256 `hash` 和可选 `deleted` tombstone |
| 不可变文档 | `golab.flag.<key>.r<revision>`、`golab.segment.<key>.r<revision>`，JSON 正文字节的 SHA-256 必须匹配 manifest |

收到通知后只能把它视为失效提示：重新 `GetConfig(golab.manifest)`，按该 manifest 获取全部活动文档，逐一校验 hash、key 和 schema，再通过 GoLab evaluator 编译。候选 revision 高于当前 revision 且全部成功，才一次性换入。任何读取、hash、JSON 或编译失败都保留 last-known-good，记录最新错误及生效 revision；超时未复核时应按调用方设定的最大陈旧期降级。连接恢复和定期轮询都要重新读取 manifest，不能仅等推送。测试中的试验代码展示了取数和校验，**并未作为新的可复用 SDK 发布**。

## 与正式 GoLab SDK 的差异

GoLab Go SDK 使用受 GoLab token 约束的 `/api/v1/sdk/config` 全量快照、ETag/304、SSE 失效提示及定期轮询；服务端从凭证导出 scope，客户端不能任意指定 namespace。它已提供原子换入、last-known-good、`Status()`（revision、陈旧状态、错误）与最大陈旧期处理，并支持曝光上报。原生 Nacos 客户端只提供配置读取和通知；原生协议不提供这些 GoLab 语义，也没有 GoLab 的租户授权。浏览器、Python/TypeScript SDK 继续走 GoLab 远程求值接口。

复现与后续验收：先在 gonacos 上补齐 gRPC 配置读/订阅的租户 ACL 和凭证映射；将 `test/native` 的跨租户特征测试改成**凭证 A 读 B 必须拒绝**的负向断言，覆盖 manifest 与不可变 dataId；在授权后的真实客户端上回归 `test/cluster` 的跨节点切换、丢通知、乱序和缺文档期间生效 revision 不回退、无部分快照。部署侧必须验证 HTTP/gRPC 端口网络策略，而不能以 namespace ID 保密代替授权。
