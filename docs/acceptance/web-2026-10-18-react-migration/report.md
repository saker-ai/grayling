# Web 控制台 React 迁移总验收（2026-10-18）

阶段 4（切换、清理与总验收）的真浏览器验收。阶段 0–3（11 个视图移植、契约测试清零、`web/render_contract_test.go` 改驱动 vitest）已先行验收，本目录记录最终形态的真机验证。

## 范围与环境

- 被测系统：本仓库 `bin/grayling`（`make build` 产物，内嵌 `web/static/` 的 React 包），真实 Go 服务、真实存储，无任何 mock。
- 运行参数：`GRAYLING_DEV_AUTOLOGIN=1 ./bin/grayling -addr :27995 -data <独立临时 store> -gonacos-data <独立临时目录>`，验收专用隔离 store（`/tmp` 下），跑前删除重建。
- 浏览器：Playwright Chromium（无头），桌面 1440×1000 + 移动 390×844（`isMobile`+触屏）两个上下文；`window.confirm/prompt/alert` 被注入探针，任何原生弹窗调用都会计入失败。
- 驱动方式：一次性脚本（`/tmp` 下，不进仓库）全程通过真实 UI 点击/输入与真实 API 完成场景，无服务端捷径。
- 前置门禁（全绿）：`pnpm install --frozen-lockfile && pnpm -r test`（web 212 + web-client 10，web-pages tsc）、`pnpm --dir web build`、`GOWORK=off go build/vet/test ./web/`、`make build`、`make check-static`、`gofmt -l web/` 为空。

## 逐项结果（15/15 通过）

| # | 场景 | 结果 | 证据 |
|---|------|------|------|
| 1 | 登录后总览（DEV autologin），桌面 1440×1000 | ✅ | 01-overview-desktop.png |
| 2 | 经真实 UI/API 创建工作区 `acceptance` + 环境 `prod` | ✅ | 01 |
| 3 | Flag 全生命周期：time-window 预设创建 → 结构化/JSON 双模式往返编辑 → 存草稿 → 提交 → 自批 → 发布预览 → 发布 r1 → 历史含 r1；第二轮改 salt 发布 r2 | ✅ | 02/03/04/05 |
| 4 | 危险操作（回滚到 r1）弹自研 ConfirmDialog，全程零原生 confirm | ✅（取消未执行） | 06 |
| 5 | Segment：`beta-users` 从 user-list 预设创建，结构化编辑保存草稿 | ✅ | 07 |
| 6 | 实验五步向导（对已发布 checkout-v2，变体对齐 control/candidate，指标 checkout_success，onDecision 曝光）创建成功 | ✅ | 08 |
| 7 | 实验详情四区 + 设置页签切换（aria-current 与深链 hash 同步） | ✅ | 09 |
| 8 | 旧 `diagnostics` 深链别名落到 health 区 | ✅ | 10 |
| 9 | 16 条 `#/console/{ws}/{env}/{view}/…` 冷开深链全部落在正确视图、hash 不被改写、导航高亮正确 | ✅ | 11 |
| 10 | 中/英就地切换不刷新页面、hash 保持 | ✅ | 12 |
| 11 | 主题 system/light/dark 三态：偏好持久化，`<html data-theme>` 为解析后主题 | ✅ | 13 |
| 12 | 脏编辑离开拦截：JSON 改动后返回 → ConfirmDialog → 取消后 URL 与编辑内容都保留 | ✅ | 14 |
| 13 | 移动端 390×844 总览无横向溢出（scrollWidth=390） | ✅ | 15 |
| 14 | 移动端 flag 编辑器：滚入长草稿卡后操作条吸附视口底（y=779=844−65），无横向溢出 | ✅ | 16 |
| 15 | 双上下文全程零 `window.confirm/prompt/alert` 调用、零 JS pageerror | ✅ | — |

## 验收中抓到并已修复的问题

1. **移动端吸附操作条被 `main { overflow: auto }` 静默废掉**（`web/src/styles/global.css`）：迁移新增的 `.sticky-actions`（`position: sticky; bottom: 0`，移动端专属）依赖文档滚动，而 `main` 的 overflow 容器身份让 sticky 相对一个不滚动的祖先计算，操作条完全不吸附。修复：移动端媒体查询内 `main { overflow: visible; }`（同时去掉同查询块里的 `overflow-x: auto`）。
2. **Flag 修订历史表撑破 390px 视口**：`table.grid` 有 `min-width: 660px`，`RevisionsTable`（`web-pages/src/components/RevisionsTable.tsx`）缺设计系统既有的 `.table-scroll` 横滚包装。已补；同类未包装的实验详情护栏表（`ExperimentDetailPage.tsx`）一并补上。报告页表格本就在 `#report-box`（自带 `overflow-x: auto`）内，诊断表移动端另有卡片替身，均无需处理。
3. **发布预览卡回滚目标的 64 位哈希不换行**（`ReviewPreview.tsx`）：纯文本 `<p>` 里的长哈希把文档撑到 557px。加既有 `.wrap-anywhere` 工具类解决。
4. **冷开深链作用域竞态**（`web/src/scope/ScopeProvider.tsx`，阶段 4 早期抓到）：冷加载 `#/console/{ws}/{env}/flags/{key}` 时，环境列表请求未完成会先用空 env 发请求（404）并把 URL 短暂改写成 `~/`。已按请求键控状态修复并补回归测试（`web/src/__tests__/scopeRestore.test.tsx`，2 例）。

## 边界与已知限制

- `diagnostics` 别名的行为与旧版逐分支一致：恢复时在内存里归一到 health 区，URL 本身不改写（旧控制台同样如此），旧书签可用即达标。
- 移动端吸附条的语义：`sticky; bottom: 0` 的元素不能越过其包含块（草稿卡）上沿，所以页面最顶端（草稿卡尚整体在首屏之下）操作条不悬浮；只要滚入草稿卡，整条长卡范围内操作条始终钉在视口底。这是该特性的设计语义，非缺陷。
- 未覆盖：读屏器/无障碍实测、真机（非模拟）设备、性能基线（首屏体积/交互耗时未量化）、服务端分页（列表仍为全量拉取）。
- npm 前端依赖未纳入二进制的 THIRD_PARTY_NOTICES/licenses.json 审计体系（那两个文件属他人进行中工作，未动）。
- `docs/实施状态.md` §11 单元格内仍引用已删除的 `web/i18n_contract.mjs`、`web/presets.js` 等旧证据文件路径（历史记录性质，本阶段按任务书只改两处最小点，未改写历史段落）。

## 验证

- 验收脚本退出码 0，15/15 场景组通过；服务器日志无异常。
- HTTP 冒烟：`/` 200 返回 React SPA（`index-*.js` 为当前构建哈希）；`/legacy/`、`/legacy/app.js` 走 SPA 回退（200 返回 index，旧文件已不存在）；`/assets/*` 响应 `Cache-Control: public, max-age=31536000, immutable`；`/` 为 `no-store`；`/console/...` 非 hash 路径回退 SPA。
- 全部截图与本报告同目录，编号对应上表。
