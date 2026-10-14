# 六类状态细分首屏文案浏览器验收（2026-10-14）

## 范围与环境

验收 C5 提交：实验详情决策摘要区首屏按六类状态（未启动 / 无样本 / 未成熟 / 已阻止 / 可推进 / 已结束）渲染独立主文案与主动作；纯函数 `classifyExpStatus` 分类，优先级为“报告读取失败 > 无 report.read > 六类”；ETA 缺失只给降级文案不做估算；no_samples 子分支只引用 diagnostics 真实分配/曝光字段；blocked 逐字引用服务端 reasons/healthIssues 与服务端维护的影响范围；ended 的操作者/时间/原因取最新 `experiment.transition` 审计，取不到给降级文案+审计链接；can_advance 展示真实主指标与护栏摘要且不宣称绝对获胜。

- 服务：本仓库 `go build ./cmd/grayling` 产出的 `bin/grayling`，隔离数据目录 `/tmp/grayling-c5-acceptance/store.json`（全新初始化，未触碰仓库 `data/`），监听 `127.0.0.1:19095`（内置 gonacos `19096/19097`），`GRAYLING_ADMIN_PASSWORD` 设固定 12+ 字节密码。验收结束已 kill 服务进程、删除临时目录（含 gonacos 子目录）并关闭两个浏览器会话。
- 浏览器：`agent-browser` CLI 驱动 headless Chromium（`AGENT_BROWSER_ARGS="--no-sandbox"`），界面语言中文，桌面 1440×900、手机 390×844；截图不含令牌明文（登录用密码，viewer 场景登录在独立 session）。
- 预置数据全部经真实管理 API 走真实业务流程产生（curl + owner/alice 令牌，未直接改存储）：
  - 工作空间 `acceptance` / 环境 `prod`；令牌 `alice-publisher`（publisher，负责批准与暂停操作）、`vince-viewer`（viewer，角色裁剪）、`sdk-eval`（sdk）、`collect-writer`（collect）。
  - 7 个 Flag（`fs-ns/fs-nos/fs-nor/fs-imm/fs-blk/fs-adv/fs-end`）：draft→submit→alice 批准→owner 发布 r1；发布过绑定的 Flag 永久持有绑定，因此每个运行中实验使用独立 Flag（服务端 `existing_binding` 预检语义）。
  - 7 个实验（exp-7…exp-13），每个运行中实验走 review→bind→alice 批准绑定→owner 发布绑定→transition running（transition 设置真实 `startedAt`）。
  - 真实流量：`POST /api/v1/sdk/eval`（onDecision 服务端自动落幂等曝光；onRender 落分配并回 exposureToken，本次**不提交曝光**以构造“已分配未触达”）、`POST /api/v1/collect` 提交真实转化（每单元一次、`metricKey=checkout_success`、当前时间 clientTime）。
  - 数据性质：单元与转化数量是脚本合成的“真实 API 流量”（与仓库既有验收一致的本地造数方式）；报告、判定、审计全部由服务端真实计算，无前端或服务端伪造。

## 结果表

| 场景 | 证据 | 判定 |
| --- | --- | --- |
| ① not_started（exp-7，draft） | hero：“还未开始采集实验结果。”+“完成准备检查后提交评审并启动。”+主动作“查看准备检查”（跳设置与审计区）；未把 draft 呈现为任何结果态 | 通过，[desktop-not-started.png](desktop-not-started.png) |
| ② no_samples 通用子分支（exp-8，running onDecision 无任何事件） | 报告真实阻断原因 `no randomized exposures recorded yet`；diagnostics 真实返回（onDecision 无分配事实）→ 不猜子分支，只给“等待首批实际曝光。”+“查看 SDK 接入和采集诊断”，不出现任何编造的分配数 | 通过，[desktop-no-samples-generic.png](desktop-no-samples-generic.png) |
| ③ no_samples 已分配未触达子分支（exp-9，running onRender，8 次 eval 仅落分配） | hero：“等待首批实际曝光。已分配 8 个单元但尚无实际曝光——请检查展示触发点与 SDK 接入。”（8 = diagnostics 真实 assignments 4+4，exposures 0） | 通过，[desktop-no-samples-assigned.png](desktop-no-samples-assigned.png) |
| ④ immature（exp-10，running，真实曝光 12/转化 3，minRuntime 24h） | hero：“结果未成熟，当前仅供观察。”+“预计可判定时间：2026-10-15 18:31:56”（= 服务端真实 startedAt+24h，本地时区显示）+服务端逐字原因 `pre-registered minimum runtime not yet satisfied`+主动作“查看水位” | 通过，[desktop-immature.png](desktop-immature.png) |
| ⑤ blocked（exp-11，running，control 转化 19/23 vs treatment 3/17 → 服务端真实 ROLLBACK） | hero 红横幅“自动推进已阻止。”+逐字服务端原因 `primary metric significantly worse in treatment: diff=-0.649616 CI=[-0.888017, -0.411216] p=4.455e-05 < alpha=0.05`+影响范围 `Flag fs-blk · 环境 env-3 · epoch 1`（服务器维护的绑定关系）+主动作“查看诊断 / 暂停/回滚流程 / 查看最近变化”；“查看诊断”点击后深链 `…/exp-11/health` 且区域 h2=实验诊断 | 通过，[desktop-blocked.png](desktop-blocked.png)、[点击后](desktop-blocked-view-diagnostics.png) |
| ⑥ can_advance（exp-12，running，treatment 转化 15/17 vs control 7/24 → 服务端真实 SHIP） | hero 绿横幅“数据满足当前放量门槛。”+真实主指标行 `p=0.000186… · diff=0.590686… · CI [0.352936…, 0.828436…]`+护栏摘要+“这不宣称绝对获胜；推进前请核对主指标与护栏证据。”+主动作“查看证据 / 放量时间轴”；viewer（真实角色裁剪）同页仅显示“查看证据”，推进动作按 `flag.publish` 门控消失 | 通过，[desktop-can-advance.png](desktop-can-advance.png)、[viewer 裁剪](desktop-can-advance-viewer.png) |
| ⑦ ended（exp-13，running→alice 暂停） | hero：“paused：alice-publisher (tok-4) 于 2026-10-14 18:31:59 操作——原因：investigating conversion drop on mobile”（操作者/时间/原因取最新 `experiment.transition` 审计，逐字）+主动作“查看审计 / 设置与审计”；“查看审计”点击后深链 `…/audit/exp-13`，精确对象过滤横幅 + 首行即该 transition 记录；hero 下方决策摘要快照（ROLLBACK/SHIP chip + 逐字 reasons）与状态带六字段同时可见 | 通过，[desktop-ended.png](desktop-ended.png)、[点击后](desktop-ended-view-audit.png) |
| ⑧ 390px | `scrollWidth=innerWidth=390` 无整页横向滚动；状态带换行、区域导航两排整宽按钮、hero 横幅与动作按钮可读可点 | 通过，[mobile.png](mobile.png)（blocked）、[mobile-can-advance.png](mobile-can-advance.png) |

## 边界

- 六类全部命中真实可达场景，无需合成故障；`can_advance` 由真实转化量产生服务端 SHIP 判定（统计成熟，非浏览器合成），report 判定、CI、审计均服务端真实计算。
- `immature` 的 `startedAt` 缺失降级文案（`exp.hero.immatureEtaUnknown`）与 diagnostics 抓取失败降级（通用文案+重试按钮）未做浏览器合成：二者分别由 `web/render_contract.mjs` 断言矩阵覆盖（无 startedAt 时 hero 不含任何时间数字；diagFailed 时渲染 `data-hero-goto="retry-diagnostics"`），且 exp-8 展示了“onDecision 无分配事实→不猜子分支”的真实通用文案路径。
- 报告读取失败（error 分支优先于六类、不呈现旧健康结论）沿用 C4 既有浏览器证据（web-2026-10-13-exp-decision / web-2026-10-14-exp-zones 场景⑤）与 render_contract 断言，本轮未重复合成。
- 无 report.read 的 restricted 分支为防御性路径（RBAC 对作用域内全部角色放行 report.read），仅由 render_contract 断言覆盖，与 C1-C4 处理一致。
- EN 文案与双语 parity 由 `web/i18n_contract.mjs` 覆盖：新增九个 hero 用例（含六类+两子分支+ETA 缺失+审计缺失）逐类 en/zh 采样，服务端原因/操作者/指标数字在两种语言逐字透传；另新增 `experiments-detail-ended` 视图 sweep 验证 ended 首屏在视图层的真实渲染与对象过滤审计请求。
- 截图语言为中文；状态带、区域导航、放量门禁行为本轮未改动（C4 证据继续有效）。

## 验证

- `node web/render_contract.mjs && node web/rollout_contract.mjs && node web/i18n_contract.mjs && node web/capability_contract.mjs && node web/flag_publish_contract.mjs`：全部通过（rollout / capability / flag_publish 契约零断言改动）。
- `GOWORK=off go test ./web -count=1`：ok。
- `GOWORK=off go build ./...`、`git diff --check`：通过。
