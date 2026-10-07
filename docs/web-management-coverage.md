# Web 管理功能覆盖矩阵

范围：`openapi.yaml` 中的 `/api/v1/admin/` 管理操作（共 53 项）。`GET /api/v1/health` 是连接检查，Web 登录时调用，不属于管理操作。SDK config/eval/collect、原生 Nacos 协议及 HMAC 原始密钥为数据面或运维密钥，刻意不进入管理 UI。

| 方法 | 管理端点 | 分类 | Web 入口 / 行为或仅 API 理由 | 验证证据 |
|---|---|---|---|---|
| `GET` | `/api/v1/admin/rollout/leadership` | 仅 API | 运维诊断；无租户管理动作 | OpenAPI ↔ 路由契约 |
| `POST` | `/api/v1/admin/workspaces` | 已覆盖 | 顶栏：工作空间/环境选择与创建 | i18n DOM；API 路由契约 |
| `GET` | `/api/v1/admin/workspaces` | 已覆盖 | 顶栏：工作空间/环境选择与创建 | i18n DOM；API 路由契约 |
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

- **权限**：管理路由先验证 admin 凭证，服务端再按工作空间与操作授权。配额 GET/PUT 均使用 `quota.manage`，只有 owner 可用。Web 通过配额 GET 成功与否控制入口；其他管理页若显示无权限动作，服务端仍返回 403，后续改进见下方 issue。
- **错误**：API 统一映射 400/401/403/404/409/429/500；配额页对 403、409、429 有专门反馈，409 提示刷新后重试，表单保留编辑值。API 契约与路由由 `TestOpenAPISpecMatchesRouteTable` 核对。
- **危险动作**：配额保存、flag 回滚/紧急禁用/删除、令牌撤销、放量控制、AI 撤销使用确认或理由提示。高影响计划与 AI pin 经审批流执行；自批由服务端拒绝。
- **中英文本**：`web/i18n_contract.mjs` 的双语视图与字典完整性检查覆盖主要视图及配额页；固定 UI 文案位于 `web/i18n.js`。
- **测试列含义**：列出的 DOM 契约是入口和主要行为证据，不能解释成每个端点都有独立端到端测试；配额另有 API 权限/默认值/持久化测试。

## 后续缺口

- **P2 [#19](https://github.com/godeps/golab/issues/19)：角色感知管理 UI**：现有令牌、审批、AI 等页可能向非 owner 显示不可执行按钮，依赖服务端 403。补充服务端能力读模型与按钮门控，并测试角色切换、403、409、429 与双语提示。
