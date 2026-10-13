# 实验诊断页浏览器验收

日期：2026-10-13。代码基线：本次独立诊断页改动（提交前工作树）。

使用本机 GoLab 服务、SQLite 存储和 Chromium 检查。数据为合成的 `diagnostic-qa/dev` 工作空间、已发布的 `qa-experiment` Flag 与 Draft 状态的 `exp-5` onRender 实验。服务端诊断请求 `GET /api/v1/admin/experiments/exp-5/diagnostics` 返回 200；页面没有使用接口模拟。浏览器通过同源会话登录，截图不含令牌明文。

| 场景 | 证据 | 结果 |
| --- | --- | --- |
| 直接打开诊断深链 | 登录后恢复 `#/console/diagnostic-qa/dev/experiments/exp-5/diagnostics`，显示状态、水位、问题和各变体计数 | 通过 |
| 详情往返 | “返回实验”进入预注册详情；点击“打开诊断”回到同一深链，网络记录为真实诊断 API 200 | 通过 |
| 空数据表达 | Draft 实验尚无开始时间和事件；页面显示窗口不完整、无分配/曝光事件及原因，不宣称 SRM 健康 | 通过 |
| 1440px 桌面 | 表格完整显示变体、分配、曝光、未曝光和转化 | [截图](diagnostics-desktop.png) |
| 390px 手机 | 表格换成逐变体记录卡，五项信息均可读；`scrollWidth=innerWidth=390`，无整页横向溢出 | [全页截图](diagnostics-mobile-full.png) |

本次只证明独立诊断页在真实服务和空数据状态下的导航与呈现。分配 SRM、曝光 SRM、污染和 bandit 冻结由 Go 定向测试及 Web DOM 契约验证；仍需使用真实采集流量在浏览器复核非空诊断。PostgreSQL 多副本和生产身份链路不在本次验收范围内。
