# Agent WebSocket SDK 验证记录

日期：2026-10-16。基线：`41b16d7`。分支：`feat/agent-ws-sdk`。

此记录只证明 Go 客户端及测试服务的契约行为，不代表生产 Agent 服务端、注册中心或 gogw 的集成验收。

| 检查 | 结果 |
| --- | --- |
| `make test-agent-sdk` | PASS：真实 WSS 测试与 race detector |
| `go test -race ./pkg/agent ./api/agent/v1 -count=5` | PASS：重复测试；pkg/agent 当次语句覆盖率 79.7%，生成代码不作为覆盖目标 |
| `GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test -short ./ ./cmd/... ./internal/... ./pkg/... ./web` | PASS；PG 已显式跳过，不证明多副本 |
| `GOWORK=off go vet ./...` | PASS |
| `make sdk-test` | PASS：现有 Python 与 TypeScript SDK 回归 |
| `make check-agent-generated` | PASS：固定生成工具重复生成一致 |
| `make license-check` / `make release-check` | PASS：依赖清单同步 |
| `git diff --check` | PASS |

传输测试覆盖并发关联、握手验证、心跳超时及独立 pending 名额、父 context 取消、幂等 Close、事件背压、fencing、文本帧拒绝、远端结构化错误、GoAway 所有权与重连延迟、401 永久失败、断线重连、typed 注册/续租/配置/采集/订阅/恢复/Applied；下载测试覆盖作用域、摘要、过期、大小和 origin/重定向边界。原公共资源签名及 Python 生成的跨语言 fixture 验证一并通过。

运行临时日志位于 `/tmp/grayling-ws-sdk-*.log`，不是可分发证据；此变更未附带容量、外部 Nacos 或生产服务端运行结果。后续按完整实施方案建立版本化 profile 与机器可读证据包。
