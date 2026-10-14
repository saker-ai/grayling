# 外部 Nacos 底座

Grayling 默认启动进程内 gonacos。传入 `-nacos-url` 后，Grayling 改用外部 Nacos 3.x 的 Admin HTTP API，不再启动进程内 gonacos。已按 Nacos 3.2.1 及以上版本的接口设计。

```bash
export GRAYLING_NACOS_USERNAME=grayling-admin
export GRAYLING_NACOS_PASSWORD='从密钥管理器注入的密码'
GOWORK=off go run ./cmd/grayling -nacos-url=https://nacos.example.com -sqlite=data/grayling.db
```

也可只设置 `GRAYLING_NACOS_TOKEN`，用于已有的 Bearer/OIDC 凭证；此时不要同时设置用户名和密码。临时 token 到期后需由部署系统更新并重启 Grayling。用户名/密码模式会在 token 到期前重新登录，遇到 401 时清除缓存并重试一次。来自 HTTP 请求的配置读取、管理发布与 AI 资源访问会传递请求取消信号，单次外部调用最多等待 10 秒。未配置凭证时，仅适用于关闭 Admin API 鉴权的 Nacos 环境。Grayling 的 `-auth-secret` 仍用于自身凭证与存储密钥；不要把 Nacos 密码当作该参数。

Grayling 会为每个工作空间环境自动创建 Nacos namespace（`grayling-<workspaceID>-<environmentID>`）。调用账号需要 namespace 管理权限以及这些 namespace 中配置的读写权限。Flag、segment 与 manifest 保存在 `GRAYLING_FLAGS` group；Prompt、Skill、AgentSpec 的独立版本保存在 `GRAYLING_AI` group。外部模式的 AI 资源使用 Nacos 配置中心作为内容存储，**不会登记到 Nacos 原生 AI Registry**。资源登记、固定版本、逐次读取摘要校验和撤销仍由 Grayling 提供。

Grayling 的 SDK、管理 API、控制台及对账任务继续通过 Grayling API 访问这些资源。若要让原生 Nacos SDK 直接读取 flag，客户端需要使用对应 namespace、`GRAYLING_FLAGS` group 与 Grayling 定义的 dataId，并自行配置外部 Nacos 的认证和网络访问策略。Grayling 的 `-nacos-native-auth` 只适用于内置 gonacos，外部模式不启用该选项。

外部模式不接受 `-redis`、`-nacos-native-auth`、恢复点和离线 HMAC 密钥轮换参数。现有恢复点绑定的是内置 gonacos 快照，不能证明外部 Nacos 的数据状态；请把外部 Nacos 的备份和 Grayling 平台存储备份纳入同一运维流程。切换底座不会自动迁移已有配置或 AI 版本，需单独执行数据迁移。

相关接口：[Nacos 3 Admin API](https://nacos.io/en/docs/latest/manual/admin/admin-api/)、[Nacos 鉴权说明](https://nacos.io/en/docs/latest/manual/admin/auth/)。
