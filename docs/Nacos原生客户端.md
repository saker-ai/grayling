# Nacos 原生客户端兼容性与部署验收（Issue #11 / #13 / #17）

## 固定版本兼容矩阵（Issue #17）

机器可读矩阵见 [`client-compatibility-matrix.json`](client-compatibility-matrix.json)。仅将官方 Go `nacos-sdk-go/v2 v2.3.5` 与本仓库固定的 gonacos 版本列为**已测试组合**；其他官方 SDK 版本及其他语言 Nacos SDK 没有验证，不在本矩阵的支持声明中。`make client-compatibility` 检查 `go.mod` 三个依赖版本并逐项运行真实客户端测试，缺测、跳过或失败都会使门禁失败；CI 的 “Client compatibility matrix” 工作流在依赖和相关测试变更时运行，并把 PASS/FAIL/INCOMPLETE/LIMIT 写入 job summary。升级官方 SDK 时必须同步更新矩阵版本、重新审查限制并跑完整矩阵，不能只改 `go.mod`。

| 固定版本 / 拓扑 | 功能证据 | 限制 |
| --- | --- | --- |
| v2.3.5，单节点 loopback 直连 | manifest 读取、订阅、撤销订阅、服务重启、网络黑洞恢复、显式鉴权的租户隔离和凭证撤销；默认匿名模式跨租户可读的负向边界用例 | 本机实验兼容性验证；撤销订阅须处理 SDK 在途监听任务，不能承诺单次调用后立即停止回调；默认匿名模式不作为租户可达接口 |
| v2.3.5，双节点本机 TLS/L4 一对一转发，共享真实 PostgreSQL/Redis | 订阅、节点故障切换、租户隔离、凭证撤销及 HTTP/2 后端连接复用的负向探针 | 不是跨主机生产网络策略验收；L7 复用不支持 |

完整官方客户端 `-race` 不能宣称通过。可选最小复现命令为 `GOWORK=off go test -race -tags sdk_race_repro ./test/native -run '^TestOfficialSDKClientMapRaceReproducer$' -count=1`；该用例只并发关闭和新建官方 SDK 客户端，服务端使用真实 gonacos，预期 race detector 定位到 SDK 的全局 `rpc.clientMap`。此命令**预期非零退出**，不作为通过项；见上游 [#861](https://github.com/nacos-group/nacos-sdk-go/issues/861)（v2.3.5 的并发 client map 写）和 [#833](https://github.com/nacos-group/nacos-sdk-go/issues/833)（Start/health-check 内部竞态）。这两个上游 issue 当前已关闭，但固定的 v2.3.5 仍能复现上述 client map 竞态；升级时必须重新检查。GoLab/gonacos 自身竞态仍由各自 `-race` 检查处理，不能把第三方竞态忽略后称整个客户端通过。

`CancelListenConfig` 在 v2.3.5 中只从本地缓存表移除监听器；与正在执行的监听任务并发时，旧任务可能把监听器再写回。固定用例等待该任务收敛后第二次撤销，再发布新 revision 并确认没有回调。业务调用方若需要严格撤销，应关闭整个客户端或采用这类收敛后复核流程；**单次撤销调用后的即时无回调不在已验证范围内**。

## 结论与支持边界

在固定的 gonacos `v0.0.0-20261006194453-ea39c83bf6f0` 上，官方 Go 客户端 `github.com/nacos-group/nacos-sdk-go/v2 v2.3.5` 已通过真实服务测试：首次读取、`ListenConfig` 收到 manifest 更新、重新获取文档并本地求值、缺文档时保留旧值、漏通知后同一客户端定期复核 manifest 并跳过中间 revision 收敛，以及新客户端按当前 revision 恢复。测试入口是 `GOWORK=off go test ./test/native -count=1`。

`TestNativeSnapshotAtomicityAndStatus` 还通过真实 gonacos 验证了多文档候选：一个新文档可读、另一个缺失或 hash 错误时，生效 revision 和两个求值结果都留在旧版本，错误状态可见；修复为更高的完整 revision 后两个值一起切换。测试随后将先前读取的旧候选延迟提交，确认 revision 和两个求值结果均不回退。`TestOfficialNacosClientReconnectsAfterServerRestart` 保留同一个官方客户端及其订阅，关闭真实 gonacos 服务并在原端口重启，发布新 revision 后靠 manifest 复核收敛；旧求值在收敛前不变。该重启用例覆盖已建立 gRPC 连接断开，不覆盖网络黑洞或跨节点故障切换。

`TestOfficialNacosClientConvergesAfterNetworkBlackhole` 在官方客户端与真实 gonacos 之间放置 HTTP/gRPC 双向 TCP 代理，故障期间静默丢弃流量而不主动关闭连接。期间发布两个新 revision，同一客户端保持旧的已验证求值；恢复网络后按 manifest 复核收敛到最新 revision。此测试已连续运行三次通过，覆盖单节点网络黑洞恢复。

`TestOfficialNacosClientFailsOverAcrossGolabNodes` 在共享真实 PostgreSQL 与 Redis 的两个独立 GoLab 进程上启用 `-nacos-native-auth=true`，验证跨节点恢复。客户端凭 GoLab SDK token 配置 B、A 两个地址，B 先离线，首次从 A 读取完整 revision；B 恢复且两个 GoLab SDK 副本确认共享状态后，终止 A，改从 B 发布新 revision。保留同一个官方客户端和 manifest 订阅，反复获取 manifest 及其引用文档并校验 hash、key，最终从 B 读到新 revision。另一租户的 SDK token 从 A、B 均无法读取该 manifest；撤销凭证后，B 的原生登录返回 401。它验证直连拓扑下的连接切换、跨租户拒绝、撤销和新版本可读性；客户端侧的多文档原子换入由 `TestNativeSnapshotAtomicityAndStatus` 单独验证。

**原生订阅尚不是 GoLab 的正式支持接口。** 默认 gonacos 仍仅绑定 loopback，默认模式没有原生客户端租户鉴权；`TestNativeClientCanReadOtherTenantWithoutAuthorization` 保留为默认模式的安全边界用例。GoLab 新增显式 `-nacos-native-auth` 模式：官方客户端使用用户名 `golab-sdk`、密码为 GoLab SDK token，经原生 HTTP 登录；该模式的 HTTP 端口只提供登录，其他 gonacos HTTP 路由一律 404。gRPC 只允许连接维护、配置查询和批量订阅；每次配置读/订阅以服务端 `ResolveToken` 的 SDK audience 与 `(workspace, environment)` 推导唯一 namespace，Group 限定 `GOLAB_FLAGS`，dataId 限定 manifest/不可变文档；推送前再次核验凭证，撤销后不再发送通知。官方 Go SDK 的真实客户端测试已验证 A 凭证读取 A、B 凭证不能读取 A 的 manifest/文档或订阅 A 更新，以及撤销已建立订阅后的推送停止。SDK 在网络读取失败时可能返回本地 last-known-good 缓存，因此调用方不能仅用 `GetConfig` 的成功返回推断凭证仍有效。

gonacos 提交 [`b001ebd`](https://github.com/godeps/gonacos/commit/b001ebd8082f) 将推送订阅从客户端 IP 改为服务端连接 ID 并串行化双向流写入；[`dee1d72`](https://github.com/godeps/gonacos/commit/dee1d72) 增加逐帧鉴权钩子，[`3836219`](https://github.com/godeps/gonacos/commit/3836219) 增加原生配置限定模式及推送时复核，[`ea39c83`](https://github.com/godeps/gonacos/commit/ea39c83) 消除了嵌入式实例之间共享 gRPC Server 导致的鉴权状态污染。相关 gonacos 包的 `go test -race` 通过。完整原生客户端 `-race` 仍报告官方 `nacos-sdk-go/v2 v2.3.5` 内部竞态：`CloseClient` 与其全局 client map 访问并发，以及断线重连时连接状态的并发读写；这不是 GoLab 或 gonacos 服务端栈。原生订阅的订阅关联依赖服务端连接 ID，已验证直连 TCP 与本机 TLS/L4 一对一转发；L7 HTTP/2 代理若复用不同租户到同一后端连接，当前不属于支持拓扑。默认保持 loopback，外部访问必须另行验证网络隔离与 TLS 终止边界。

## Issue #13：支持矩阵与当前验收范围

截至 2026-10-07，**原生 Nacos 订阅仍未列为生产正式支持接口**。下表将真实进程测试与待完成的部署验收分开；本机 TLS/L4 测试证明协议路径和租户约束，不能代替跨主机网络策略的实测。

2026-10-07 预检增强：允许源除 `h2` 握手外，须用 `cmd/nacos-native-grpc-probe` 对每个 gRPC 入口 IP 直发无凭证配置查询及批量订阅请求，两者均须得到 gRPC status 16；否则不写 PASS 证据。配对核验要求 `anonymousGRPCDenied` 与 `anonymousGRPCSubscriptionDenied`。探针的读与订阅拒绝路径由本地真实 TLS/HTTP2 服务测试覆盖；双 GoLab 进程本机 TLS/L4 定向用例也验证无凭证订阅返回 status 16。本轮拓扑预检器 33/33、`make all` 与 `make client-compatibility` 通过。没有跨主机现场入口与代理/网络策略日志，生产支持结论不变。

2026-10-07 双节点本机 TLS/L4 回归补充：真实进程测试在同一条 HTTP/2 连接上，确认 A 撤销前订阅返回 gRPC status 0、B 对 A 命名空间订阅返回 status 7；撤销 A 的 SDK token 后，A 再次订阅返回 status 16。原有测试还确认撤销后直接读取返回 status 16、旧订阅不再收到新 revision 推送。这样同时检查了既有订阅与新订阅的撤销边界；它仍不是跨主机网络策略或现场代理验收。

2026-10-07 声明门禁复核：`allowedSources` 的两条 `/1` 即使合计覆盖整个 IPv4 或 IPv6 地址空间也会被拒绝；合法相邻工作负载网段仍可通过。`python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 32/32、示例声明检查、`make all`（vet、全量 Go 测试、构建）、`make client-compatibility`（直连 9/9、本机双节点 TLS/L4 1/1）及 `git diff --check` 通过。该门禁不能认定任意非全网 CIDR 已获批准，现场仍须核对网络策略；本次没有新增跨主机现场验收证据。

| 拓扑 | 现有证据 | 正式支持 |
| --- | --- | --- |
| 默认匿名 gonacos、仅 loopback | 原生功能测试显示知道 namespace 即可跨租户读取；启动时即使设置 `GOLAB_ALLOW_NON_LOOPBACK_GONACOS=1` 也拒绝匿名非 loopback 绑定 | 不作为租户可达接口 |
| `-nacos-native-auth=true`、单/双节点直连 TCP | 官方 Go SDK v2.3.5 真实进程读、订阅、跨租户拒绝、撤销与双节点切换（#11） | 仅实验兼容性验证；明文连接不能跨主机部署 |
| 双节点 TLS 终止 + 每个客户端 TCP 连接对应独立后端 TCP 连接，gonacos 后端仅 loopback | `test/cluster/TestOfficialNacosClientFailsOverAcrossGolabNodes` 使用真实 GoLab/PG/Redis 及 TLS/L4 代理，验证两个租户、故障切换、HTTP 绕过、撤销；直接向同一 TLS HTTP/2 连接发送原生配置查询，验证有效凭证成功、匿名及撤销后的凭证均被拒；代理与后端在同一测试主机 | **待跨主机入口网络策略与拒绝源验收**；目前不声明生产正式支持 |
| L7 HTTP/2 代理多路复用不同租户到同一后端 gRPC 连接 | 双租户各自发布 manifest 后，共享一条后端连接：B 读取 A 的 namespace 返回 status 7，读取 B 自己的 namespace 返回 status 0，但订阅 B 自己的 namespace 返回 status 7；B 换用独立后端连接订阅返回 status 0。订阅读取行为不一致，且真实 L7 代理的推送身份隔离未证明；`scripts/check_nacos_native_topology.py` 对复用配置报错 | **不支持** |
| 其他语言或官方 SDK 其他版本 | 未测试 | 不支持 |

2026-10-07 先前复核：`GOWORK=off go test ./test/cluster -run '^TestOfficialNacosClientFailsOverAcrossGolabNodes$' -count=1` 通过，当时的负向探针只让 B 读取、订阅 A 的 namespace，均返回 status 7；这不能证明 B 在复用连接上访问自己的 namespace 的行为。`make all`、`make client-compatibility`（直连 9/9、双节点本机 TLS/L4 1/1）和拓扑检查器 25 个单元测试通过。测试使用现有 PostgreSQL、测试自管 Redis，并将已满 `/data` 上的 Go 临时构建目录改设为 `GOTMPDIR=/tmp`。此结果不包含跨主机 TLS/网络策略或真实 L7 代理验收。

2026-10-07 本轮补充：真实双 GoLab 进程、PostgreSQL、Redis 与官方 Go SDK 的定向用例增加 B 自己发布的 manifest。共享后端 HTTP/2 连接上 B 可以读取自己的 manifest（status 0），却不能订阅自己的 manifest（status 7）；换用独立连接后 B 订阅成功（status 0）。一条连接及两条连接分别由 TLS/L4 代理的接受计数确认。由此明确：跨租户读取被拒不等于 L7 连接复用可用，更不能证明推送隔离；运维部署仍须维持一客户端 TCP 连接对应一条后端 TCP 连接。该用例的 TLS/L4 代理与后端仍在同一测试主机。

撤销后连接身份检查：同一 TLS HTTP/2 连接在 A 凭证撤销后继续拒绝 A 的读与订阅（status 16）；B 在该连接上读取自己的 manifest 仍可成功，但订阅仍返回 status 7，不能因为 A 已撤销就接管其连接。B 建立独立连接后订阅返回 status 0，代理计数从一条增至两条。这项本机回归约束 L7 复用和撤销交互；它不代表真实 L7 代理或跨主机生产网络已验收。

2026-10-07 HTTP 绕过检查补强：现场允许源探针对每个 TLS HTTP 解析 IP 完成两租户有效登录后，分别携带其 token 的 `accessToken` 请求头读取**对应真实 namespace 的 manifest**，均须得到 404；`--verify-pair` 拒绝缺少 `authenticatedHTTPDenied` 的旧证据。本机双 GoLab 进程 TLS/L4 用例也验证有效 token 的 HTTP 配置请求仍返回 404。token 只放请求头和登录请求体，不放配置查询 URL 或证据 JSON；这项检查不能代替跨主机真实代理与策略日志。

### 运维部署模板与预检

允许源登录探针要求 HTTP TLS 入口协商 `http/1.1` ALPN，再核对 HTTP 200 并解析 Nacos 登录 JSON：`accessToken` 必须等于本次提交的 SDK token，`tokenTtl` 必须为正整数。未协商 `http/1.1`、通用 200 页面、空令牌或错误 TTL 均报 `ALERT`，不会写入 PASS 证据。响应体上限为 16 KiB；令牌值只在进程内比较，不写入错误输出或证据文件。允许源还须提交两个不同 namespace ID；原生 gRPC 探针对每个入口解析 IP 用两个独立 TLS/HTTP2 连接，分别检查两租户在自己 namespace 的 manifest 读取和订阅 status 0，以及对方 namespace 的读取和订阅 status 7，并检查匿名读取、订阅 status 16。对于自身读取与订阅，探针还验证实际 gRPC 响应帧、响应类型、`resultCode: 200` 和 `success: true`；自身读取必须有可解析的 manifest，且包含正 revision 与工作空间、环境 ID。仅返回 HTTP 200/gRPC status 0 的空响应不能通过。namespace ID 必须对应 token 所属环境，两个环境都应已有 manifest；探针只读和注册监听，不发布配置。`tenantGRPCIsolation` 标记这些检查通过。此门禁仍不证明后续推送、凭证撤销或双节点故障切换，现场须完成这些生命周期验收。

允许源证据还记录每个入口解析 IP 上实际验证过的服务端叶证书 SHA-256（`tlsCertSHA256`）；HTTP 登录探针的多次 TLS 连接若在一次运行中看到证书变化，会报 `ALERT`。`--verify-pair` 拒绝缺少这些指纹的旧证据，因此需要重新采集允许源记录。指纹是探针时的快照，须与现场代理证书部署记录核对；它不能单独证明后续连接继续使用同一证书，也不能代替代理连接日志与网络策略验收。

2026-10-07 先前验证：`python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 34/34、示例拓扑声明、`make all` 和 `make client-compatibility` 通过；真实双 GoLab 进程、PostgreSQL/Redis、本机 TLS/L4 用例调用新增探针，对两个实际 SDK token 的自身读取/订阅及交叉拒绝通过。后续提交已让原生 gRPC 探针核对响应帧、成功载荷及 manifest 的基本字段；正式现场验收还须由官方 SDK 核对完整正文、推送、撤销、重连和故障切换。本机证据不能替代跨主机网络策略、真实代理连接日志或生产支持批准。

2026-10-07 预检复核：`python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 30/30 通过，覆盖 HTTP TLS 未协商 ALPN 与误协商 `h2` 的拒绝路径；`python3 scripts/check_nacos_native_topology.py docs/nacos-native-topology.example.json`、`make all` 和 `make client-compatibility` 通过。后两项使用已有 PostgreSQL 与测试自管 Redis，兼容矩阵为直连 9/9、本机双节点 TLS/L4 1/1，保留 SDK 竞态和跨主机部署未验收的 LIMIT。未取得现场两节点策略或代理证据，正式支持状态不变。

2026-10-07 本次补强的核验：拓扑预检 Python 单元测试 29/29、示例声明检查、`git diff --check`、`make all`（vet、全量 Go 测试、构建）与 `make client-compatibility`（直连 9/9、本机双节点 TLS/L4 1/1）均通过。全量 Go 测试使用已有 PostgreSQL 的隔离临时数据库与测试自管 Redis。此轮没有取得跨主机两节点的现场允许/拒绝源记录，正式支持边界保持上表所述。

一个 GoLab 节点需提供两个独立的原生端口：HTTP 只用于 `/nacos/v1/auth/users/login`，gRPC 用于配置读取和订阅。以下是**待环境验证**的 TLS/L4 侧车示例，HAProxy 与 GoLab 在同一主机或同一网络命名空间；每个节点分别配置一份，负载均衡客户端持有两个节点的地址。`mode tcp` 保留客户端到后端的一对一 TCP 连接，禁止改成汇聚多租户后端连接的 L7 HTTP/2 路由。

```haproxy
frontend nacos_login
    mode tcp
    bind 10.42.1.10:8848 ssl crt /etc/haproxy/nacos.pem alpn http/1.1
    default_backend golab_nacos_http
backend golab_nacos_http
    mode tcp
    server local 127.0.0.1:18848
frontend nacos_grpc
    mode tcp
    bind 10.42.1.10:9848 ssl crt /etc/haproxy/nacos.pem alpn h2
    default_backend golab_nacos_grpc
backend golab_nacos_grpc
    mode tcp
    server local 127.0.0.1:19848
```

GoLab 使用 `-nacos-native-auth=true -gonacos-addr 127.0.0.1:18848 -gonacos-grpc-addr 127.0.0.1:19848`。TLS 证书必须由客户端信任的 CA 签发并匹配入口主机名；官方 SDK 同时设置 HTTP `https` scheme、显式 gRPC 端口和 `WithTLS(NewTLSConfig(WithCA(...)))`。入口防火墙/NetworkPolicy 只允许获批 SDK 工作负载源访问 8848/9848，拒绝其他源；后端 18848/19848 不得从容器或主机外部路由。不要对公众暴露默认匿名模式，不要把 SDK token 写进 URL、日志或探针参数。

复制并填写 [`nacos-native-topology.example.json`](nacos-native-topology.example.json)，然后在**每个节点**运行：

```sh
GOWORK=off go build -o ./bin/nacos-native-grpc-probe ./cmd/nacos-native-grpc-probe
python3 scripts/check_nacos_native_topology.py deployment-nacos.json
python3 scripts/check_nacos_native_topology.py deployment-nacos.json --probe \
  --grpc-probe-bin ./bin/nacos-native-grpc-probe \
  --tenant-namespace "$TENANT_A_NAMESPACE_ID" \
  --tenant-namespace "$TENANT_B_NAMESPACE_ID" \
  --login-token-file /run/secrets/tenant-a-sdk-token \
  --login-token-file /run/secrets/tenant-b-sdk-token \
  --evidence node-a-allowed.json
# 在拒绝源机器/Pod 上执行，使用同一份部署配置
python3 scripts/check_nacos_native_topology.py deployment-nacos.json --probe-denied --evidence node-a-denied.json
# 收集同一节点的两份 JSON 后，在任一验收主机运行
python3 scripts/check_nacos_native_topology.py deployment-nacos.json --verify-pair node-a-allowed.json node-a-denied.json
# 对 B 节点使用自己的配置执行相同探针，再核对两个不同入口的四份证据
python3 scripts/check_nacos_native_topology.py deployment-node-a.json --verify-cluster deployment-node-b.json node-a-allowed.json node-a-denied.json node-b-allowed.json node-b-denied.json
```

第一个命令检查声明中鉴权、loopback 后端、彼此独立的 HTTP/gRPC 后端端口及 TLS 端口、显式允许源与连接不复用；`allowedSources` 即使把 IPv4/IPv6 全地址空间拆成多条 CIDR，也会因汇总后等价于 `/0` 而拒绝。允许源网段若包含未指定地址、loopback、链路本地、多播或 IPv4 映射 IPv6 地址，也会被拒绝；IPv4 来源请直接写 IPv4 CIDR。此检查无法判断一个非全网的宽网段是否属于获批 SDK 工作负载，仍须对照现场网络策略审查。TLS 入口填 loopback、`localhost`、通配地址、链路本地或多播地址也会在声明阶段被拒绝。DNS 名称仍需通过在线探针验证其解析地址；探针及证据配对拒绝解析成 loopback、通配、链路本地、多播或映射到这些 IPv4 地址的 IPv6 地址。L7/HTTP2 多租户复用会输出 `ALERT` 并以非零状态退出，适合部署门禁或告警。第二个命令从允许源对两个 TLS 入口的**每个 DNS 地址**分别完成证书验证、gRPC `h2` TLS 握手、无凭证原生 gRPC 配置读取及批量订阅均返回 status 16、匿名 HTTP 配置读取 404（同时检查虚构 namespace 和两个实际租户 namespace）、无效登录 401，以及两个有效 SDK 凭证登录 200；还用两个有效 token 的 `accessToken` 请求头分别尝试读取各自 namespace 的 HTTP manifest，均须返回 404，防止已登录客户端绕过 gRPC 鉴权。token 不进入查询参数或证据 JSON。连接固定到所检查的 IP，证书校验和 HTTP Host 仍使用声明中的入口主机名。gRPC 负向探针是单独构建的 Go 二进制，部署时须与检查器一同分发并核对来源；未提供 `--grpc-probe-bin`、二进制执行失败或返回非预期状态都不能生成 PASS 证据。旧版缺少 `anonymousTenantHTTPDenied`、`authenticatedHTTPDenied`、`anonymousGRPCDenied` 或 `anonymousGRPCSubscriptionDenied` 的允许源证据需重新采集；前两项分别要求两个真实租户 namespace 的匿名 HTTP 读取，以及携带有效 token 请求头的 HTTP 读取均返回 404。该探针同时检查两个 token 对各自 namespace 的直接读取与订阅成功、交叉读取与订阅拒绝；不替代官方 SDK 的推送、撤销、重连及双节点切换实测。两个不同租户的 token 应分别存放在访问受限的单行文件中，不放入命令参数、日志或证据 JSON。脚本检查两个 token 和两个 namespace ID 各不相同；须保留现场 token 与环境的对应关系，以供审计。该探针还从允许源检查侧车宿主机的两个明文后端端口均拒绝 TCP，防止获批 SDK 主机绕过 TLS；不使用环境中的 HTTP 代理，并将 HTTP 重定向视为失败。第三个命令从拒绝源检查两个 TLS 入口和两个后端端口都无法建立 TCP，并逐个检查 DNS 返回的地址，不能因其中一个地址拒绝而漏过另一个开放地址；`backendExternalHost` 必须是允许源和拒绝源均可路由的侧车宿主机或 Pod **单播 IP**，不可填写 loopback、通配地址或主机名。两侧探针均记录四个目标的路由选择源 IP 与解析 IP，并按 `allowedSources` 检查允许源属于、拒绝源不属于允许网段；无路由、DNS 错误等探针故障不能计作网络策略拒绝。拒绝源和允许源的明文后端 TCP 探针必须使用各自路由检查时解析出的同一后端 IP；允许源完成 TLS/HTTP 探针后也会重新解析入口，若地址集合漂移则报警。第四个命令要求同一配置 SHA-256、同一 CA 文件内容 SHA-256、不同执行主机、四个目标端口完整、源 IP 分类正确、两侧入口和后端解析 IP 相同，且两份成功记录相差不超过五分钟。最后一个命令要求两个节点的 TLS 入口及可路由后端主机不同，两节点 TLS 入口的解析 IP 不重叠、允许源验证同一组两个租户 namespace，且四份证据处于同一五分钟窗口。旧版没有 `resolvedIPs`、允许源四个目标记录、`backendDirectDenied`、`validLoginCount: 2`、`tenantGRPCIsolation`、`tenantNamespaces` 或 `caSHA256` 的证据 JSON 需重新采集；这些字段分别记录解析目标、明文后端拒绝、两个有效登录检查、双租户直接 gRPC 隔离检查、对应的非敏感 namespace ID 及探针所用 CA 内容。允许源和拒绝源机器须提供内容相同的 CA 文件；离线核验主机不需持有该文件。若核验主机上声明路径的 CA 文件存在，配对器也会核对其内容与两侧探针证据一致，避免用已轮换的本地 CA 误验旧记录；文件不存在时只能核对两份记录彼此一致。探针运行期间 CA 内容改变会失败；CA 指纹不能证明代理正在使用的服务端证书未轮换。

2026-10-07 允许源声明复核：预检器现在拒绝包含未指定地址、loopback、链路本地、多播及 IPv4 映射 IPv6 地址的 `allowedSources`，同时继续接受明确的 IPv4/IPv6 工作负载网段。`python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 37/37、示例声明、`git diff --check`、`make all`（vet、全部 Go 包测试、构建）及 `make client-compatibility`（直连 9/9、本机双节点 TLS/L4 1/1）通过。此声明检查无法替代现场源 IP、NetworkPolicy 或代理后端连接日志；本次没有新增跨主机部署验收，原生订阅仍未正式支持生产拓扑。

`--verify-pair` 与 `--verify-cluster` 只接受采集于最近十分钟内、且不比核验主机时钟快超过一分钟的记录；两侧记录仍须处于同一五分钟窗口。请在探针结束后立即核验，并确认各执行主机时钟同步。旧记录可归档，但不能重复作为当前部署验收结果。

双节点核验还要求两个节点的允许源探针来自同一台 SDK 主机，拒绝源探针来自同一台拒绝主机；否则四份分别通过的记录不能证明同一个客户端可访问两个节点并完成故障切换。`probeHost` 是探针主机自报名称，须结合原始命令、源 IP 与代理/防火墙日志核对，不能仅凭该字段证明主机身份或双节点生命周期结果。

本轮增加有效登录门禁：允许源预检现在对每个 HTTP 入口解析 IP 用两份不同的 SDK token 文件各执行一次登录，并要求 HTTP 200；`--verify-pair` 拒绝缺少 `validLoginCount: 2` 的旧证据。该计数只表示登录成功，不证明两个 token 属于不同租户，也不替代双租户 gRPC 读取、订阅、推送和撤销验收。本机拓扑检查器 27 个单元测试及声明检查通过；尚无可用于运行这一新增预检的跨主机现场入口和凭证。

证据核验还要求字面 IP 目标（尤其是 `backendExternalHost` 的两个端口）的 `resolvedIPs` 与声明中的 IP 一致；两侧记录即使都写入同一个错误 IP，也会报 `ALERT`。DNS 名称目标仍按两侧解析结果一致性检查，实际代理连接目标要以现场日志复核。 本次 CA 指纹门禁的回归测试 `python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 为 28/28；`make all` 与 `make client-compatibility` 在现有 PostgreSQL 和测试自管 Redis 上通过。此结果没有新增现场跨主机证据。

`--evidence` 不记录凭证，先写入同目录临时文件，再原子创建最终文件；即使两个探针同时使用同一路径，也不会覆盖先到的证据，并会对后到的探针报 `ALERT`。每次运行仍须使用唯一文件名，避免失败后误用旧的 PASS 文件；证据目录须支持同目录硬链接。IPv6 后端目标在证据中使用 `[地址]:端口`；证据配对和双节点检查将 IPv4 地址与其 IPv4 映射 IPv6 写法视为同一地址，不能用两种写法冒充不同节点或不同 TLS 入口。路由源 IP 可能在 NAT 后改变，需另从防火墙/代理连接日志核对实际看到的源 IP。`resolvedIPs` 是探针前后的 DNS 解析快照；两次相同仍无法证明 TLS/HTTP 客户端连接时命中的 IP，需从代理连接日志核对实际目标 IP。**证据配对也不能单独证明代理配置或网络策略生效；服务停机时拒绝源探针会通过。** 必须在服务运行期间从允许源通过 `--probe` 并完成有效 SDK 登录、读和订阅；同一时间窗口从拒绝源运行 `--probe-denied`。记录两台执行主机、原始输出、退出码、策略与代理配置版本，以及代理实际观察到的源地址。应通过受控客户端实际发布/撤销验证推送停止和重连。两节点分别执行这些检查并保留证据后才可扩大正式支持声明。

在允许源和拒绝源各自执行相同的连通性探针，保留原始输出；将主机名和 CA 路径替换为部署值：

```sh
openssl s_client -connect nacos.example.internal:9848 -servername nacos.example.internal -CAfile /etc/golab/nacos-ca.pem -alpn h2 </dev/null
curl --cacert /etc/golab/nacos-ca.pem -sS -o /dev/null -w '%{http_code}\n' 'https://nacos.example.internal:8848/nacos/v1/cs/configs?tenant=x&group=GOLAB_FLAGS&dataId=golab.manifest'
```

允许源应通过证书校验并协商 `h2`，HTTP 绕过返回 404；拒绝源应无法建立 TLS/TCP 连接。还要从侧车外部对 18848/19848 做 TCP 连接测试，必须失败。由于本工作环境不能创建网络命名空间（`ip netns add` 返回 `Permission denied`），这些跨主机拒绝源检查本轮**没有实测证据**。

2026-10-07 复核：使用现有 PostgreSQL、独立 Redis Docker 测试实例、真实 GoLab 双进程和官方 Go SDK 运行上述定向用例，通过（28.30s）；同一环境的 `make all`（`go vet ./...`、`go test ./... -count=1`、`go build ./...` 及两个命令二进制构建）通过。部署预检的 7 个 Python 单元测试通过；示例域名未配置 DNS 时运行 `--probe-denied` 得到 `ALERT: ... Name or service not known`，按预期拒绝记录伪证据。Docker 可创建两个独立 bridge 网络：同网络 Alpine 客户端连接测试 TCP 服务成功，异网络客户端连接同一容器 IP 失败；这只确认隔离试验可搭建，**没有**让两个 GoLab 节点、TLS/L4 代理及 SDK 客户端在该拓扑上完成端到端验收。本机 `/data`（Docker 根目录）已满；该试验未拉取或构建新镜像。待部署环境按上文分别保存两节点的允许源和拒绝源原始输出、证书/代理/策略配置及有效凭证生命周期证据，才具备正式支持审查条件。

后续同日复核：部署预检新增路由源 IP 分类、DNS 所有地址的拒绝测试与五分钟证据配对门禁，10 个单元测试通过；真实双 GoLab 进程官方 SDK TLS/L4 定向用例通过（26.65s）。`make all` 使用现有 PostgreSQL 与测试自管 Redis Docker 容器完整通过；`make client-compatibility` 直连 9/9、本机 TLS/L4 双节点 1/1 通过，并保留 SDK 竞态及生产跨主机网络策略未验收的 LIMIT。若改用外部 Redis 地址，`TestClusterMultiReplica/ConvergenceRedisDown` 因无法执行 Redis 停机操作而跳过，其后续子测试会以未设置的 revision 0 失败；因此全量检查必须让测试管理 Redis 生命周期。新增预检尚未取得真实跨主机允许源/拒绝源记录，不能改变上表的正式支持结论。

本轮复核：部署预检新增双节点四份证据同窗口门禁，并将拒绝源的“无路由”结果判为探针失败，12 个 Python 单元测试通过，声明文件检查通过。`go vet ./...` 通过。此共享环境的 Docker 根目录 `/data` 无可用空间；`make all` 中新起的 PostgreSQL 容器无法完成初始化，`TestQuotaAdminReplicaRead` 与 `TestPostgresJourneyReportReplaysAcceptedEvents` 均在 90 秒后因 TCP 连接拒绝失败，故本轮**没有**全量测试通过的结论。`ip netns add golab-issue13-probe` 返回 `mkdir /run/netns failed: Permission denied`。既有本机 TLS/L4 双节点测试记录仍有效，但本轮没有新增跨主机双节点、真实入口网络策略与代理源 IP 日志，因此 Issue #13 继续开放。

同日新增预检复核：证据现在记录每个目标的 DNS 解析 IP；配对要求允许源与拒绝源解析一致，双节点要求两个 TLS 入口解析 IP 集合不重叠，拒绝源在路由检查与 TCP 探针之间的 DNS 变化会报错。`python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 13/13 通过，`GOWORK=off go test ./test/native -count=1` 通过（43.5 秒），`go vet ./...` 与 `go build ./...` 通过。`make all` 在当前主机仍因 `/data` Docker 根目录无可用空间导致新 PostgreSQL 容器未就绪，不能报告全量测试通过。`unshare -Urn true` 返回 `Operation not permitted`，`ip netns add` 返回 `/run/netns` 权限不足；没有新增跨主机、真实网络策略或代理日志证据，正式支持声明仍未扩大。

本轮部署预检加固：声明中的 TLS 入口若为 loopback、`localhost`、通配、链路本地或多播地址，预检立即失败；对应 Python 回归用例 13/13 通过，示例声明通过。`GOWORK=off go test ./test/native -count=1`、`make all` 与 `make client-compatibility` 均通过；兼容性矩阵为直连 9/9、本机 TLS/L4 双节点 1/1，通过时仍保留官方 SDK 竞态与跨主机部署未验收的 LIMIT。全量检查使用已有 PostgreSQL 管理 DSN，Redis 由测试自行管理。该结果仍只是本机验证，不补足跨主机网络策略、代理连接日志和真实多租户生命周期验收。

本次预检补强：HTTP 登录与 gRPC 的后端端口、TLS 入口端口都必须分别独立，即使主机名不同也不能共用端口；允许源 HTTP 探针遇到重定向会报警，避免将另一个站点的 404/401 误记为目标入口的拒绝结果。Python 回归用例 14/14 与示例声明检查通过；`make all` 使用已有 PostgreSQL 与测试自管 Redis 通过；`make client-compatibility` 的直连 9/9 与本机 TLS/L4 双节点 1/1 通过，限制项仍为 LIMIT。`/data`（Docker 根目录）仍无可用空间，`unshare -Urn true` 返回 `Operation not permitted`。没有新的跨主机源/目标、现场网络策略或代理连接日志；上述本机检查不改变 Issue #13 的生产支持状态。

同日预检追加允许源 DNS 漂移检查：完成 TLS/HTTP 探针后重解析两个入口，地址集合改变即报 `ALERT`，避免将探针前的解析快照直接当作探针后的证据。回归测试 15/15、示例声明检查、`make all` 和 `make client-compatibility` 均通过；兼容性门禁仍为直连 9/9、本机 TLS/L4 双节点 1/1，并保留跨主机与官方 SDK 竞态的 LIMIT。再次尝试 `unshare -Urn true` 被拒，现场代理实际连接目标仍须由连接日志核对，生产支持状态未变。

本次预检补强入口 DNS 答案：通配、链路本地、多播和 IPv4 映射 IPv6 的本机地址在探针及证据配对时均报 `ALERT`。回归测试 17/17、示例声明检查、`make all`（使用现有 PostgreSQL 与测试自管 Redis）和 `make client-compatibility`（直连 9/9、本机 TLS/L4 双节点 1/1）通过。构建时将 Go 临时目录移至有空间的 `/home`；`/data` 仍满。本次未取得生产跨主机允许源/拒绝源、网络策略或代理实际连接日志，正式支持状态不变。

复现本机 TLS/L4 实测：

2026-10-07 探针复核：允许源探针现在对入口 DNS 返回的每个 IP 逐个固定地址连接，同时以声明中的主机名执行 TLS SNI/SAN 校验及 HTTP Host 检查；任一地址未协商 `h2`、HTTP 绕过不是 404 或无效登录不是 401 都失败。Python 单元测试覆盖第二个地址返回重定向的失败路径；另用临时自签 CA、真实本机 TLS HTTP 与 gRPC 握手验证固定地址连接下的 SNI/SAN、Host、404/401 与 `h2`。这仍是本机探针自检，不提供跨主机 NetworkPolicy 或生产代理连接日志。

2026-10-07 明文后端绕过门禁复核：允许源在线探针新增对 `backendExternalHost` 的 HTTP/gRPC 后端端口 TCP 拒绝检查；任何一个端口可连都会使 `--probe` 报 `ALERT`。允许源证据新增 `backendDirectDenied`，旧证据不能通过配对核验。`python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'` 24/24、示例声明、`git diff --check`、`make all`（vet、全部 Go 包测试、构建）和 `make client-compatibility`（直连 9/9、本机 TLS/L4 双节点 1/1）通过。全量测试复用了已有 PostgreSQL 并由测试管理 Redis；新建 PostgreSQL 容器因 `/data` 满无法就绪。仍缺跨主机两节点、允许源/拒绝源与现场网络策略及代理日志，原生订阅不扩大生产正式支持声明。

```sh
GOWORK=off go test ./test/cluster -run '^TestOfficialNacosClientFailsOverAcrossGolabNodes$' -count=1 -v
GOWORK=off go test ./test/native -count=1
python3 -m unittest discover -s scripts -p 'test_check_nacos_native_topology.py'
```

`test/cluster` 需要可用 PostgreSQL 和 Redis；设置 `GOLAB_TEST_PG_DSN` / `GOLAB_TEST_REDIS_ADDR` 可使用现有服务，否则测试基座尝试 Docker。该测试中的 TLS 代理是 Go 实现的透明 TCP 转发，不是 HAProxy 生产配置的实测。测试验证有效登录、文档 hash、两个租户隔离、共享连接上 B 读取自己配置成功但订阅自己配置被拒、独立连接订阅成功、订阅收到更新、失效后的无推送、TLS 证书、HTTP 404/401、双节点切换；还在同一条 TLS HTTP/2 连接上直接发送原生配置查询，确认两个租户各自的有效凭证 gRPC status 0、跨租户凭证 status 7、无凭证及撤销后的凭证 status 16，并确认撤销一个租户后另一个租户仍可读取自己的配置且不能读取被撤销租户的配置。这个直读绕过官方 SDK 的本地缓存；官方 SDK 的 `GetConfig` 在远端读取失败时可能返回 last-known-good，不能以其返回旧值证明凭证有效。排查撤销请看原生登录是否 401、直连 gRPC 是否拒绝、服务端授权日志及新 revision 推送是否停止；排查重连请核对 gRPC 端口和 h2 ALPN、证书 SAN/CA、两节点地址、后端一对一连接以及最新 manifest revision/文档 hash。

## 试验协议

| 项目 | 当前映射 |
| --- | --- |
| 客户端 | 官方 `nacos-sdk-go/v2 v2.3.5`；其他版本、Java/Python 客户端未验证 |
| 连接 | gonacos HTTP 端口及其独立 gRPC 端口；默认仅 loopback；鉴权模式已在直连 TCP 与本机 TLS/L4 双节点实测，L7 HTTP/2 后端连接复用不在支持范围 |
| 认证 | `-nacos-native-auth` 模式用 `golab-sdk` + GoLab SDK token 登录；每次读/订阅/推送复核 workspace/environment，默认模式不启用此接口 |
| 命名空间 | 每个 `(workspace, environment)` 使用平台生成的独立 namespace ID，不能用用户输入的名字推导 |
| Group | 固定 `GOLAB_FLAGS` |
| 订阅入口 | `golab.manifest`；`schemaVersion=1`，`revision` 单调递增，`flags` / `segments` 条目给出 `key`、`revision`、`dataId`、SHA-256 `hash` 和可选 `deleted` tombstone |
| 不可变文档 | `golab.flag.<key>.r<revision>`、`golab.segment.<key>.r<revision>`，JSON 正文字节的 SHA-256 必须匹配 manifest |

启动 GoLab 时加 `-nacos-native-auth=true`，为目标 workspace/environment 创建 `kind=sdk` 的 GoLab token。官方 Go 客户端设置 `constant.WithUsername("golab-sdk")`、`constant.WithPassword(sdkToken)` 和该环境的 `constant.WithNamespaceId(namespaceID)`，同时配置 GoLab 的 gonacos HTTP 与 gRPC 端口。不要将 SDK token 或原生端口交给浏览器；跨主机部署需先完成独立的 TLS 和网络策略验收。复现已验证范围：`GOWORK=off go test ./test/native -count=1` 与配置真实 PostgreSQL/Redis 后的 `GOWORK=off go test ./test/cluster -run TestOfficialNacosClientFailsOverAcrossGolabNodes -count=1`。

收到通知后只能把它视为失效提示：重新 `GetConfig(golab.manifest)`，按该 manifest 获取全部活动文档，逐一校验 hash、key 和 schema，再通过 GoLab evaluator 编译。候选 revision 高于当前 revision 且全部成功，才一次性换入。任何读取、hash、JSON 或编译失败都保留 last-known-good，记录最新错误及生效 revision；超时未复核时应按调用方设定的最大陈旧期降级。连接恢复和定期轮询都要重新读取 manifest，不能仅等推送。测试中的试验代码展示了取数和校验，**并未作为新的可复用 SDK 发布**。

## 与正式 GoLab SDK 的差异

GoLab Go SDK 使用受 GoLab token 约束的 `/api/v1/sdk/config` 全量快照、ETag/304、SSE 失效提示及定期轮询；服务端从凭证导出 scope，客户端不能任意指定 namespace。它已提供原子换入、last-known-good、`Status()`（revision、陈旧状态、错误）与最大陈旧期处理，并支持曝光上报。原生 Nacos 客户端只提供配置读取和通知；原生协议本身不提供这些 GoLab 语义，租户授权仅在显式 `-nacos-native-auth` 模式由 GoLab 附加。浏览器、Python/TypeScript SDK 继续走 GoLab 远程求值接口。

当前兼容验证覆盖直连 TCP 与本机 TLS/L4 的单节点或双节点、跨租户拒绝和撤销；网络黑洞、乱序、缺文档及无部分快照分别由对应原生用例验证。`test/native` 的默认模式越权特征用例继续约束未授权底座的边界，安全模式负向用例约束可支持路径。若要扩大到 L7 代理或将原生订阅列为通用正式接口，还需验证后端连接不跨客户端复用、TLS 与网络策略，并处理官方 SDK 的竞态限制。
