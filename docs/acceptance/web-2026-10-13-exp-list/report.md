# 实验列表报告摘要验收（2026-10-13）

## 范围

- 新接口 `GET /api/v1/admin/workspaces/{ws}/environments/{env}/experiment-summaries` 按更新时间降序返回当前环境的实验，一次最多重算 50 份报告。检索、状态过滤、分页均在服务端完成。
- 列表显示真实报告健康、决策、首个阻断原因；未启动、无报告权限和报告计算失败分别展示独立状态。报告计算失败不显示为健康。
- `createdBy` 从已认证管理员身份赋值，忽略请求体伪造值。旧记录可能没有创建人，列表以“—”显示。

## 验证

| 检查 | 结果 |
| --- | --- |
| API 工作空间隔离、环境隔离、分页/参数边界、运行中无曝光 HOLD、草稿无报告 | `TestExperimentSummariesScopePaginationAndReport` 通过 |
| 认证创建人不可伪造 | `TestExperimentLifecycleRepublishesFlag` 通过 |
| 路由与 OpenAPI 一致 | `TestOpenAPISpecMatchesRouteTable` 通过 |
| 中英列表 DOM 与 Web 契约 | `go test ./web` 通过 |
| 本机真实浏览器，运行中且无曝光 | 列表显示“Needs diagnosis / hold / no randomized exposures recorded yet”；同页 Draft 显示“Not started”，见 [桌面截图](desktop.png)与[390px 手机截图](mobile.png) |
| 本机真实浏览器，状态筛选为 paused | 显示空结果与 `0–0 of 0`，上一页和下一页均禁用 |

浏览器使用临时本机 JSON 数据仓库和本机管理员会话；构造了一个用于验收的实验，先创建 Draft 再迁移到 Running，另外创建一个 Draft 检查未启动状态和创建人归属。运行中实验没有用户曝光，因此报告的阻断状态来自真实服务端分析。它不是生产流量或统计效果验收。

`GRAYLING_TEST_PG_SKIP=1 go test ./internal/api ./internal/experiment ./web` 最初有 3 个统计用例失败：测试在创建后修改返回实验对象的 `MinRuntime`，存储的预注册值没有随之改变，故报告正确返回最短运行时间未满足的 HOLD。现已将参数配置移到入库前，`internal/experiment` 包通过；分析算法没有改动。本机跳过 PostgreSQL 容器测试，生产数据库路径仍以 CI 为准。

全项目回归还发现多个端到端夹具沿用旧版无条件保存 Flag 草稿的请求体。夹具现带 `baseRevision` 和 `expectedDraftHash`；重复发布前先读最新值。跳过本机 PostgreSQL 容器并排除 `test/gateway` 后，`go test` 对其余所有 Go 包通过；网关 A–E/G 子测试也通过。完整 `test/gateway` 的 Redis 子测试在本机 Docker Redis 等待可用时超时，属于当前环境门禁，未据此推断网关 Redis 路径通过。Python SDK 与 Kind 夹具已更新并通过 `python3 -m py_compile`；`uv run --project sdk/python --extra test python -m pytest sdk/python/tests -q` 有 24 个测试通过。
