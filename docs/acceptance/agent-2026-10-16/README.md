# Agent 实施检查点

本目录目前记录阶段性实现，**不是完整验收通过证据**。A01—A28 均未满足全部断言，因此在 `results.json` 中保留 SKIP。P0—P6 退出门槛尚未完成；API 可显式注入 Agent handler，App/cmd 尚未开放启用入口。

首个用户要求的基线提交及推送已完成：`41b16d71e30cd75c99225e8288c22a6b184dcf53`，远端 `origin/main` 已核对一致。基线包含原有 40 个文件的工作区改动（2,900 增、298 删）；`git show --stat 41b16d7` 可复核。gogw 工作区干净，SHA 见 manifest。

已实现显式 oneof 消息目录、生成脚本、严格信封 codec、竞态/模糊测试、64 位 JSON 精度测试、授权矩阵 ADR、预算 profile 和 CI 契约测试入口。未知可选字段允许，未知 payload 和重复 payload 拒绝。codec 不执行领域授权或构造就绪状态。

复现：

```sh
make generate-agent check-agent-generated test-agent-contract
GOWORK=off go test ./api/agent/v1 -run '^$' -fuzz '^FuzzDecode$' -fuzztime=10s
GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test -short ./ ./cmd/... ./internal/... ./pkg/... ./web
GOWORK=off go vet ./...
make sdk-test license-check release-check
git diff --check
```

后续必须按主方案继续 Naming/WSS POC、共享存储/CAS、机器权限、Go 客户端、签名配置/缓存、注册租约、发布闭环、真实 gogw 和多副本/性能验收。`manifest.json` 中空缺或未冻结项必须补证据；不允许从现有绿色检查推导运行时或生产验收通过。完整实施验收后才进行用户要求的最终提交与推送。

本轮新增签名资源校验与控制面签发（标准 Ed25519）、严格二进制 claims 编码、资源高水位比较、信任根窗口及轮换测试。Python 签名/Go 验证夹具通过；claims 模糊测试执行 398,854 次。对应资源校验竞态检查及 vet 通过。它们尚未接入持久缓存或运行时，不证明完整 A05/A07/A26。

内置 Naming POC 在真实 `Server.Services().Naming` 上完成注册、查询、订阅记录、续租、注销；可控时钟证明原生租约 15 秒、过期只变 unhealthy 且可被普通心跳复活。该 API 没有 Agent fencing，Grayling 必须自行维护共享治理状态。详见 `naming-poc.jsonl`；外部 Naming 与真实订阅推送尚未验收。

```sh
make test-agent-resources test-agent-naming-poc
python3 scripts/agent_signature_fixture.py --check
GOWORK=off go test ./pkg/agent -run '^$' -fuzz '^FuzzClaims$' -fuzztime=10s -parallel=4
```

新增共享会话与真实 WSS 检查点：PostgreSQL 事务保存凭证摘要、boot 授权、准入与连接代次；跨节点重连、旧连接拒绝、授权 boot 接管、撤销、容量和重开测试通过。API 可选注入 WSS，机器身份与 dev 登录隔离；Go 会话客户端已完成真实 TLS 连接及心跳验证。完整命令 `make test-agent-channel`，记录见 `sessions-wss.txt`、`client-wss-faults.txt`、`channel-final.txt`。该会话检查点当时只声明 session 能力；后续策略资源和 Go SDK 对接进展见下文。

原有短回归（显式 PG SKIP）见 `channel-regression.txt`；Agent 的真实 PG/WSS 套件独立执行，未用短回归代替。源码、协议和依赖版本仍未最终提交，首次提交/推送基线保持不变。A01—A28 的完整状态仍为 SKIP，不能将当前子集提升为整体 PASS。

P2 策略链路已接入组件集成：完整快照验证编译 → 主库 operation 去重/CAS → 签名不可变资源 → 另一节点 WSS 订阅/通知 → HTTPS 会话鉴权下载 → 验签与本地 evaluator。真实 PostgreSQL 中验证了并发去重、过期 CAS、完整 uint64 版本、重开、scope/权限拒绝；WSS 验证了全量恢复、300 KiB 策略引用下载、引用/签名版本一致性、旧会话下载拒绝和 JSON 返回值不污染原视图。SDK 同时检查握手/心跳时钟预算。测试见 `policy-and-channel.jsonl`、`policy-unit-race.txt`，复现 `make test-agent-policy`。

本轮复用了工作区新增的 Go SDK 提交 `805a696`（并发请求、事件分发、重连），并与服务端组件对接；它不改变初始已推送基线为 `41b16d7` 的记录。App/cmd 启用及管理发布源尚未接入，可信缓存与高水位原子持久化仍待实现；尚无实例租约、发布编排或真实 gogw 闭环。A01—A28 仍不计整体 PASS。

当前统一使用 805a696 的 Go SDK，早期重复客户端骨架已移除。策略集成原始记录无 SKIP/FAIL；仍按完整 A01—A28 的全部断言保留整体 SKIP。App/cmd、可信缓存及完整 RuntimeView、注册/租约、发布编排与 gogw 是后续必做项。

P2 新增可信缓存组件：显式初始化、HMAC 身份绑定、SQLite 原子资源/水位、单所有者文件锁、不可变 PolicyView、持久撤销、信任根轮换与时钟回退保护。`policy-cache-race.txt` 记录组件竞态测试；`policy-cache-wss.txt` 记录真实 PostgreSQL/WSS 下载后落盘、重开、本地求值和旧策略拒绝的集成测试。复现：

```sh
go test -race -count=1 ./api/agent/v1 ./internal/agent ./pkg/agent
go test -tags=integration -race -count=1 -timeout=3m ./test/agent -run '^TestPolicyWSSCrossReplicaAndLargeDownload$'
```

该检查点更新此前“可信缓存待实现”的状态，但不代表完整 A06/A07 已通过。非正常断电测试、独立恢复授权、可信密钥部署、WSS 撤销接线及完整 RuntimeView 仍待完成；整份有效旧缓存的离线回退检测需要目录外的独立水位。其余 P0—P6 退出门槛和最终提交/推送保持待完成。

独立恢复授权组件已补齐：GRA1 + 独立 Ed25519 trust ring 精确绑定前置水位、目标策略和短期 nonce 授权，原子持久化新 epoch/策略/授权摘要；重试不续新鲜度且不能抹去撤销。测试覆盖普通策略密钥冒用、伪造签名、目标替换、过期、前置水位变化、普通更新跨 epoch 拒绝、写入失败保持旧 epoch、重开幂等，以及普通安装/恢复安装提交后不调用 Close 直接退出的 WAL 恢复。它证明进程退出恢复，不证明物理断电持久性。

复现 `make test-agent-recovery`（Python fixture check 需要 cryptography 41.0.7）。`policy-recovery-race.txt` 含全部 Agent SDK/签名/缓存竞态测试；`policy-recovery-integration.txt` 含真实 PG/WSS 策略回归；`policy-recovery-fuzz.txt` 含恢复编码模糊测试。独立 Python fixture 通过 Go 缓存安装和求值路径验证。管理端恢复审批、epoch 提升与分发、完整运行视图及 A01—A28 其余断言仍待完成，整体状态保持 SKIP。

恢复组件接入后的仓库短回归见 `policy-recovery-regression.txt`，显式设置 `GRAYLING_TEST_PG_SKIP=1`；该记录只用于现有功能回归。真实数据库证据使用单独运行的 `policy-recovery-integration.txt`，未用跳过数据库的短回归替代。相关包 vet 及 `git diff --check` 通过。

App/CLI 启用入口已接通，更新此前“App/cmd 未开放”的检查点状态。实验通道默认关闭，启用需 PG、显式 Host/连接上限及 TLS；CLI 在打开存储前检查证书配置。`agent-app-unit.txt` 记录配置拒绝与 TLS 测试；`agent-app-wss.txt` 记录真实 App/PG/WSS/Go SDK 的关闭、重启代次延续、机器凭证隔离及默认关闭测试；`agent-app-cli.txt` 记录实际编译 CLI 的帮助与无 TLS 启动拒绝（未写状态）。复现 `make test-agent-app`。

凭证/配额管理端、批准策略源、Agent epoch 恢复门禁仍待接线；当前拒绝 `AgentEnabled` 与现有 RecoveryPoint 同时配置。其余完整注册租约、发布及 gogw 验收未被本次传输接线替代。A01—A28 仍保留整体 SKIP。

本次全部真实 Agent 集成回归通过（`agent-app-integration.txt`，54.584 秒），仓库短回归通过（`agent-app-regression.txt`，显式 PG SKIP）。App 接线使此前仅测试/库中使用的 backoff、websocket 和 protobuf 进入发行二进制依赖，已更新许可清单与 notices；按实际 LICENSE 文本补齐 ISC 识别及防误识别测试。`agent-app-checks.txt` 记录 license-check、release-check 和重复协议生成检查通过，相关包 vet 通过。

机器凭证与配额管理入口已补齐，更新前一检查点“管理端待接线”的部分状态：App 启用时开放独立 owner 权限的 TLS API，含签发/列表/撤销、配额 CAS 和审计分页。operationId 去重且仅首次返回密文，所有业务变更与 Agent 审计同事务。`agent-admin-transactions.txt` 为仓储首轮测试；`agent-admin-integration.txt` 为后续包含真实 App 发证→WSS→撤销、RBAC/越权、JSON 严格解码、审计失败回滚、v2→v3 迁移和分页的集成记录。复现 `make test-agent-admin`。库/现有 API 竞态测试见 `agent-admin-unit.txt`。

Agent 关闭时保留旧 capabilities 响应及路由行为；新增管理界面尚未实现，已在 `docs/web-management-coverage.md` 明确标记“需补齐”，没有以 API 测试冒充浏览器验收。批准策略源、epoch 恢复管理流程、完整实例租约/发布/gogw 等仍待实施，A01—A28 继续整体 SKIP。

管理迁移后的全部当前 Agent 组件集成套件通过（`agent-admin-full-integration.jsonl`，92.867 秒，无 SKIP/FAIL）；该套件仍未覆盖实施方案的全部验收条目。复核确认管理去重结果无后台清理，当前 24 小时过期仅拒绝复用，不代表物理清理；长期存储保留和有界清理仍是 P6 待完成项，不能据此宣称运维或压力验收已通过。配额 CAS 目前比较连接数值，不是发布 generation；完整发布状态机必须另用其权威代次。

实例治理增量及限制见 [instance-registration-status.md](instance-registration-status.md)。真实 Naming 的 revision 隔离、延迟旧写入清理与稳定空集合刷新已有组件测试；注册变更消息已接入 WSS，登记策略与部署授权管理 API 已接通，嵌入式 App 对账已接通，签名发现订阅已接通，可信发现缓存组件现已完成本轮测试，外部 Naming 仍待完成；完整 P3 与 A01–A28 仍未完成。


发现可信缓存增量：复用封印 SQLite 事务引擎，提供独立文件与封印域的 DiscoveryCache、双版本水位、不可续期的重复资源安装、动态视图守卫及独立恢复根授权。真实 Naming → WSS/HTTPS 下载 → 缓存安装 → 关闭重开 → 离线读取测试通过（`discovery-cache-wss.txt`）。`discovery-cache-regression.txt` 记录完整 SDK 竞态测试通过，包含进程退出后的 WAL 恢复和空缓存跨封印域替换拒绝；`discovery-cache-vet.txt` 对相关包检查通过。该增量不代表自动应用驱动、RuntimeView、Prepared 或整体 A01—A28 已完成，宿主机断电验收仍待进行。


命令期限修复：鉴权与资源/注册存储调用共享派发时固定的两秒上限，客户端短期限会传入 PostgreSQL 等待；超时不会单独关闭有效会话。Subscribe 首次读取失败时不建立订阅，鉴权阶段的期限错误保留 subscriptionId。真实 PostgreSQL 表锁测试三次通过（`command-deadline.txt`），覆盖 Fetch、Subscribe、Heartbeat 的取消、失败订阅换 key 重试与序号、超时后继续心跳。初版测试遗漏取消订阅处理屏障，已增加同连接心跳确认，避免将取消订阅超时误认作后续请求错误。`command-deadline-registration.txt` 记录三项注册 WSS 回归通过；`command-deadline-unit.txt`、`command-deadline-vet.txt` 记录领域包竞态测试与相关包 vet。完整 RuntimeView/Prepared 和整体退出门槛仍未完成。


发布 Bundle 本地组件：新增 `MarshalBundle`、`VerifyBundle` 和精确策略依赖校验，BundleHash 绑定规范 payload、策略 epoch/revision/摘要、string 路由值映射、build 镜像/资源摘要、协议和授权硬截止。文档深拷贝与原始签名资源私有副本防止调用方修改已验证状态；依赖校验使用当前信任根重新验证 Bundle，不能借新策略根延用已退役 Bundle 根。`bundle-tests.txt` 覆盖有效全链、uint64 边界、schema/默认行为/资源/路由映射负例、非规范字节、跨 scope/epoch/Bundle 和截止时间；完整 SDK 竞态回归及 vet 分别见 `bundle-regression.txt`、`bundle-vet.txt`。批准配置源、事务化 Bundle/策略发布与分发缓存、Prepared 和 RuntimeView 接线尚未完成，整体验收仍为 SKIP。

已发布配置源增量：新增内部 `LoadPublishedSnapshotContext`，在发布锁内固定平台 revision、检查底座与平台头一致性，逐份独立计算摘要并完整编译后返回确定性快照。草稿、跨 scope/slugs、重复条目、底座领先/内容漂移、伪造返回 hash、缺失文档与缺失分群依赖均拒绝；已批准删除会从完整集合移除。`published-source-tests.txt` 记录定向竞态测试，`published-source-regression.txt` 记录含分群依赖的完整 admin 竞态回归。真实 PostgreSQL 双平台 Store 的批准发布 → 副本读取 → Agent 策略签发 → WSS/HTTPS → Bundle 精确依赖校验及本地求值通过（`published-source-wss.txt`）；相关包 vet 通过（`published-source-vet.txt`）。测试显式编排这些内部组件，并未证明尚未实现的管理 HTTP 发布入口、Bundle 审批、事务化成对签发或自动调度。复用发布锁/后端重载的及时取消限制仍待处理；完整 RuntimeView、Prepared、gogw 与 A01—A28 仍未完成。

Bundle/策略事务增量：新增内部 `PublishBundle`，按 operation → policy head → Bundle head → schema 锁序同事务写两份资源、推进头和幂等结果；共用数据库 epoch/时间，期限被 Bundle 硬截止裁剪。独立操作摘要域与内部 `bundle/` 存储键避免旧策略入口碰撞；关键写入检查实际影响行数，提交前重新检查过期。真实 PostgreSQL 测试覆盖双副本并发精确去重、CAS/跨发布器操作冲突、操作记录过期后的旧请求拒绝、第二资源写入失败的完整回滚、非法依赖/epoch/期限不留状态、内部键隔离。已发布配置源集成改用实际成对发布器，再通过 WSS/HTTPS 取策略并进行 Bundle 依赖求值。详细原始结果见 `bundle-pair-integration.txt`；SDK/Agent 领域竞态回归与 vet 分别见 `bundle-pair-unit.txt`、`bundle-pair-vet.txt`。管理审批、Bundle 网络下载/缓存、Prepare/Activate、完整 RuntimeView/gogw 与整体 A01—A28 仍待完成，不能从本组件推导发布成功。

成对发布补充边界：发布器允许的最短一秒 lifetime 能成功签发，Bundle 与策略保持同一截止；`bundle-pair-lifetime.txt` 记录该真实 PostgreSQL 边界测试通过。

Bundle 固定版本下载增量：新增 ConfigRead 鉴权的 BundleResource，与 WSS Fetch(kind=3) 和 HTTPS `/resources/3/...` 共用。只读精确正版本，检查签名 claims 身份/内容 Hash/当前 epoch，返回前重验 session 与期限；不要求本节点有签名私钥。SDK DownloadBundle 将同源有界下载与 Bundle 验签及引用元数据绑定。ResourceRef 截止裁剪到 reader 凭证期限，保留原签名资源字节。`bundle-download-integration.txt` 覆盖真实跨副本获取、SDK 验签、同公共键的 policy 隔离、短凭证引用、旧会话 HTTPS fencing、未知/外 scope 一致错误、旧控制面 epoch 拒绝及缺少 ConfigRead 的权限拒绝。`bundle-download-regression.txt` 覆盖原策略/发现交付与已发布源回归；`bundle-download-unit.txt`、`bundle-download-vet.txt` 记录相关领域/SDK/API 竞态与 vet。新增复现目标 `make test-agent-bundle`，其中已发布源测试进一步从网络下载实际成对发布的 Bundle 和策略，再做精确依赖求值；尚未接通 Bundle 缓存、管理审批、Prepared/RuntimeView 和 gogw，整体状态保持不完整。

Bundle 可信缓存增量：新增固定 Hash 的 BundleCache/BundleView，独立文件及 GBC1 封印域；复用原子资源/水位提交并保持策略/发现缓存兼容。完全重放不刷新 verifiedMs，同版本签名资源字节变化拒绝，更高 artifact 才更新验证时间；Document 深拷贝及 ValidatePolicy 精确校验均保留动态守卫。定向用例覆盖外部水位、旧版本/跨 epoch、空数据库跨 kind 替换、文件锁、写入抑制、撤销持久化、根轮换/退役与时钟回退；子进程退出后重开保留 WAL 中 Bundle 和水位（不等同于宿主机断电）。`bundle-cache-tests.txt` 记录定向测试，`bundle-cache-wss.txt` 记录实际下载 Bundle 安装/关闭重开及网络策略的持久视图依赖校验；最终当前代码的完整 SDK/admin/Agent 与 Bundle 集成回归见 `bundle-cache-regression.txt`（make test-agent-bundle），vet 见 `bundle-cache-vet.txt`。Bundle 不提供原地跨 epoch recovery；服务端周期性 artifact 刷新、管理审批、自动资源应用、Prepared、RuntimeView、gogw 和完整 A01—A28 均仍待完成。

Bundle 刷新增量：新增内部 RefreshBundle，固定原 payload/BundleHash 并分配更高 artifact。新签发使用部署可信 keyring 验证仍有效的原 Bundle、精确 policy 依赖及新签发根；新期限被原策略签名期限、Bundle 硬截止和 issuer lifetime 共同约束。operation → Bundle head → schema 锁序下，新资源、head 和幂等结果同事务写入，重放不重签、旧版本不改写。`bundle-refresh-tests.txt` 记录真实 PostgreSQL 双副本去重/CAS、旧操作清理后拒绝、24 小时 issuer 不能延长原五分钟 policy 授权、密钥轮换、body 与 wire hash 同改但签名无效时拒绝、缺失 scope/旧 epoch/策略过期/插入失败不留新状态，以及 WSS/HTTPS 新 artifact 更新缓存而旧视图仍过期。`bundle-refresh-regression.txt` 记录完整 make test-agent-bundle 通过，`bundle-refresh-vet.txt` 记录相关包 vet。自动刷新调度、审批/撤销门禁及完整发布/RuntimeView/gogw 接线仍待完成；整体结果保持不完整。


定向准备证据检查点：数据库 v7 保存定向意图、目标 CAS 和不可变 Prepared 回执。
意图与当前发布、boot、credential/principal/agent、lease ID/epoch 绑定；读取待投递快照和
接受回报均重新核对实时资格及精确签名资源。回执与计数同事务，重放不刷新期限。
真实 PostgreSQL 竞态测试覆盖跨副本并发、替换 fencing、租约轮换、Draining、旧连接、
显式回报权限、身份/时间边界、过期意图、写入抑制回滚、Abort 和 v6→v7 迁移。
`make test-agent-prepared`、`make test-agent-release`、`make test-agent-bundle` 与相关 vet
通过，原始结果见 prepared-*.txt。测试曾发现并修复旧命令错误分类顺序和测试 SQL 参数类型。
目前尚无 WSS Prepare 投递或 SDK 回报，也没有 NACK/容量门槛和状态推进；A16 保持 SKIP，
完整实施验收及最终提交推送仍未完成。


Prepare 网络检查点：App/Channel 的独立 ReleaseVerifier 配置与凭据权限共同控制
release.prepare，Hello 必须显式声明能力。WSS 定时重新核对共享定向意图，重连可跨副本
恢复同一 command ID；HTTPS 下载固定 Bundle artifact。新增字段 69 PreparedAck 仅确认
证据持久化，SDK ReportPrepared 校验关联关系及回执边界。真实 WSS 测试覆盖重投、精确
下载、重放不刷新 TTL、旧连接、改变内容冲突、缺信任配置和未协商拒绝；锁阻塞测试发现并
修复 SDK 覆盖短信封 deadline 的问题，修复后验证超时回滚且会话可重试。
make test-agent-prepared、make test-agent-channel、协议生成/竞态、SDK 竞态、相关 vet 和
短回归通过，原始记录见 prepared-wire-*.txt。仍缺外部批准容量意图、NACK/容量门槛、
Applied、激活与探测；A01—A28 保持完整范围 SKIP，最终提交推送仍须等待全部实施验收。


准备容量决策检查点：v8 增加互斥 NACK 证据和 PREPARED 决策快照。新增能力
release.prepare.reject 与字段 74 PrepareRejectedAck 明确协商，旧准备客户端不会收到
无法解析的新负向确认。正负回报共用单调 catalog revision，同命令不能变更结果。
可信内部 EvaluatePreparation 在凭据/admission/scope/release 屏障下重新校验全部当前目标，
每 build/AZ 独立检查容量，并以 (failed+pending)/targets 的失败率上界避免 pending 稀释。
状态、事件、决策和操作回执原子提交，提交前复查时间/期限；历史结果不授权激活。
真实 PostgreSQL/WSS 覆盖并发去重与正负竞争、缺 AZ/build、49.99%/50% 边界、过期、
boot 接管、Ready=false 的 NACK、写入抑制/授权耗尽回滚、v7→v8 和旧版本兼容迁移。
make test-agent-preparation、test-agent-release、test-agent-bundle、协议契约、相关 vet 与
短 API 回归通过，原始结果见 preparation-*.txt。仍缺外部批准容量意图、Activate/Applied、
部分成功恢复与 ACTIVE 探测；A16 及整体 A01—A28 保持完整范围 SKIP，最终提交推送待完成。


激活意图检查点：v9 保存同事务 ACTIVATING 状态、签名 kind5 activation-view、私有资源
head/精确引用、明确来源命令和操作回执。激活时重新检查所有当前准备容量、签名和期限，
不能使用历史 PREPARED 直接授权；来源逐一绑定有效 Prepared、credential、boot 和 lease。
kind5 快照冻结本次有效成员与精确资源/成员/证据版本，只向指定来源提供精确 Fetch 和
同源 HTTPS 下载。SDK 校验规范 schema、签名、完整 uint64、来源绑定、集合不可变性和
精确 Bundle/build/image/protocol 连接；下载不产生 Applied。
真实 PostgreSQL/WSS 测试验证副本并发重放、冷句柄、旧证据拒绝、来源权限、其他实例拒绝、
boot 替换后下载拒绝、未信任签发器、写入抑制和授权期限导致的完整回滚，以及 v8→v9
保留会话/租约/准备证据。开发测试曾发现精确 loadResource 需要 resource head，现已将其
插入与影响行数检查纳入事务并覆盖回滚。激活核心、准备/发布迁移回归、协议契约、vet 与
短 API 回归通过，日志见 activation-*.txt。尚无 Activate 投递、Applied、RuntimeView 安装
和业务探测；撤销 epoch 仍为初始 0。A16 和全部验收门槛未完成，不进行最终提交推送。


Activate/Applied 传输检查点：v10 新增独立回报 catalog 与同源命令正负互斥表。能力
release.activate 显式协商后投递带精确 kind5 引用/版本的 Activate，跨副本恢复同一命令。
Applied/Rejected 绑定当前源身份、boot/lease 和所有视图版本；负回报允许 Ready=false。
报告、不可变字段75 ACK、report catalog 与 VERIFYING revision/event 同事务，任何回报
都不直接进入 ACTIVE。历史诊断状态按源分页，双 revision 固定续页，有变更要求重取。
真实 PostgreSQL/WSS 测试验证投递/重连/未知响应恢复、正负竞争、错误版本、未协商拒绝、
部分成功、分页重同步、短 deadline、抑制写入回滚、迁移保留激活意图、未知字段重放、
过期历史、计数上限、跨 scope 与旧租约拒绝。ACTIVE 回放分支仅通过测试 SQL 注入状态
检查，不构成真实探测或 ACTIVE 验收。测试源手工上报版本，未安装实际 RuntimeView。
日志见 applied-*.txt；Applied、激活、发布、Bundle、SDK、协议、vet 和 API 短回归通过。
持续授权刷新、实际 RuntimeView、VERIFYING 超时/恢复、主动撤销、探测/ACTIVE 和真实 gogw
仍待实现；A16 及全部验收保持完整范围 SKIP，最终提交推送仍未完成。

撤销证明刷新组件已增加独立 artifactRevision：内容 epoch 仅随拒绝/取消集合变化，续签保持
原始 payload，避免使有效激活视图无故失配。SDK 封存证明版本和精确字节摘要，目录外
MinimumRevocation 检测同 epoch 的旧证明整库回放；续签不更新 Runtime 的首次验证时间。
App 默认关闭的 AgentRevocationRefresh 已接入，最多四任务、每页 100 scope、整页两秒
预算及前缀游标推进。真实双副本验证去重，锁住前部 scope 后后部仍能刷新，App 关闭等待
工作器退出；未登记 scope 同样扫描。历史精确下载仅允许当前同内容且尚未过期的证明，
证明续签与下载并发不会替换引用字节，内容变化则拒绝旧引用。

证据见 proof-refresh-*.txt；完整撤销集成测试 125.425 秒通过，最终历史下载修复另跑真实
WSS/HTTPS 回归。SDK 竞态、API 短测、vet 和契约检查通过。此前“生产周期调度待完成”
仅对该拒绝证明工作器更新；正向授权续签、CLI 配置、根轮换、保留扩容、实际 gogw、
探测/ACTIVE、网络隔离和规模验收仍未完成。results.complete 保持 false，A01—A28 未改为通过。

持续激活授权的前置版本绑定已补齐：Rejected 新增 activationArtifactRevision，激活回报
缺少版本时写入前拒绝，SDK 在发送前拒绝；Prepare 拒绝保持独立路径。Applied 和激活
Rejected 都按声明的精确签名版本核验，移除回报入口硬编码的版本 1。真实 PostgreSQL
测试证明 0、未签发版本和 uint64 最大值均不能消耗当前命令，随后正确版本可提交并跨
副本重放；真实 WSS 覆盖错误版本拒绝后正确负回报仍可成功。证据为 rejection-artifact-*.txt，
含 SDK/服务端竞态、生成一致性及 vet。数据库仍 v10；按激活版本保存命令/结果、持续
重核验签发、续租和探测/ACTIVE 仍待完成，未把这个前置改动记为持续授权完成。

数据库已推进至 v11：按 artifact 保留不可变激活版本、来源命令和报告；当前指针显式
保存在 agent_activations。报告复合外键同时绑定完整 scope/generation/artifact/实例/命令，
不能拼接不同版本的来源。Pending 仅查询当前版本，取得屏障后重查指针；状态集合按当前
artifact 过滤，分页需要同时固定 artifact/report/release 三个版本。历史资源按原版本下载，
已存在的精确回执在期限仍有效时可重放，新报告不得写入旧版本。

activation-v11-*.txt 记录真实数据库迁移/回归、两版签名 fixture 隔离、外键拒绝拼接、
并发副本升级、写入抑制回滚、SDK 混合版本拒绝、API 与生成检查。迁移保留原报告、摘要、
ACK、资源字节。旧 v10 NACK 缺少版本时，仅可精确重放已保存的报告，不能新增；补测含
真实 WSS 的 ReplayLegacyActivationRejected。第二版是未延长期限的测试 fixture，生产
尚无 RefreshActivation，也没有 Prepared 重新准备、持续续签或探测/ACTIVE 闭环；本轮
只证明版本化基础，不替代 A16/完整验收。最终提交推送继续待完整实施与验收后执行。

正式 RefreshActivation 组件已补齐，更新上一检查点“尚无 RefreshActivation”的状态：
release/artifact 双 CAS、独立 operation 摘要域、初版签名冻结源集合、初版 SQL 交叉核对、
当前凭证/租约/Prepared/容量/资源/根/撤销重新验证。新版资源、版本、source commands、
pins、双 head、事件及幂等结果原子提交，刷新回 ACTIVATING，不能复用旧 Applied。
旧投递证明到期后可以重新授权，但新签名仍受当前有效证据/资源/批准期限与 60 秒上限约束，
不延长旧 Prepared 或 lease。

activation-refresh-*.txt 记录真实 PostgreSQL、双副本稳定重放/双 CAS、源集合变更拒绝、
实际 WSS/HTTPS 下载/Runtime 处理器替换/新版 Applied、旧捕获与新请求版本一致、重复
安装不重建、签名及 wire hash 同改但验签失败、未受信签发根、撤销后禁止续签、审批/租约
失效、逐项写入抑制和锁等待期限取消后完整回滚。广泛 activation/Applied/Runtime WSS
回归 145.617 秒通过，SDK/协议/服务端竞态及短 API 回归通过。

现有 Prepare 命令期限和证据有效期仍限制长期刷新，重新 Prepare、自动正向续签调度、
管理审批/API/UI、探测/ACTIVE 和真实 gogw 尚未完成。本轮证明显式重新授权签发链路，
不宣称持续 60 秒探测门槛或完整 A16/A17；results.complete=false，最终提交推送待完成。

Prepare/Prepared 期限分离及显式重新准备已完成组件回归：命令最多 120 秒，首次回报必须
在命令期限与实时租约内接收；正证据最多 300 秒并受报告/credential/资源约束，不再被
原命令或首次 lease 截断。证据重放保持首次 revision/时间。容量始终重新校验当前
lease/boot/credential/Ready/Draining/Naming，签名激活成员仍裁到实时 lease。正证据可晚于
命令到期，但旧 lease/boot、新 target 或实时资格失败均不能凭该证据继续授予新激活。

QueueReprepare 在激活阶段采用独立操作域、目标 CAS 和新的最多 120 秒期限，可选批准
截止进入含存在性标记的请求摘要；重试不续期。新 target pending/NACK 时不能复用旧
positive。已有 WSS 可完成 NACK→明确新 target→正 Prepared→新版激活，仍不自动 ACTIVE。
初始 PREPARING/PREPARED 的原窗口保持约束，激活后重新计算则以资源/证据/实时租约为界。

证据 reprepare-*.txt：最终 Prepare/容量/重新准备回归 132.027 秒通过，激活/Applied/
Runtime WSS 回归 148.597 秒通过，存储报告时间篡改、SDK 回执期限边界、竞态、短 API 和
vet 通过。默认 120/300 常量有独立断言；跨初始窗口测试缩短了 fixture 窗口并等待真实时间，
不替代默认期限的长期运行、网络隔离或规模验收。Make test-agent-preparation 已包括
重新准备用例。自动调度、实际 Agent 资源准备驱动、管理审批/API/UI、探测/ACTIVE、
真实 gogw 与完整 A01—A28 仍未完成；results.complete=false，最终提交推送继续待完成。

CLI 签名/信任根/拒绝证明维护入口已接线，更新此前“CLI 配置待完成”的对应状态：
-agent-signing-key-id、-agent-signing-key-file（owner-only 单个 PKCS8 Ed25519 PEM）、
-agent-trust-roots-file（独立 schema1 JSON，1—32 根）、-agent-revocation-refresh（默认关闭）。
不从私钥自动导出信任；签名根必须匹配且当前有效，文件/JSON/算法/期限检查在开存储前
完成。根可预留旧/未来配置，但未来根不能提前验签。私钥 0400/0600、最大 4096 字节，
根文件最大 32 KiB 且不能 group/world 写或执行；均拒绝符号链接。错误不回显密钥内容。
Agent 关闭或离线恢复/轮换模式不能悄悄忽略这些设置；根文件不会热更新运行实例。

agent-cli-*.txt 记录加载边界、真实二进制和竞态回归。实际 CLI 测试用 PostgreSQL 验证
权限错误私钥在建表前拒绝；正确配置启动 TLS、管理 API 发证/撤销、WSS 协商并下载独立
验签。等待后台工作器将 proof artifact 推进到 2，撤销 epoch/payload 不变，随后 SIGTERM
正常排空；测试 49.614 秒通过。Make test-agent-cli 提供复现入口，App/CLI/API 竞态及 vet
通过。自动正向续签、实际 Agent 准备驱动、管理发布编排/UI、根轮换完整流程、探测/
ACTIVE、实际 gogw 及全部验收仍未完成；results.complete=false，未进行最终提交推送。

SDK 实际准备候选组件已接通：Client.PrepareRelease 下载精确 Bundle/policy、核验本地
scope/instance/boot/build/image/protocol 后调用应用 Builder，完整检查实际 Artifact 字节
的键集合/SHA/总大小和健康，才发送固定 Prepared。构建、摘要和健康失败不自动发送任何
终态；结果将本地 Workload、固定 Prepared 与远端 Receipt 分离，未知回报可保留句柄并
重连 RetryPrepared，不重建或续证据 TTL。候选是进程内实际资源，不从磁盘 ACK 恢复就绪。

PreparedWorkload.BuildRuntime 精确绑定激活的资源、代次、身份和 Prepare command，
通过应用 Instantiate 产生完整、独立的 RuntimeResources，再由原 RuntimeManager 执行
持久化/安装/Applied。Retire/Close 取消新的候选操作，已安装视图持引用到请求排空；
证据到期由单调计时器退役并最终关闭资源，不能靠冻结墙钟无限保留准备资格。

preparation-driver-*.txt 为真实 PostgreSQL/WSS/HTTPS、实际资源构造 handler、重连、
根更新、错误 boot、无提前 Ready、候选与活动资源生命周期、失败无终态及到期清理证据。
定向集成 23.170 秒、完整 SDK/服务端/协议竞态与 vet 通过。应用字节来自测试内实际提供器，
并非应用专用远程资源下载器；自动候选选择、完整 Agent 注册/续租/调度循环、管理编排/UI、
实际 gogw、探测/ACTIVE 和全部生产验收继续待完成。results.complete=false，未最终提交推送。

PreparationSet 现支持按已认证 epoch/generation/Prepare.targetRevision 选择进程内候选。
相同命令并发去重、重连精确回报；新 target 取消并退役旧构建，迟到结果不能取代新候选。
同一 RuntimeManager Builder 可跨重新准备替换资源，旧活动请求持引用到排空。顺序不从
磁盘 ACK 恢复，epoch 跳变拒绝；服务端字段为 additive uint64，并有最大值往返测试。

构建最多 4 个、未完成清理候选/构建最多 8 个，阻塞且忽略取消的回调不能触发无限替换；
Close 可超时且仍保有后台所有权。清理失败锁存首错并停止新分配。明确远端拒绝退役候选，
未知回执保留精确重试。信任根更新取消旧快照在途构建，应用需另行更新 RuntimeManager。

preparation-set-*.txt：真实 PostgreSQL/WSS/HTTPS 准备驱动及候选选择竞态集成 91.448 秒
通过；独立根更新隔离测试 8.087 秒通过；SDK/服务端/协议竞态、生成一致性和 vet 通过。
测试覆盖实际 handler 热切换、旧请求排空、迟到构建、4/8 数量上限、阻塞清理、清理失败、
远端冲突及重连不重建。并非完整自动 Agent 循环、真实下载器、发布编排/UI、探测/ACTIVE、
实际 gogw 或 A01—A28 全部验收；results.complete=false，最终提交/推送继续待完成。

注册循环组件新增 InstanceRegistration/RunRegistered，复用 Run 的凭证刷新、抖动退避和
不可重试拒绝处理。固定部署/boot 的注册和未决续租操作 ID/请求跨连接保存；重连不另做
注册、不替换 lease epoch，续租工作独立于应用事件回调。收到的身份/Ready/lease 形状、
期限、scope 和 epoch 重验；本地租期使用 UTC 加带 5 秒保守余量的单调截止，相同到期
回执不滑动截止。临近到期的计划间隔最小 100ms，防止不变期限触发高频续租。

每个 owner 只允许一个 runner；取消关闭连接并等待应用回调，不响应取消时保有所有权，
禁止悄悄开启新回调/runner。过期或明确身份/权限拒绝停止，不自动重新授权或从 ACK
恢复 Ready。State 只表达登记和链路，不证明应用健康/活动 RuntimeView/路由资格。

registration-loop-*.txt：SDK 故障注入和竞态测试 5.370 秒通过，全 SDK 竞态 17.655 秒
通过；真实 PostgreSQL/WSS 三组集成 32.919 秒通过，覆盖双副本重连、默认续租配置
（20 秒上限，剩余租期保守提前）、单独回调下续租、shutdown 排空和实际凭证撤销终止。
SDK 另覆盖丢失注册/续租回执精确重试、身份/Ready/异常 TTL 拒绝、相同到期不续权、
重复 runner、取消不响应回调所有权及零值 owner。vet 通过，Make test-agent-instances
纳入新测试。事件准备/激活/撤销仍由 serve 显式编排，健康更新/注销/过期重新授权、应用
资源下载器、完整 Agent 进程、管理发布编排/UI、探测/ACTIVE 和全部 A01—A28 尚未完成。

外部仓库状态补充：本轮只读观察相邻 gogw 已推进到
82671d1afce49bd5bdc6dd266e12e6b4706e651a（其 .omc/ 未跟踪内容保留），此前基线
62b1f935f7612dbb730504e8210d8cffe2f4aef7 仍用于既有证据。新增提交自述为 mock 闭环，
SDK 固定 805a696；其自定义 Snapshot Bundle、随机 Prepared/Applied operation、无持久
安全撤销等与本工作区生产契约尚未适配，不能将 mock 通过等同本方案真实 gogw 验收。
未修改该仓库。results.complete=false，未最终提交/推送。
