# 跨角色发布浏览器验收

日期：2026-10-13。环境：本机 Grayling 二进制、隔离 JSON 存储、内置 gonacos、Chromium，工作空间 `acceptance/prod`。测试数据均为本地合成数据；令牌和管理员密码未写入仓库。

| 场景 | 观察结果 | 证据 |
| --- | --- | --- |
| 管理员新建 Flag | `browser-flag` 的键名输入使用页面内必填表单；保存后 API 列表出现 editing 草稿 | 浏览器快照、`GET .../flags` 200 |
| 管理员提交、独立发布者批准 | 两个隔离的 Chromium 会话分别操作，`POST .../submit` 与 `POST .../approve` 均为 200；草稿状态变为 approved | 浏览器网络记录、真实 API 读取 |
| 只读角色 | 可读 Flag 详情；列表不提供“新建”，详情不提供保存、提交、批准或发布按钮 | [只读详情截图](viewer-flag.png) |
| 发布复核与执行 | 修复未定义草稿变量后，复核弹窗可显示预览与差异；发布备注按 API 契约必填。另一条已批准的 `browser-release` 草稿经浏览器提交备注后，`POST .../publish` 返回 200 | [必填备注弹窗](publish-required.png)、[发布后详情](published.png)、浏览器网络记录 |
| 409 并发草稿冲突 | 管理员 A 编辑期间，发布者 B 保存新草稿；A 保存得到 409。本地输入保持不变，页面逐字段对照服务端草稿与本地输入，并明确再次保存会替换服务端草稿；A 确认后重试返回 200 | [冲突状态](conflict-409.png)、[草稿对照](conflict-comparison.png)、浏览器网络记录 |
| 双人回滚 | r2 发布后，管理员在 Flag 历史中申请回滚 r1；独立发布者在审批详情批准；管理员执行后 `POST .../rollback` 返回 200，服务端当前生效版本为 r1。已执行的审批详情不再误报“来源变更、无法执行” | [审批复核](rollback-review.png)、[执行结果](rollback-executed.png)、真实 API 读取 |

验收过程中发现并修复三处问题：Flag 发布弹窗引用了作用域外的 `draft`，点击时抛出 `ReferenceError`；Flag/Segment 发布备注原先标为可选，但服务端在空备注时返回 400；回滚执行后比较接口因生效版本改变标记 stale，页面错误地显示“无法执行”。为避免系统原生 prompt 阻断键盘和自动化流程，新建 Flag/Segment、Flag 提交/批准/驳回改为页面内表单。

浏览器 CLI 对部分详情按钮的 `click` 只完成滚动而未触发处理器；这些按钮使用同一 Chromium 页面中的 DOM `onclick()` 调用打开弹窗，随后通过真实输入框与按钮完成提交。`browser-release` 的首次草稿、提交和独立批准由真实 HTTP 请求预置，发布由浏览器完成；第二版发布也经真实 HTTP 请求预置，以验收浏览器回滚。报告只证明上述路径；网络故障和手机端跨角色流程仍待单独验收。
