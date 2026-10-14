# 实验详情四区域改版浏览器验收（2026-10-14）

## 范围与环境

验收 C4 提交：实验详情重构为“状态带 + 四个可直达区域（决策摘要 / 数据健康 / 效果证据 / 放量时间轴）+ 设置与审计页签”，每区域独立 hash 深链，旧 `…/diagnostics` 深链归一化到数据健康区域；预注册契约卡原样迁入设置与审计页签；放量门禁行为不变。

- 服务：本仓库 `make build` 产出的 `bin/golab`，隔离数据目录 `/tmp/golab-c4-acceptance/store.json`（全新初始化，未触碰仓库 `data/`），监听 `127.0.0.1:19093`（内置 gonacos `19094/19095`），`GOLAB_ADMIN_PASSWORD` 设固定密码，`-rollout-interval 5s` 让放量引擎尽快启动计划。验收结束已 kill 服务进程、删除临时目录并关闭浏览器会话。
- 浏览器：`agent-browser` CLI 驱动 headless Chromium（`AGENT_BROWSER_ARGS="--no-sandbox"`），界面语言中文，桌面 1440×900、手机 390×844，截图不含令牌明文。
- 预置数据全部经真实管理 API 走真实业务流程产生（curl + 管理员/ alice 令牌，未直接改存储）：
  - 工作空间 `acceptance` / 环境 `prod`；令牌 `alice-publisher`（publisher）、`carol-viewer`（viewer）、`sdk-collect`（sdk）、`collect-writer`（collect）。
  - Flag `checkout-v2`：r1 发布 → r2 增 bucketing（unit=user_id）→ r3 增 50/50 百分比规则 → r4 实验绑定（每轮均 draft→submit→alice 批准→owner 发布）；引擎放量步骤资格变更另行产生 r5/r6。
  - `exp-9`：针对 r3 创建→评审→绑定→批准/发布绑定→running；`sdk-collect` 令牌对 15 个单元 `POST /api/v1/sdk/eval`（onDecision 真实记录曝光），`collect-writer` 令牌 `POST /api/v1/collect` 提交 5 条真实转化；报告真实给出 control 9 / treatment 6 曝光单元与主指标 p=0.025（verdict HOLD：最小运行时间未到）。
  - `exp-7`：reviewed→running，无任何采集事件（报告真实阻断原因 `no randomized exposures recorded yet`）。
  - `exp-10`：draft（onRender），用于状态带“报告时间未加载 = —”场景。
  - `exp-9` 放量计划：owner 提交 `release_plan.create` 审批 `approval-12` → alice 批准 → owner 创建 → 引擎 5s tick 内 armed→running（canary-25，手动推进）。

## 结果表

| 场景 | 证据 | 判定 |
| --- | --- | --- |
| ① 五区导航切换 + 每区深链直达 | 点击页签与直接写入 hash 两种方式均验证：`…/exp-9`（summary）、`…/exp-9/health`、`…/exp-9/evidence`、`…/exp-9/rollout`、`…/exp-9/settings`；每次 `location.hash` 与 `#exp-zone-nav [aria-current]` 一致，summary 无第 6 段；硬刷新 `…/exp-9/rollout` 后区域仍正确恢复 | 通过，[summary](desktop-summary.png)、[health](desktop-health.png)、[evidence](desktop-evidence.png)、[rollout](desktop-rollout.png)、[settings](desktop-settings.png) |
| ② 旧深链 `…/diagnostics` 兼容 | 整页重载 `#/console/acceptance/prod/experiments/exp-9/diagnostics`：恢复为数据健康区域（`#exp-zone h2` = 实验诊断、状态带恒显），地址栏归一化为 `…/exp-9/health`，刷新后不回跳 | 通过，[旧深链截图](desktop-legacy-diagnostics.png) |
| ③ 设置与审计页签 | 预注册契约卡完整（单元/互斥层/入组比例/变体/主指标/α power MDE/每臂最小样本/最短运行时间/分析窗口/曝光时机/方法/分流模式/假设）；生命周期控件全在（评审/查看 Flag 与草稿/重新检查/绑定/状态迁移 + bind-check 真实结果）；“查看该实验的审计记录”→ `#/console/acceptance/prod/audit/exp-9` 并显示“对象：exp-9（精确匹配）” | 通过，[设置页签](desktop-settings.png) |
| ④ 状态带六字段 | exp-9（running）：状态 chip + epoch 3 + Flag 版本 r3 + 注册版本 sha256:173850… + 报告时间（loadReport 成功后局部刷新为真实生成时间）+ 环境 env-3；exp-10（draft，未加载报告）：报告时间显 `—`，hero 保持“实验尚无结果…”不造数 | 通过，[draft 状态带](desktop-draft-band.png)、[桌面总图](desktop.png) |
| ⑤ 放量门禁复验（报告不可用） | 浏览器对 `**/experiments/exp-9/report` 请求中止（**浏览器合成，非服务端真实故障**）后进入放量区：门禁横幅“当前报告不可用；恢复、推进及交还自动控制已禁用……”、`#ro-advance` disabled，`#ro-pause`/`#ro-takeover-on` 仍可用；同场 summary 区 hero 走错误分支“当前分析暂不可用……”，状态带报告时间回落 `—`；解除中止并重进区域后门禁清空、`#ro-advance` 恢复可用、报告时间为真实值 | 通过，[门禁截图](desktop-rollout-gate-aborted.png) |
| ⑥ 390px 区域导航与记录卡 | `scrollWidth=innerWidth=main.scrollWidth=390`，无整页横向滚动；区域导航换行两排、按钮整宽不截断；设置页签生命周期/预注册卡单列完整；放量区桌面表隐藏、步骤+历史改为 4 张记录卡 | 通过，[手机设置页签](mobile.png)、[手机放量记录卡](mobile-rollout.png)、[手机摘要](mobile-summary.png) |
| 附：无曝光实验数据健康区 | exp-7 health 区报告摘要卡逐字展示服务端阻断原因 `no randomized exposures recorded yet`（“报告不健康”警告横幅），护栏“无”，样本流已曝光 0 单元；有曝光 exp-9 显示真实 15 曝光 / 5 转化且 `onDecision` 分配列显“—” | 通过，[无曝光](desktop-noexposure-health.png)、[有曝光](desktop-health.png) |

## 边界

- 六类状态细分首屏文案属 C5，本轮 summary 区 hero 行为未变（仍是 report/error/pre-launch 三态）；`can_advance`/`ended` 等分类浏览器场景待 C5 与真实成熟数据。
- 门禁场景的故障由浏览器请求中止合成，服务端真实报告构建失败的端到端拒绝路径由 `internal/rollout` 的 `TestOperatorActionsRequireAvailableReport` 覆盖；Web 侧 `rollout_contract.mjs` 对禁用/恢复矩阵零改动全绿。
- viewer 角色裁剪未在本轮重复验收（C2/C3 报告已覆盖，且本次未触碰权限逻辑）；导出 CSV/JSON 按钮门控与位置沿用原实现，仅迁移到效果证据区。
- 截图语言为中文；EN 文案由 `web/i18n_contract.mjs` 五个区域 sweep 双语覆盖。
- 预置产生的 `exp-8`（reviewed 残留，绑定预检失败的历史尝试）保留在列表中，未用于截图场景。

## 验证

- `node web/render_contract.mjs && node web/rollout_contract.mjs && node web/i18n_contract.mjs && node web/capability_contract.mjs && node web/flag_publish_contract.mjs`：全部通过（rollout / capability 契约零断言改动）。
- `GOWORK=off go test ./web -count=1`：ok。
- `GOWORK=off go build ./...`、`git diff --check`：通过。
