# Web 管理功能覆盖矩阵

范围：`openapi.yaml` 中的 `/api/v1/admin/` 管理操作（共 62 项）。`GET /api/v1/health` 是健康检查，不属于管理操作。SDK config/eval/collect、原生 Nacos 协议及 HMAC 原始密钥为数据面或运维密钥，刻意不进入管理 UI。

下表说明 Web 是否提供入口及代码契约，**不表示每项都已在浏览器执行**。真实浏览器的操作、截图、HTTP 结果与未覆盖项见 [2026-10-08 验收报告](acceptance/web-2026-10-08/report.md)、[跨角色发布验收](acceptance/web-2026-10-13-roles/report.md)、[故障状态模拟验收](acceptance/web-2026-10-13-failures/report.md)、[会话恢复验收](acceptance/web-2026-10-13-reauth/report.md)、[实验列表摘要验收](acceptance/web-2026-10-13-exp-list/report.md)、[实验绑定并发冲突验收](acceptance/web-2026-10-13-bind-conflict/report.md)及 [实验首屏决策验收](acceptance/web-2026-10-13-exp-decision/report.md)。

| 方法 | 管理端点 | 分类 | Web 入口 / 行为或仅 API 理由 | 验证证据 |
|---|---|---|---|---|
| `POST` | `/api/v1/admin/session` | 已覆盖 | 登录：换取 HttpOnly 管理会话 | 会话生命周期与 CSRF 测试；API 路由契约 |
| `GET` | `/api/v1/admin/session` | 已覆盖 | 页面刷新后恢复登录状态 | 会话生命周期测试；API 路由契约 |
| `DELETE` | `/api/v1/admin/session` | 已覆盖 | 顶栏退出登录 | 会话生命周期测试；API 路由契约 |
| `GET` | `/api/v1/admin/rollout/leadership` | 仅 API | 运维诊断；无租户管理动作 | OpenAPI ↔ 路由契约 |
| `POST` | `/api/v1/admin/workspaces` | 已覆盖 | 顶栏：工作空间/环境选择与创建 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces` | 已覆盖 | 顶栏：工作空间/环境选择与创建 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/capabilities` | 已覆盖 | 顶栏：无工作空间时的全局创建权限 | 能力 API/RBAC；空工作空间契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/capabilities` | 已覆盖 | 顶栏及按钮：服务端解析当前 admin 能力 | 能力 API/RBAC；角色及跨工作空间契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments` | 已覆盖 | 顶栏：工作空间/环境选择与创建 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments` | 已覆盖 | 顶栏：工作空间/环境选择与创建 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/tokens` | 已覆盖 | 令牌页：列表/创建/撤销 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/tokens` | 已覆盖 | 令牌页：列表/创建/撤销 | i18n DOM；API 路由契约 |
| `DELETE` | `/api/v1/admin/tokens/{id}` | 已覆盖 | 令牌页：列表/创建/撤销 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/audit` | 已覆盖 | 审计页：搜索、条数选择与游标翻页 | i18n DOM；游标筛选测试；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/quotas` | 已覆盖 | 配额页：owner 可见，读取/编辑/校验/确认/保存 | quota API + i18n DOM |
| `PUT` | `/api/v1/admin/workspaces/{ws}/quotas` | 已覆盖 | 配额页：owner 可见，读取/编辑/校验/确认/保存 | quota API + i18n DOM |
| `GET` | `/api/v1/admin/workspaces/{ws}/metrics` | 已覆盖 | 指标目录列表、搜索和状态筛选 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/metrics` | 已覆盖 | 新建指标表单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}` | 已覆盖 | 指标版本详情与定义哈希 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}/submit` | 已覆盖 | 详情页提交独立审批 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}/publish` | 已覆盖 | 详情页或审批队列执行已批准发布 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}/deprecate` | 已覆盖 | 详情页确认并弃用版本 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}` | 已覆盖 | 开关列表/详情；实验创建向导核对发布版本及变体 | i18n DOM；API 路由契约；向导浏览器样本验收 |
| `DELETE` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `PUT` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/draft` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/submit` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/approve` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/reject` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/publish` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/rollback` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/import-gff` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `PUT` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/draft` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/submit` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/approve` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/publish` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/experiments` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/experiments` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/experiment-summaries` | 已覆盖 | 实验列表：按页展示真实报告健康、HOLD 原因和决策状态；检索与翻页由服务端执行 | API 作用域/分页/报告测试；双语 DOM 契约 |
| `GET` | `/api/v1/admin/experiments/{id}` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/review` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/bind` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/bind-check` | 已覆盖 | 实验详情：绑定前逐项准备检查；409 后重新读取并显示变化项 | i18n DOM；绑定准备检查及冲突 DOM；跨身份浏览器验收 |
| `POST` | `/api/v1/admin/experiments/{id}/transition` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/report` | 已覆盖 | 运行中实验首屏自动显示报告结论、阻断、生成时间和水位；完整报告按需展开 | i18n DOM；报告 DOM；真实浏览器验收；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/report/export` | 已覆盖 | 实验详情：只读 JSON/CSV 报告导出 | 报告 DOM；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/diagnostics` | 已覆盖 | 实验详情独立诊断页：水位、分配/曝光、SRM、污染与阻断项 | i18n DOM；API 路由契约；待真实浏览器验收 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/release-plan` | 仅 API | 实验详情已从实验读模型展示计划；独立读取供自动化客户端 | OpenAPI ↔ 路由契约；rollout DOM |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/pause` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/resume` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/advance` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/takeover` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/debug/eval` | 已覆盖 | 调试页：无曝光求值 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}` | 已覆盖 | AI 资源详情：所有权、固定版本历史、撤销状态及配置来源；无原始内容 | 只读/跨空间 API 测试；渲染契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/pin` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/revoke` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/reinstate` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resolve` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `PUT` | `/api/v1/admin/workspaces/{ws}/model-allowlist` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/model-allowlist` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/approvals` | 已覆盖 | 审批队列/详情：申请、批准、执行 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/approvals/{id}/approve` | 已覆盖 | 审批队列/详情：申请、批准、执行 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/approvals/{id}/reject` | 已覆盖 | 审批详情：异人拒绝并保留原因 | 审批状态与 API 测试；i18n DOM |
| `GET` | `/api/v1/admin/workspaces/{ws}/approvals/{id}/comparison` | 已覆盖 | 审批详情：回滚前后版本及新放量计划差异、过期基线提示；AI 内容不回传 | 权限与 API 测试；i18n DOM |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/approvals` | 已覆盖 | 审批队列/详情：申请、批准、执行 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/preview` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/dependencies` | 已覆盖 | Segment 详情：当前发布 Flag 的直接和嵌套引用 | 管理服务与 API 测试；i18n DOM |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/match-preview` | 已覆盖 | Segment 详情：样本上下文的草稿与发布版命中对照，无曝光 | 求值器、管理服务和 API 测试；i18n DOM |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/history` | 已覆盖 | Segment 详情：保留的发布版本、当前生效版本与字段差异 | 管理服务和 API 测试；i18n DOM |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/emergency-disable` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |

## 横向核对

- **权限**：全局能力 GET 支持空工作空间冷启动；工作空间能力 GET 解析目标工作空间并用已认证 admin actor 逐项调用 `Authorize`；作用域外返回 403。Web 在连接和切换工作空间时加载能力，按 `workspace.admin`、`token.manage`、`quota.manage`、`flag.write/approve/publish`、`experiment.write`、`ai.write/content.read` 门控入口与动作。服务端保留每个写请求的 RBAC 和对象状态校验。
- **错误**：403、409、429 共用中英反馈。403 重新加载能力；409 指示重新读取当前状态并应用修改；429 指示限流后重试。能力请求失败时动作默认不可用。
- **登录**：同源 HTTPS（或本机回环地址）可用服务器设置的 HttpOnly、SameSite=Strict 会话 Cookie；跨源连接仅在当前页面内存保存管理员凭据，刷新即失效。旧版保存在 localStorage 的凭据会在启动时删除。Cookie 写请求校验 Origin；撤销原始管理员令牌会立即使会话失效。
- **危险动作**：令牌撤销、flag 删除/回滚/紧急禁用、AI 撤销和审批操作使用可读目标与影响的页面内确认框；审批自批仍由服务端拒绝。放量控制保留原有的确认与理由步骤。
- **编辑与检索**：flag、segment 草稿在切换视图、作用域或离开页面时提醒未保存修改。flag、segment、experiment 列表支持键盘操作和页内筛选；审计查询在服务端筛选并通过游标翻页。
- **中英文本**：`web/i18n_contract.mjs` 和能力 DOM 契约覆盖双语文本、权限动作与语言切换。
- **测试列含义**：DOM 契约验证页面入口和行为，API 契约验证服务端工作空间权限；不代表外部部署的现场验收。
