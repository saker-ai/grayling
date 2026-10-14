# 放量双进程并发与报告版本验收（2026-10-13）

## 范围

`TestTwoProcessPlanConflictAndReportEvidence` 使用独立 PostgreSQL 测试库和两个独立的管理 API 进程。第二个进程在准备数据后启动，因此持有独立的内存快照；两进程均通过真实 HTTP 和共享 PostgreSQL 操作同一个实验。测试验证放量锁下的状态重读、并发操作串行化，以及报告证据版本冲突。

## 结果

- 两进程同时请求暂停：一次 200、一次 409；计划只留下一个 `manual.pause` 历史事件和一条 `release_plan.pause` 审计记录。
- 第一进程采集新曝光与转化后，报告证据版本改变。第二进程用旧版本请求恢复得到 409，计划保持暂停；用当前版本请求恢复得到 200，第一进程重读 PostgreSQL 后看到运行中计划，审计含该证据版本。
- 本测试与现有 `TestMultiReplicaPlanStartTakeoverAndManualAdvance` 在真实 PostgreSQL 容器网络内各连续运行 5 次，全部通过；新测试以 `-race` 再运行一次通过。`GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test ./internal/rollout -count=1` 覆盖非 PostgreSQL 用例。

本机 Docker 发布到宿主机的 PostgreSQL 端口可建立 TCP 连接，但数据库协议读取超时。为验证产品逻辑，先用 `go test -c` 编译原测试二进制，再将其放在同一 PostgreSQL 容器网络中运行，使用 `GRAYLING_TEST_PG_DSN` 创建和清理独立测试库。容器内现有多副本放量测试 `TestMultiReplicaPlanStartTakeoverAndManualAdvance` 也通过。

## 后续门槛

本验收覆盖暂停与恢复的双进程 HTTP 路径。人工推进、交还自动控制、自动引擎与事件恰在检查和写入之间发生的竞争仍需独立验证；当前服务端没有跨事件存储和计划存储的原子报告快照。完整 Grayling 二进制跨主机验收也尚未完成。
