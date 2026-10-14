# 总览三模块浏览器验收（2026-10-14）

## 范围与环境

验收 C2 提交：总览页风险带、待我处理审批、近期发布三模块（单请求读取服务端汇总端点 `GET {ws}/overview`，模块级降级与覆盖范围声明）。

- 服务：本仓库 `GOWORK=off make build` 产出的 `bin/grayling`，隔离数据目录 `/tmp/grayling-c2-acceptance/store.json`（全新初始化，未触碰仓库 `data/` 下真实文件），监听 `127.0.0.1:19090`，`GRAYLING_ADMIN_PASSWORD` 设固定密码；内置 gonacos 绑定 `127.0.0.1:19091/19092`。验收结束已 kill 服务进程并删除临时目录。
- 浏览器：`agent-browser` CLI 驱动 headless Chromium（本机 userns 限制要求 `--no-sandbox` 启动参数）。界面语言为中文，截图不含令牌明文。
- 预置数据全部经真实管理 API 走真实业务流程产生（curl + admin token，未直接改存储）：
  - 工作空间 `acceptance` / 环境 `prod`；bootstrap owner、`alice`（owner）、`carol-viewer`（viewer）三个 admin 令牌。
  - `checkout-v2`、`pricing-test` 两个 Flag 经草稿→提交→异人批准→发布（`flag.publish` 审计 ×3），`beta-users` Segment 同样流程发布（`segment.publish` 审计 ×1）。
  - 实验 `exp-6`：创建→评审→running，尚无任何采集事件；报告真实给出阻断原因 `no randomized exposures recorded yet`（`exp-6` 未绑定 Flag：绑定要求 reviewed/scheduled 状态，验收按 `internal/api/overview_test.go` 先例直接 reviewed→running，风险事实不受影响）。
  - `alice` 提交 `flag.rollback`（回滚 `pricing-test` 到 r1，HIGH 风险）审批申请 `approval-7`，处于 submitted 状态——对 owner 是“待我审批”，对 alice 自己是自批，对 viewer 是角色受限。

## 结果表

| 场景 | 证据 | 判定 |
| --- | --- | --- |
| owner 登录，三模块真实数据（1440px） | 风险带显示阻断 chip + `exp-6` + 服务端逐字原因 `no randomized exposures recorded yet`；统计卡 0/1/1/2 为真实计数；待我处理审批显示“1 项待你处理”与“待我审批”chip、申请人 alice、风险等级 high；近期发布 4 条（3×Flag 已发布、1×分段已发布）按新→旧排列并附原始动作；底部覆盖声明“已检查当前环境全部 1 个实验的状态及可用报告” | 通过，[桌面截图](desktop.png) |
| owner 登录，390px 手机 | 发布表格换成逐条记录卡（动作、时间、对象、操作者、环境、查看审计记录），`scrollWidth=innerWidth=390`，无整页横向滚动 | 通过，[手机截图](mobile.png) |
| viewer 角色真实裁剪 | `carol-viewer` 登录后风险带仍真实展示（viewer 有 report.read），待办项显示服务端 `blockedReason=role` 的“当前角色不能批准或执行”，无“待我审批/待我执行”chip 且无“N 项待你处理”计数；导航按能力隐藏令牌/配额页 | 通过，[viewer 截图](viewer.png) |
| 提交者自批排除（API 侧同场验证） | `alice` 视角 `GET .../overview` 返回 `myAction=""`、`blockedReason="self_approval"`、`actionableTotal=0`；owner 视角 `myAction="review"`、`actionableTotal=1` | 通过（HTTP 响应核对） |
| 跳转修正 | 风险项“打开实验”→ `#/console/acceptance/prod/experiments/exp-6`（summary 页签）；审批项“打开审批”→ `#/console/acceptance/prod/approvals/approval-7` 详情（修复旧版误跳 audit）；发布行“查看审计记录”→ `#/console/acceptance/prod/audit/pricing-test` 并显示“对象：pricing-test（精确匹配）”筛选横幅 | 通过 |
| 整页失败 + 重试 | 浏览器对 `**/overview*` 请求中止（合成故障，非服务端真实故障）后点击“刷新状态”：页面显示错误横幅与“重试”按钮；解除中止后点击“重试”恢复三模块真实数据 | 通过，[失败截图](failure.png) |
| 覆盖范围声明 | 审计扫描窗 21 条未触顶 → “已扫描最近 21 条审计记录。”且无截断提示；风险覆盖 1/1 完整 → complete 文案、无“查看全部实验”按钮（total 未超过 inspected 时不引导） | 通过 |

## 边界

- 风险带“覆盖不完整”（`riskBandPartial`）与“模块不可用”降级横幅的浏览器场景需要 >50 个实验或注入存储故障；本次由 `web/render_contract.mjs`（partial/unavailable/restricted/空数据矩阵）与 `internal/api/overview_test.go` 覆盖，浏览器只验收真实可达的 complete 路径与整页失败路径。
- `expired` / `breakglass_review` 两类 blockedReason 由服务端单测与 render 契约覆盖；本地存储无法直接铸造过期授权，未做浏览器场景。
- 风险项与发布项的环境列显示服务端内部环境 ID（如 `env-3`）而非 slug，与 C1 端点返回的 `environmentId` 语义一致（服务端事实逐字展示），后续如需展示 slug 属展示层调整。
- 截图语言为中文；EN 文案由 `web/i18n_contract.mjs` 双语 sweep 覆盖。

## 验证

- `node web/render_contract.mjs && node web/i18n_contract.mjs && node web/rollout_contract.mjs && node web/capability_contract.mjs && node web/flag_publish_contract.mjs`：全部通过。
- `GRAYLING_TEST_PG_DSN='postgres://postgres:grayling-test@127.0.0.1:15500/grayling_test?sslmode=disable' GOWORK=off go test ./web ./internal/api -count=1`：`ok` ×2。
- `GOWORK=off go build ./...`、`git diff --check`：通过。
