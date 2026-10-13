# 平台存储

GoLab 的控制面数据与 gonacos 配置底座分别存储。`-sqlite` 和 `-pg` 只选择 GoLab 平台存储；gonacos 仍由内嵌 Redis 或 `-redis` 指定的外部 Redis 承载。

| 参数 | 用途 | 部署范围 |
| --- | --- | --- |
| `-data path.json` | 默认 JSON 文件 | 本地开发、单进程 |
| `-sqlite path.db` | GORM + 纯 Go SQLite | 单节点部署；无需 CGO |
| `-pg postgres://...` | GORM + PostgreSQL | 多副本部署 |

`-pg` 和 `-sqlite` 互斥，启用任一参数后忽略 `-data`。数据库首次打开时自动建表。新 GORM schema 不读取旧版 `pgx` schema；切换已有部署时须按业务需要单独规划数据迁移。

数据库后端以事务保存控制状态、凭证、原始事件和统计增量。控制状态有版本号，跨副本写入使用乐观冲突检查；事件 ID 是持久去重键，仅成功插入的事件会递增统计。原始事件、单元聚合、曝光单元集合分别存储，重启时重建内存工作集。事件写事务通过状态行锁串行化，吞吐量须按实际采集负载验证。事件按保留期删除；统计保留供报告使用。HMAC 轮换在同一事务中更新事件、分配记录与统计键。

PostgreSQL 使用数据库会话锁串行化跨副本发布、计划操作和首次 owner 凭证创建，租约由数据库时钟裁决。副本每 500 ms 检查版本：控制状态变化时重载工作集；只有事件变化时按提交版本读取新增事件和受影响的统计键。事件同步积压超过 4096 条或遇到保留期清理时回退完整重载。SQLite 使用单连接写入和 WAL，适合单节点；数据库文件权限收紧为 `0600`。

PostgreSQL 旧版按月分区的原始事件表不属于新 GORM schema。当前使用带保留期索引的事件表；需要按月分区的大规模部署应在容量规划时单独评估。备份恢复应同时覆盖平台数据库、gonacos 快照和密钥，并使用同一恢复点校验。

验证：`GOWORK=off go test ./internal/platform -run '^TestSQLite'` 检查纯 Go SQLite 与事件增量同步；提供 `GOLAB_TEST_PG_DSN` 后运行 `GOWORK=off go test ./internal/platform ./internal/experiment ./internal/api -run 'TestGORMPostgres|TestPostgresReport|TestQuotaAdminReplicaRead'` 检查真实 PostgreSQL。CI 将快速单测与 PostgreSQL 集成测试分成独立任务。旧 pgx 后端的分区、增量失效和性能报告不能作为当前 GORM 后端的容量保证；上线前仍须用实际事件速率与数据量压测。
