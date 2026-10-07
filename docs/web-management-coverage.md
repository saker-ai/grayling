# Web 管理功能覆盖矩阵

范围：`openapi.yaml` 中的 `/api/v1/admin/` 管理操作（共 53 项）。`GET /api/v1/health` 是连接检查，Web 登录时调用，不属于管理操作。SDK config/eval/collect、原生 Nacos 协议及 HMAC 原始密钥为数据面或运维密钥，刻意不进入管理 UI。

| 方法 | 管理端点 | 分类 | Web 入口 / 行为或仅 API 理由 | 验证证据 |
|---|---|---|---|---|
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
| `GET` | `/api/v1/admin/workspaces/{ws}/audit` | 已覆盖 | 审计页：读取与条数选择 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/quotas` | 已覆盖 | 配额页：owner 可见，读取/编辑/校验/确认/保存 | quota API + i18n DOM |
| `PUT` | `/api/v1/admin/workspaces/{ws}/quotas` | 已覆盖 | 配额页：owner 可见，读取/编辑/校验/确认/保存 | quota API + i18n DOM |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |
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
| `GET` | `/api/v1/admin/experiments/{id}` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/review` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/bind` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/transition` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/report` | 已覆盖 | 实验列表/详情：创建、评审、绑定、状态、报告 | i18n DOM；报告 DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `GET` | `/api/v1/admin/experiments/{id}/release-plan` | 仅 API | 实验详情已从实验读模型展示计划；独立读取供自动化客户端 | OpenAPI ↔ 路由契约；rollout DOM |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/pause` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/resume` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/advance` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/experiments/{id}/release-plan/takeover` | 已覆盖 | 实验详情：放量计划、暂停/恢复/推进/接管 | rollout DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/debug/eval` | 已覆盖 | 调试页：无曝光求值 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/pin` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/revoke` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/reinstate` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/ai/resolve` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `PUT` | `/api/v1/admin/workspaces/{ws}/model-allowlist` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/model-allowlist` | 已覆盖 | AI 资源页：注册、解析、版本、撤销、恢复、模型白名单 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/approvals` | 已覆盖 | 审批队列/详情：申请、批准、执行 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/approvals/{id}/approve` | 已覆盖 | 审批队列/详情：申请、批准、执行 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/approvals` | 已覆盖 | 审批队列/详情：申请、批准、执行 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/preview` | 已覆盖 | 分段列表/详情：草稿、预览、审批、发布 | i18n DOM；API 路由契约 |
| `POST` | `/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/emergency-disable` | 已覆盖 | 开关列表/详情：草稿、审批、发布、回滚、删除、导入、紧急禁用 | i18n DOM；API 路由契约 |

## 横向核对

- **权限**：全局能力 GET 支持空工作空间冷启动；工作空间能力 GET 解析目标工作空间并用已认证 admin actor 逐项调用 `Authorize`；作用域外返回 403。Web 在连接和切换工作空间时加载能力，按 `workspace.admin`、`token.manage`、`quota.manage`、`flag.write/approve/publish`、`experiment.write`、`ai.write/content.read` 门控入口与动作。服务端保留每个写请求的 RBAC 和对象状态校验。
- **错误**：403、409、429 共用中英反馈。403 重新加载能力；409 指示重新读取当前状态并应用修改；429 指示限流后重试。能力请求失败时动作默认不可用。
- **危险动作**：令牌撤销、flag 删除/回滚/紧急禁用、放量控制、AI 撤销已有确认或理由；审批队列批准需理由和确认，执行需逐项确认并展示目标、状态、影响、回滚和护栏。审批自批仍由服务端拒绝。
- **中英文本**：`web/i18n_contract.mjs` 和能力 DOM 契约覆盖双语文本、权限动作与语言切换。
- **测试列含义**：DOM 契约验证页面入口和行为，API 契约验证服务端工作空间权限；不代表外部部署的现场验收。
