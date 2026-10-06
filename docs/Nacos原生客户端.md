# Nacos 原生客户端兼容性验证（Issue #11）

## 结论与支持边界

在固定的 gonacos `v0.0.0-20261006194453-ea39c83bf6f0` 上，官方 Go 客户端 `github.com/nacos-group/nacos-sdk-go/v2 v2.3.5` 已通过真实服务测试：首次读取、`ListenConfig` 收到 manifest 更新、重新获取文档并本地求值、缺文档时保留旧值、漏通知后同一客户端定期复核 manifest 并跳过中间 revision 收敛，以及新客户端按当前 revision 恢复。测试入口是 `GOWORK=off go test ./test/native -count=1`。

`TestNativeSnapshotAtomicityAndStatus` 还通过真实 gonacos 验证了多文档候选：一个新文档可读、另一个缺失或 hash 错误时，生效 revision 和两个求值结果都留在旧版本，错误状态可见；修复为更高的完整 revision 后两个值一起切换。测试随后将先前读取的旧候选延迟提交，确认 revision 和两个求值结果均不回退。`TestOfficialNacosClientReconnectsAfterServerRestart` 保留同一个官方客户端及其订阅，关闭真实 gonacos 服务并在原端口重启，发布新 revision 后靠 manifest 复核收敛；旧求值在收敛前不变。该重启用例覆盖已建立 gRPC 连接断开，不覆盖网络黑洞或跨节点故障切换。

`TestOfficialNacosClientConvergesAfterNetworkBlackhole` 在官方客户端与真实 gonacos 之间放置 HTTP/gRPC 双向 TCP 代理，故障期间静默丢弃流量而不主动关闭连接。期间发布两个新 revision，同一客户端保持旧的已验证求值；恢复网络后按 manifest 复核收敛到最新 revision。此测试已连续运行三次通过，覆盖单节点网络黑洞恢复。

`TestOfficialNacosClientFailsOverAcrossGolabNodes` 在共享真实 PostgreSQL 与 Redis 的两个独立 GoLab 进程上验证跨节点恢复。客户端配置 B、A 两个地址，B 先离线，首次从 A 读取完整 revision；B 恢复且两个 GoLab SDK 副本确认共享状态后，终止 A，改从 B 发布新 revision。保留同一个官方客户端和 manifest 订阅，反复获取 manifest 及其引用文档并校验 hash、key，最终从 B 读到新 revision。用例连续运行两次通过。它验证原生客户端的连接切换与新版本可读性；客户端侧的多文档原子换入由 `TestNativeSnapshotAtomicityAndStatus` 单独验证。

**原生订阅尚不是 GoLab 的正式支持接口。** 默认 gonacos 仍仅绑定 loopback，默认模式没有原生客户端租户鉴权；`TestNativeClientCanReadOtherTenantWithoutAuthorization` 保留为默认模式的安全边界用例。GoLab 新增显式 `-nacos-native-auth` 模式：官方客户端使用用户名 `golab-sdk`、密码为 GoLab SDK token，经原生 HTTP 登录；该模式的 HTTP 端口只提供登录，其他 gonacos HTTP 路由一律 404。gRPC 只允许连接维护、配置查询和批量订阅；每次配置读/订阅以服务端 `ResolveToken` 的 SDK audience 与 `(workspace, environment)` 推导唯一 namespace，Group 限定 `GOLAB_FLAGS`，dataId 限定 manifest/不可变文档；推送前再次核验凭证，撤销后不再发送通知。官方 Go SDK 的真实客户端测试已验证 A 凭证读取 A、B 凭证不能读取 A 的 manifest/文档或订阅 A 更新，以及撤销已建立订阅后的推送停止。SDK 在网络读取失败时可能返回本地 last-known-good 缓存，因此调用方不能仅用 `GetConfig` 的成功返回推断凭证仍有效。

gonacos 提交 [`b001ebd`](https://github.com/godeps/gonacos/commit/b001ebd8082f) 将推送订阅从客户端 IP 改为服务端连接 ID 并串行化双向流写入；[`dee1d72`](https://github.com/godeps/gonacos/commit/dee1d72) 增加逐帧鉴权钩子，[`3836219`](https://github.com/godeps/gonacos/commit/3836219) 增加原生配置限定模式及推送时复核，[`ea39c83`](https://github.com/godeps/gonacos/commit/ea39c83) 消除了嵌入式实例之间共享 gRPC Server 导致的鉴权状态污染。相关 gonacos 包的 `go test -race` 通过。原生客户端完整 `-race` 验收仍未通过：官方 `nacos-sdk-go/v2 v2.3.5` 的 `CloseClient` 后台清理曾触发竞态，需要定位和复测。安全模式还需在真实双节点部署下验收，且 HTTP/2 代理不得将不同客户端复用到同一后端连接；订阅关联依赖服务端连接 ID，部署应使用客户端独占的 TCP 连接或端到端 TLS 透传。未完成这些验证前，端口不得直接对租户开放。

## 试验协议

| 项目 | 当前映射 |
| --- | --- |
| 客户端 | 官方 `nacos-sdk-go/v2 v2.3.5`；其他版本、Java/Python 客户端未验证 |
| 连接 | gonacos HTTP 端口及其独立 gRPC 端口；默认仅 loopback；鉴权模式仍须验证直接连接或不复用后端连接的透传部署 |
| 认证 | `-nacos-native-auth` 模式用 `golab-sdk` + GoLab SDK token 登录；每次读/订阅/推送复核 workspace/environment，默认模式不启用此接口 |
| 命名空间 | 每个 `(workspace, environment)` 使用平台生成的独立 namespace ID，不能用用户输入的名字推导 |
| Group | 固定 `GOLAB_FLAGS` |
| 订阅入口 | `golab.manifest`；`schemaVersion=1`，`revision` 单调递增，`flags` / `segments` 条目给出 `key`、`revision`、`dataId`、SHA-256 `hash` 和可选 `deleted` tombstone |
| 不可变文档 | `golab.flag.<key>.r<revision>`、`golab.segment.<key>.r<revision>`，JSON 正文字节的 SHA-256 必须匹配 manifest |

收到通知后只能把它视为失效提示：重新 `GetConfig(golab.manifest)`，按该 manifest 获取全部活动文档，逐一校验 hash、key 和 schema，再通过 GoLab evaluator 编译。候选 revision 高于当前 revision 且全部成功，才一次性换入。任何读取、hash、JSON 或编译失败都保留 last-known-good，记录最新错误及生效 revision；超时未复核时应按调用方设定的最大陈旧期降级。连接恢复和定期轮询都要重新读取 manifest，不能仅等推送。测试中的试验代码展示了取数和校验，**并未作为新的可复用 SDK 发布**。

## 与正式 GoLab SDK 的差异

GoLab Go SDK 使用受 GoLab token 约束的 `/api/v1/sdk/config` 全量快照、ETag/304、SSE 失效提示及定期轮询；服务端从凭证导出 scope，客户端不能任意指定 namespace。它已提供原子换入、last-known-good、`Status()`（revision、陈旧状态、错误）与最大陈旧期处理，并支持曝光上报。原生 Nacos 客户端只提供配置读取和通知；原生协议不提供这些 GoLab 语义，也没有 GoLab 的租户授权。浏览器、Python/TypeScript SDK 继续走 GoLab 远程求值接口。

后续验收：在真实 GoLab 双节点进程中启用 `-nacos-native-auth`，用各自的 SDK token 回归跨节点切换、丢通知、乱序和缺文档期间生效 revision 不回退、无部分快照；验证直连/透传部署不发生 HTTP/2 后端连接复用和 HTTP 旁路；定位并复测原生客户端完整 race。`test/native` 的默认模式越权特征用例继续约束未授权底座的边界，安全模式负向用例约束可支持路径。
