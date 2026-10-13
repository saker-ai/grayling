# 跨角色发布浏览器验收

日期：2026-10-13。环境：本机 GoLab 二进制、隔离 JSON 存储、内置 gonacos、Chromium，工作空间 `acceptance/prod`。测试数据均为本地合成数据；令牌和管理员密码未写入仓库。

| 场景 | 观察结果 | 证据 |
| --- | --- | --- |
| 管理员新建 Flag | `browser-flag` 的键名输入使用页面内必填表单；保存后 API 列表出现 editing 草稿 | 浏览器快照、`GET .../flags` 200 |
| 管理员提交、独立发布者批准 | 两个隔离的 Chromium 会话分别操作，`POST .../submit` 与 `POST .../approve` 均为 200；草稿状态变为 approved | 浏览器网络记录、真实 API 读取 |
| 只读角色 | 可读 Flag 详情；列表不提供“新建”，详情不提供保存、提交、批准或发布按钮 | [只读详情截图](viewer-flag.png) |
| 发布复核与执行 | 修复未定义草稿变量后，复核弹窗可显示预览与差异；发布备注按 API 契约必填。另一条已批准的 `browser-release` 草稿经浏览器提交备注后，`POST .../publish` 返回 200 | [必填备注弹窗](publish-required.png)、[发布后详情](published.png)、浏览器网络记录 |

验收过程中发现并修复两个问题：Flag 发布弹窗引用了作用域外的 `draft`，点击时抛出 `ReferenceError`；Flag/Segment 发布备注原先标为可选，但服务端在空备注时返回 400。为避免系统原生 prompt 阻断键盘和自动化流程，新建 Flag/Segment、Flag 提交/批准/驳回改为页面内表单。

浏览器 CLI 对部分详情按钮的 `click` 只完成滚动而未触发处理器；这些按钮使用同一 Chromium 页面中的 DOM `onclick()` 调用打开弹窗，随后通过真实输入框与按钮完成提交。`browser-release` 的草稿、提交和独立批准由真实 HTTP 请求预置，发布由浏览器完成。报告只证明上述路径；409 并发冲突、回滚、网络故障和手机端跨角色流程仍待单独验收。
