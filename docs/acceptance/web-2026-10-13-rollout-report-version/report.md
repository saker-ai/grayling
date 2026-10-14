# 放量操作报告证据版本验收（2026-10-13）

## 范围与实现

`GET /api/v1/admin/experiments/{id}/report` 返回 `evidenceVersion`，JSON/CSV 导出包含同一值。该 SHA-256 指纹覆盖报告决策证据；生成时间与仅随时钟移动的水位不改变它。运行中实验人工推进、已起跑计划恢复、关闭人工接管必须传 `expectedReportVersion`。服务端在状态变更前重算并比较：缺失版本 400，过期版本 409，计划不改变。手动动作的计划历史和审计包含实际核对的版本。

控制台打开确认框时显示报告摘要与完整版本，提交前复读。若版本变化，确认框保持打开、理由保留、摘要更新，用户须重新核对后再次提交；若当前报告不可用，提交不发生。暂停和开启人工接管仍可在报告故障时使用。

## 已执行证据

命令：

```sh
GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test ./internal/experiment ./internal/rollout ./internal/api ./web -count=1
git diff --check
```

结果：四个 Go 包测试通过，diff 无空白错误。`TestReportEvidenceVersionTracksAnalysisWithoutClockChurn` 覆盖时钟稳定、证据变化与报告不被改写；`TestOperatorActionsRejectChangedReportEvidence` 经真实 HTTP 路由验证缺失/过期版本、计划保持、当前版本成功、历史与审计；`web/rollout_contract.mjs` 验证确认期间版本变化不发写请求。导出测试核对 JSON/CSV 版本一致性。

在隔离 QA 数据上又执行了真实浏览器验收：以发布者身份打开运行中的 `exp-8`，点击“Advance now…”后确认弹窗包含实验、环境、当前阶段、报告判定、原因和完整证据版本。检查了 [桌面截图](desktop.png) 与 [390 px 手机截图](mobile.png)。初次验收发现长 SHA-256 值在桌面弹窗里被裁切；给弹窗版本值增加换行样式后重新构建服务并复测，两种宽度下完整值均可见。浏览器只打开确认框，未提交推进操作。

## 边界与后续

已补充[双进程 PostgreSQL 并发与证据版本验收](../rollout-2026-10-13-two-process-evidence/report.md)，覆盖暂停和恢复路径。服务端在写入前检查最新报告；如果事件写入恰好发生于检查和计划状态持久化之间，目前没有跨事件存储与计划存储的原子快照。其他放量动作的跨副本并发和真实浏览器提交操作仍属于[总实施方案](../../产品与Web体验完整实施方案.md) P5 的现场验收。
