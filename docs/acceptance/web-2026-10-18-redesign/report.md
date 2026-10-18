# Web 控制台重设计总验收（2026-10-18）

重设计 R5 总报告。R0–R4（设计令牌与主题、WCAG 缺口、移动端记录卡、手绘 SVG 数据可视化、控件态打磨）完成后的统一验收：R5A（axe 审计 / 性能基线 / 键盘走查）+ R5B（角色×状态×设备×语言矩阵）。全部针对真实 `bin/grayling`、独立 store、零 mock。

## 范围与环境

- 被测系统：本仓库 `make build` 产出的 `bin/grayling`（内嵌当前 `web/static/` React 包），真实 Go 服务 + 内嵌 gonacos（ephemeral 回环端口）。每次运行在 `U4_TMP` 下新建隔离 store，跑完删除；零 mock、零存储直写。
- 浏览器：Playwright Chromium 无头（`@playwright/test` 固定 1.62.0，经 `web/package.json` 的 `createRequire` 解析；Chromium 来自 npmmirror CfT 镜像）。桌面主上下文 1440×1000，移动上下文 390×844（`isMobile`+触屏）。
- 探针：每个页面注入 `window.confirm/prompt/alert` 计数探针与 `pageerror` 捕获，任何原生弹窗调用或未捕获异常即判失败；各脚本结束前排空探针。
- 数据：全部经真实 REST/collect API 产生（`lib/seed.mjs`；矩阵内四个角色令牌经真实令牌管理 API 铸造，实验事件经真实 collect API 摄入）。
- 脚本：`scripts/acceptance/u4/{axe-audit,perf-baseline,keyboard-walkthrough,matrix}.mjs`（另有 `smoke.mjs` 自检、`run-server.sh` 手动起服；用法见同目录 README）。
- 变更范围：`de408c6..aa79e92`（R0–R4 与 R5A 设施），外加 R5B 验收中抓到并修复的一处真实 bug（见下）。

## R0–R4 变更摘要（git log de408c6..HEAD）

| commit | 内容 |
| --- | --- |
| `340bdca` | 关闭 WCAG 缺口：APG tabs、live loading 区、progressbar 语义 |
| `11d7577` | 移动端记录卡统一进 DataTable 单列声明 |
| `bcecd2d` | 手绘 SVG 数据可视化层：CI、样本流、SRM、放量时间轴 |
| `0e506e8` | 控件态、暗色表面、触控目标分级打磨 |
| `2395d82` | U4 验收设施 + axe 审计（R5A.1） |
| `7091e31` | U4 性能基线 + 硬预算断言（R5A.2） |
| `aa79e92` | U4 键盘走查（R5A.3） |

## 验收总表

### 1. axe 审计（R5A.1）— 64 运行，0 违例，0 例外

| 项 | 结果 |
| --- | --- |
| 覆盖 | 24 条路由 × desktop/zh/light 全量 + 10 条关键页路由 × zh/dark、en/light、en/dark、mobile-390px，合计 64 次 axe 运行 |
| 规则集 | wcag2a / wcag2aa / wcag21a / wcag21aa / wcag22aa |
| 开放违例 / 例外 | 0 / 0（例外机制存在且强制登记，当前为空表） |
| 原生弹窗 / pageerror | 0 / 0 |
| 修复历史 | R5A 期间抓到并修复 2 条 axe 规则违例（`select-name` critical×2、`target-size` serious）+ 2 条走查准备问题（skip-link 哈希路由冲突、侧边导航 `aria-current`），逐条记录于 `axe-exceptions.md` |
| 证据 | `axe-results.json`、`axe-summary.{md,json}`、`axe-exceptions.md` |

### 2. 性能基线（R5A.2）— 预算全过

| 预算项 | 实测 / 上限 | 判定 |
| --- | --- | --- |
| 主 chunk `index-Du8ZIdKt.js` | 445337 / 471040 B | PASS |
| 样式 `index-tZBqJxoB.css` | 32003 / 32768 B | PASS |
| 可视化源码 `web-pages/src/viz/**` | 15360 / 15360 B | PASS（零余量，属移交注意事项） |
| 运行时依赖（对 `HEAD~` 锁文件） | added=0 removed=0 | PASS |

- 六关键页 FCP/LCP/load/TTI-proxy 中位数（冷/热各 5 次）见 `perf-summary.md`：冷 LCP 全部 ≤408ms（本机回环），热 LCP 全部 ≤392ms。
- 1000-flag 列表筛选中位 17.8ms（n=5）；实验报告页 longtask 捕获 1 条 >50ms（合计 53.0ms，单次运行抖动，预算判定不受影响）。
- 方法论：`perf-methodology.md`；原始数据：`perf-results.json`。

### 3. 键盘走查（R5A.3）— K1–K7 全过

| 组 | 内容 | 结果 |
| --- | --- | --- |
| K1 | skip-link 首个 Tab 停靠、激活聚焦 `#main`、hash 路由存活 | PASS ×4 |
| K2 | 侧边导航 11 按钮全部可达、唯一 `aria-current=page` | PASS ×4 |
| K3 | ZoneNav 方向键移动不选中（手动激活）、Enter 选中并深链、tabpanel 配对 | PASS ×6 |
| K4 | 对话框焦点进入/陷阱/循环/Escape 关闭/焦点归还触发器 | PASS ×6 |
| K5 | 回滚两步确认全程键盘可达，产生真实 `flag.rollback` 审批 | PASS ×4 |
| K6 | 装饰性 SVG 图表不进入 Tab 序（28 站走查） | PASS |
| K7 | 焦点环截图（zh/light + zh/dark，8 张） | PASS |

证据：`keyboard-walkthrough.{json,md}`、`keyboard-*.png`。

### 4. 角色×状态×设备×语言矩阵（R5B.1）— 71 检查全过

`matrix.mjs` 对全新隔离 store 单次运行：71 项检查、0 失败、0 警告；产出 78 张截图 + 6 份 a11y tree YAML + `matrix-results.json` / `matrix-summary.md` / `matrix-reports.json`。

| 组 | 检查数 | 内容 | 证据 |
| --- | --- | --- | --- |
| R 角色 | 26 | 真实令牌 API 铸造 publisher/approver/analyst/viewer；四角色 capabilities 地图与 RBAC 逐项一致；**双管理员闭环**：publisher 全程 UI 建草稿→提交，approver（异身份）全程 UI 批准→发布，审计归因 `submit=mx-publisher (tok-7)` / `approve=publish=mx-approver (tok-8)` 服务端复核；analyst/viewer 写拒绝为 API 403 + UI 隐藏双层 | `matrix-role-*.png`（16） |
| S 状态 | 10 | 无凭据 401（API 401 + 登录页拦截）；并发编辑 409（API 409 + UI 冲突面板且草稿保留）；配额耗尽 429（API `quota_exceeded` + UI 错误 toast）；离线：已缓存路由降级为页内错误横幅、未缓存 chunk 由 shell 内重载卡兜底、恢复连接后两条路径均复原 | `matrix-state-*.png`（7） |
| D 数据 | 12 | 空工作空间、加载骨架（真实响应延迟注入）、不存在 flag 错误页、零数据 running 实验（不伪造结论）、小样本水位未到（55 事件）、健康样本真实 CI/样本流图（308 事件）、SRM 偏斜 200/20（242 事件）、1000 flag 大列表 | `matrix-data-*.png`（10） |
| V 设备 | 16 | 390/768/1024/1440 四宽 × overview/flags/flag-detail/exp-summary，全部 `scrollWidth=innerWidth` 零横向溢出 | `matrix-width-*.png`（16） |
| L 语言×主题 | 1 | zh/en × light/dark 四组合 × 6 关键页全组合截图 + 探针巡检（零弹窗零异常） | `matrix-combo-*.png`（24） |
| A 辅助技术 | 5 | 200% 缩放等效（720 CSS px 视口）零横向滚动；`prefers-reduced-motion` 使骨架脉冲静止（duration≈0s/1 次）；`forced-colors` 下图表结构与文字状态不丢（非纯颜色编码）；`prefers-contrast: more` 巡检 | `matrix-a11y-*.png`（5） |
| T 无障碍树 | 1 | 6 关键页 `ariaSnapshot` 归档 | `a11y-tree/*.yaml`（6） |

抽样矩阵（全笛卡尔积不可行，按设计文档 + 风险抽样；每维度至少在 2 个关键页交叉）：

| 维度 | 取值 | 交叉方式 |
| --- | --- | --- |
| 角色 | publisher / approver / analyst / viewer / 无凭据 | 每角色 API + UI 双层；publisher、approver 另跑全流程闭环 |
| 故障状态 | 401 / 409 / 429 / 离线（缓存与未缓存两变体） | flags 列表 + flag 详情两处交叉 |
| 数据状态 | 空 / 加载 / 错误 / 零数据 / 小样本 / 健康 / SRM / 大列表 | flags 列表 + 实验详情（summary/health/evidence 区） |
| 设备 | 390 / 768 / 1024 / 1440 | 4 关键页 × 4 宽 = 16 格全组合 |
| 语言×主题 | zh/en × light/dark | 6 关键页 × 4 组合 = 24 格全组合 |
| 辅助技术 | 200% 缩放 / reduced-motion / forced-colors / contrast-more | overview / flag-detail / 健康实验证据页 |

### 5. 遗留限制

| 限制 | 说明 |
| --- | --- |
| 读屏器替代 | 本验收以 **a11y tree 快照 + axe 静态规则 + 键盘走查**三者组合代替真实读屏器（NVDA/VoiceOver）实测；结构性语义（角色/命名/焦点序/实时区）有保障，但不等于读屏器实际朗读体验 |
| 加载态产生方式 | D2 的 loading 证据用 `route.continue()` 对**真实服务端响应**做延迟注入产生，不是 mock；骨架真实渲染、数据真实到达 |
| 200% 缩放方法 | 以 720 CSS px 视口等效 1440 基准的 200% 缩放（同 DPR），方法已在本报告与 `matrix.mjs` 注释声明 |
| 语言×主题证据深度 | 24 格为截图 + 探针巡检（零弹窗/零异常），未做逐像素 golden 比对 |
| 长任务单次抖动 | 本次 perf 运行报告页出现 1 条 53ms longtask（上一轮为 0），属本机抖动量级，预算判定不受影响 |

## 验收中抓到并已修复的问题

1. **离线进入未缓存的路由 chunk 会拆掉整台控制台**（真实 bug，`web/src/routes.tsx`）：路由级 code-splitting 下，离线时首次导航到未加载过的 lazy chunk，`React.lazy` 的动态 import 失败且对整个页面生命周期缓存该拒绝；各路由均无 `errorElement`，React Router 默认错误边界把**整个控制台（含 shell 与导航）**替换为错误页，不刷新无法恢复。修复：新增 `web/src/components/ChunkErrorBoundary.tsx`（按 `location.pathname` keyed 复位），`routes.tsx` 的 `view()` 统一包裹，失败降级为 shell 内的错误卡 + 显式"重新加载"按钮（新增 i18n 键 `err.chunkLoad`/`btn.reload`，中英同步）。矩阵 S5 拆成两变体验证：已缓存路由离线→页内横幅 + 恢复；未缓存 chunk 离线→shell 存活 + 重载卡 + 恢复后可达。修复后 `pnpm -C web test`（36 文件/279 测试）、`pnpm -C web build`、`make build` 全绿，axe/perf/keyboard 三套证据对修复后二进制全量重跑。

## 边界与已知限制

- 矩阵不跑全笛卡尔积；采样矩阵如上表，每维度至少在 2 个关键页交叉。角色×设备、角色×语言未全交叉（门控在数据层、布局与语言正交，风险低）。
- 设计文档"数据×故障"全景中的**无分配、污染、报告不可用、503、部分汇总失败**五态未进矩阵：本地真实服务无合成故障注入点（零 mock 约束下不可达）；其中整页失败 + 重试、模块级降级横幅由 `web-2026-10-14-overview-modules` 既有验收覆盖，护栏破线在健康样本实验中真实出现（`matrix-data-healthy-summary.png` 顶部"自动推进已阻止"横幅与 ROLLBACK 决策为真实计算输出，非矩阵显式检查项）。
- 401 的"会话过期后再认证"完整链路属 `web-2026-10-13-reauth` 既有验收；矩阵验证无凭据→401 + 登录页拦截。
- 大列表验证的是 1000 项下渲染与筛选可用；服务端分页不存在（列表全量拉取），与迁移验收的边界声明一致。
- 性能数字为本机回环（无网络损耗）下的相对基线与预算门禁，非生产 SLA。
- 真实读屏器、真机（非模拟）设备未覆盖，同迁移验收声明。
- `web-pages/src/viz/**` 预算 15360/15360 零余量：任何 viz 源码改动都会触发预算重估，属移交注意事项而非违例。

## 验证

- 四套 U4 脚本对修复后二进制全绿（退出码均为 0）：
  `node scripts/acceptance/u4/axe-audit.mjs`（64 运行 0 违例）、
  `node scripts/acceptance/u4/perf-baseline.mjs`（预算 4/4 PASS）、
  `node scripts/acceptance/u4/keyboard-walkthrough.mjs`（K1–K7 PASS）、
  `node scripts/acceptance/u4/matrix.mjs`（71 检查 0 失败 0 警告）。
- 源码修复门禁：`pnpm -C web test`（36 文件 / 279 测试全过）、`pnpm -C web build`（`index-Du8ZIdKt.js` 445337 B、`index-tZBqJxoB.css` 32003 B）、`make build`、`make check-static`。
- 全部证据与本报告同目录：截图 86 张（matrix 78 + keyboard 8）、`a11y-tree/*.yaml` 6 份、各脚本 `*-results.json` / `*-summary.md`。
