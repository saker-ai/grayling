# Rollout 多副本选主与故障接管

所有控制面副本使用同一个 PostgreSQL DSN，并配置相同的 `-rollout-interval`（默认 30s）。自动推进任务使用 `rollout_leader` 单行租约；无需指定固定主副本。文件和内存存储只适合单进程。`-rollout-interval 0` 在该副本关闭自动任务，人工计划操作仍可使用。`-rollout-lease-duration` 默认 `0`（自动取 `max(3 × interval, 30s)`）；显式配置时必须大于 tick 间隔，并应大于实际最长决策 pass。

每轮 tick 在 PostgreSQL 上以数据库时钟获取或续租，租期为配置值或上述默认值。持有者身份包含主机名、进程号和随机启动 ID。租约有效期内仅持有者运行扫描；进程停止、失联或无法续租后，数据库时钟达到 `expires_at` 时其他副本可接管，`generation` 随之递增。进程本地时钟仅用于计划观察窗，不决定选主。数据库不可用时无法获得或验证租约，自动任务停止推进并记录日志；连接恢复后下次 tick 重试。数据库连接在一次决策过程中中断时，变更前的持有权复核失败，任务停下。

每个计划仍使用 PostgreSQL advisory lock，锁内重载实验状态；决策门查询完成后复核租约，再执行持久化和发布。若旧持有者在复核后失联，当前实验的锁会串行化新旧操作，后继副本重载状态并按现有幂等路径收敛。长于租期的单次扫描不会续租，后续检查失败后结束；应按实际报告耗时调大 tick 间隔。手动暂停、恢复、推进和人工接管不依赖自动任务租约，继续使用实验锁与原有决策规则。

## 观察与操作

管理员 token 请求 `GET /api/v1/admin/rollout/leadership`，查看 `holder`、`generation`、`expiresAt`、`lastRunAt`、`lastTakeoverAt`、`lastError`。空 `holder` 表示当前无有效持有者；503 表示数据库状态无法确认。各进程日志记录获取租约、选主失败、执行结果和持有权丢失。监控应关注 `lastRunAt` 停滞、`lastError` 非空、频繁 `generation` 递增。

计划性关闭自动任务：把所有副本的 `-rollout-interval` 设为 `0` 并重启，等待当前租约过期后确认 `holder` 为空。恢复时在所有副本设回相同的正间隔。单节点回退可仅保留一个副本开启自动任务，但仍需共享 PostgreSQL；切换前等待旧租约过期，避免误把等待期视为故障。切勿手工修改租约表的 holder 或 generation。数据库故障恢复后确认管理接口状态与计划历史、审计，再按需人工推进。

## 验证范围

`internal/platform/rollout_lease_test.go` 使用真实 PostgreSQL 验证租约过期、代际隔离、连接中断和受控 TCP 断连后的故障接管。`internal/rollout/engine_test.go` 使用共享真实 PostgreSQL 的两个引擎验证同时运行、健康决策门只推进一次、SRM 暂停、护栏回滚和切主期间的人工接管/手动推进。`test/cluster/TestRolloutLeaderProcessFailover` 使用两个真实 `cmd/golab` 进程和共享 PostgreSQL、Redis 验证主进程终止、重启和接管；计划步骤与审计断言由前述引擎测试覆盖。可运行 `GOWORK=off go test ./internal/platform ./internal/rollout ./test/cluster -count=1`。
