# 实例治理组件进度（未完成 P3 验收）

当前：登记管理、注册 WSS、嵌入式 Naming 对账、持久签名发现 WSS/HTTPS 已有组件证据；可信发现缓存、Prepared/RuntimeView、外部 Naming 和完整验收仍未完成。

新增 PostgreSQL v4 登记策略、精确部署授权、独立 60 秒租约、操作去重、连接栅栏、完整候选集合，以及嵌入式 Naming 的持久投影与精确回读。候选集合仍需 Prepared/Bundle 资格检查，不能直接作为路由依据。

Naming 身份包含 lease ID 和不可变 revision。续租、更新和替换保留旧投影的删除任务；旧写入晚于新版本确认返回，也只能创建旧投影，不能覆盖当前投影。已确认删除任务仍定期重查，清理延迟写入。直接读取原始 Naming 集合不提供治理资格，消费者必须匹配当前权威版本。稳定空集合按当前数据库时钟刷新传输有效期，不需要改变集合 revision。

证据：`instance-revision-fencing.txt`（真实嵌入式 Naming、旧版本延迟写入、精确元数据回读、租约生命周期等）；`instance-empty-refresh.txt`（旧 updated_at 的空集合刷新）。测试结果以各日志最终 PASS/FAIL 为准。

未接入：外部 Nacos 一致性。嵌入式底座 API 无 context 中断接口，尚未证明关闭时限；数据库操作记录、部署记录和 Naming tombstone 的长期清理仍待实现，不能宣称容量有界。A01–A28 整体验收状态保持未完成。

只读数据库复核确认 SQL 参数、领取 JOIN 和确认操作均使用 lease + revision，未发现新的同版本覆盖/误删路径。v4 为本工作区尚未发布的新增迁移；若存在曾使用早期 v4 DDL 的需保留数据库，必须另加迁移，不能仅替换建表 SQL。所有本轮集成测试使用隔离、可重建的 PostgreSQL 数据库。

本轮全套 `./test/agent` race 集成回归：39 个测试通过，0 失败、0 跳过，耗时 133.256 秒，见 `instance-full-integration.jsonl`。`go test -race ./internal/agent`、`go vet -tags=integration ./internal/agent ./test/agent` 与 `git diff --check` 通过。可通过 `make test-agent-instances` 单独复现实例组件检查。

注册 WSS 增量：已支持 Register/RenewLease/UpdateInstance/Deregister，按 instance.register 与 instance.renew 分别授权；Go SDK 增加 UpdateInstance/Deregister 类型化方法。跨副本重试使用持久 operationId，当前会话 fencing 保持生效。命令预算取 2 秒和调用者 deadline 的较早者。`instance-wss.txt` 覆盖真实 WSS/SDK 生命周期与权限拒绝；`instance-wss-deadline.txt` 为初始期限测试；加强后的期限及原有 WSS 回归以 `instance-wss-regression.txt` 最终结果为准。上述 39 项全套集成结果产生于本次 WSS 增量之前，不代替新增消息处理的回归。

WSS 扩大回归通过（27.899 秒）；API 契约、服务端和 SDK race 单元检查通过；App TLS 启停/重启、默认关闭路径及跨副本策略订阅/大资源下载回归通过（26.336 秒），见 `instance-wss-app-policy.txt`。本轮仍未完成 P3 整体接入或最终提交。

登记策略与部署授权管理 API 已接入真实 TLS/owner scope 授权，并复用事务审计与幂等操作。GET/PUT registration-policy 和 POST deployments 的 scope 由解析后的管理路径绑定，revision 以十进制字符串传输。`registration-admin-http.txt` 覆盖完整管理预置→WSS 注册→撤销及权限/CAS/载荷/地址负例。控制台尚无入口，仍不能据此认定 P3 或 A01–A28 验收完成。

管理、迁移和实例治理组合 race 回归：20 项通过，0 失败，80.323 秒。审计故障导致策略/部署业务回滚，恢复后相同 operationId 可成功授权；默认关闭新增三个管理路由保持 404。证据见 `registration-admin-regression.txt`、`registration-admin-disabled.txt`。

嵌入式 App 已接 Naming worker：100 scope keyset 页、四 scope/八底座调用并发、失败继续推进游标、按页汇总日志。关闭时先等待在途任务，再关闭 Sessions 与底座。`naming-worker-app.txt` 证明管理预置→WSS→自动底座回读确认→撤销清理；`naming-worker-integration.txt` 证明 102 scope 分页、不可取消回调的结构化等待，以及底座丢失投影后从已确认持久意图恢复。不宣称底座回调硬关闭时限、长期表容量、外部 Nacos 或整体 P3 完成。

发现载荷/验签组件：`pkg/agent/discovery.go` 定义完整候选集合 schema、精确 Bundle 分键、最长 60 秒有效期与租约上限、uint64 十进制字符串、严格 schema/地址/SNI/唯一性校验和不可变读取。`internal/agent/discovery.go` 将 coherent governance capture 转为仅包含 NamingConfirmed 成员的候选载荷。`discovery-codec-regression.txt`、`discovery-codec-integration.txt` 证明组件行为；其中治理确认使用模拟回读，不替代已有真实 Naming 回读证据。未实现持久 artifact 分配、传输发布、可信发现缓存与 Prepared/路由资格组合，不能宣称完整签名发现已交付。

v5 持久发现 artifact 已实现：独立资源计数器、同事务 epoch/成员捕获及签名落库、原字节复用、近过期刷新新版本、失败原子回滚和 uint64 耗尽拒绝。跨池并发、成员确认前后、Bundle 隔离、旧历史不变、空集合刷新、v2/v3/v4 迁移、未来签发时间拒绝均有 PostgreSQL race 证据（`discovery-store-integration.txt`、`discovery-store-boundaries.txt`）。`discovery-store-session-expiry.txt` 使用真实锁等待跨过会话期限，确认返回 FENCED 且不推进 artifact head。历史记录容量清理、网络发现及 Prepared 组合仍未完成。

发现网络传输增量：配置同 Sessions 的签名 Publisher 后声明 discovery.read；按权限支持完整快照订阅、刷新、精确版本 FetchResource 和受会话授权的 HTTPS kind=2 下载。每连接最多一个活动发现订阅。真实 Naming→签名 WSS→原字节 HTTPS→跨副本重连、独立发现权限、未配置发布器、旧会话及跨 scope 拒绝见 `discovery-channel-integration.txt`；真实 App 管理预置与自动 Naming 对账/发现、原有策略/大下载/WSS 回归见 `discovery-channel-regression.txt`。可信发现缓存、Prepared 组合、外部 Naming、签名密钥 CLI 供应及完整方案验收仍未完成。
