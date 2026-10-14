# 会话失效后的未保存输入验收

日期：2026-10-13。使用本机 Grayling 服务、隔离 JSON 数据和 Chromium，在 `acceptance/prod/browser-release` Flag 详情编辑未保存的分流盐。浏览器临时将该次保存请求的响应模拟为 401；后续重新登录和对象加载使用真实服务端。模拟仅用于稳定复现会话失效，不代表服务端实际过期。

| 场景 | 观察结果 | 证据 |
| --- | --- | --- |
| 同一凭证重新登录 | 401 后出现登录页；用原管理员密码重登后重新读取服务端草稿，本地未保存 JSON 作为只读副本显示，两份内容可以分别查看 | [恢复截图](same-principal-recovered.png)、浏览器 DOM 与真实 `GET /api/v1/admin/session` |
| 换一位管理员重新登录 | 用另一枚发布者令牌重登后，主体 ID 不同；内存副本清除，页面正文、URL 和 localStorage 均未出现原本地输入 | [隔离截图](different-principal-no-copy.png)、浏览器 DOM 检查 |
| 离开原对象 | 同主体恢复后返回 Flag 列表，副本卡片和内存副本均被清除 | 最新构建的浏览器 DOM 检查 |

`GET /api/v1/admin/session` 现在返回不含令牌明文的 `principalId`。恢复只在主体 ID、工作空间、环境、页面和对象均相同时展示；副本仅留在当前标签页内存中，不自动提交，也不直接覆盖重新加载的服务端草稿。Flag、Segment 的 JSON 输入与实验创建向导的表单值有内存捕获路径；本次浏览器仅执行了 Flag。真实 Cookie 过期、Segment/向导恢复、403 权限变化和移动端重登仍需单独验收。
