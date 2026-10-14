# IngressRoute 真实集群验收

`test/gateway/k8s/run.sh` 在独立的 kind 集群中验证 `internal/gateway.IngressRoutes` 的原样输出。测试使用真实 Grayling/gonacos 发布流程、集群内 Traefik 和 proxy，以及两个集群内 HTTP 后端。Grayling 控制面运行在宿主机，由 proxy 经 kind 网桥访问；这样可复用 file/KV 验收的同一 flag 发布和分桶数据。测试结束会删除由脚本创建的集群；`KEEP_CLUSTER=1` 保留集群供排查。

## 固定版本与运行

环境需要 Linux/amd64、Docker、kubectl、curl、sha256sum、Go 1.27 和 `rg`。运行：

```bash
bash test/gateway/k8s/run.sh
```

脚本固定 kind v0.27.0（下载 SHA-256 `a6875aae…`）、Kubernetes v1.32.2 节点镜像摘要 `142f5435…`、Traefik v3.7.13 镜像摘要 `24841fe2…`、Python 后端镜像摘要 `1b668429…`，并校验官方 v3.7.13 CRD 文件 SHA-256 `1d5e8558…`。安装方式是直接应用该 CRD，再应用 [stack.yaml](../test/gateway/k8s/stack.yaml)：命名空间内的 Traefik Deployment 仅启用 Kubernetes CRD provider，禁用集群范围资源发现，监听 `web` entryPoint。集群命名为 `grayling-issue12`，测试通过 `--context kind-grayling-issue12` 操作，避免修改当前 kubectl context。脚本创建的集群在结束时删除；已有同名集群会复用并保留。

`test/gateway/k8s_e2e_test.go` 由脚本用 `-tags=integration,k8s` 显式执行，不会进入常规 `go test ./...`。测试把 Grayling 绑定到宿主机网桥可达地址；只在隔离的临时环境执行。proxy 使用测试生成的 SDK token 和共享路由 secret；部署生成的 IngressRoute 前先等待所有 Pod Ready。

## 验收结果（2026-10-06）

| 场景 | 真实请求结果 |
| --- | --- |
| 初始 50/50 | 100 个键稳定/金丝雀 58/42；每键重复请求路由一致；POST 原方法到达后端 |
| 伪造 `X-Grayling-*` | 稳定键携带伪造变体、flag 和 fail-open 头后仍到稳定后端，后端只收到求值回写值 |
| 50/50 → 0/100 | 30/30 到金丝雀；IngressRoute YAML SHA-256 前后均为 `54b77ba3026ff6443c55e3b24c91951cb3a72fea289a2133f7789d99f8f0ef15` |
| 金丝雀缩容 1 → 0 → 1 | 选中的金丝雀无端点时返回 503，不静默改送稳定版；恢复后继续返回金丝雀 |
| CRD 更新与回滚 | 父路由规则 `/api` → `/v2` 后新路径 200、旧路径 404；回滚后 `/api` 再次 200，配置哈希恢复；Traefik Pod 未重启 |
| proxy 求值超时 | 代理仍可用时，默认稳定版 200，`X-Grayling-Fail-Open: upstream_timeout` |
| Grayling 停机 | 默认稳定版 200，`X-Grayling-Fail-Open: upstream_unreachable` |
| proxy 进程不可用 | forwardAuth 无法连接时，Traefik 返回 500，无后端收到请求 |

该组关键断言与 [file/KV 真实 Traefik 验证](实施状态.md#网关集成验证证据)一致。它们共享 `gatewayFlag`、发布客户端和同一 `RouteSpec` 生成逻辑。集群测试额外检查了 EndpointSlice 缩容、CRD watch 收敛和 proxy 进程不可用。proxy 进程不可用不属于 Grayling 求值 fail-open：Traefik forwardAuth 在无法联系认证服务时拒绝请求。生产环境要为 proxy 配置多副本和可用性监控。

## 权限与网络边界

Traefik ServiceAccount 仅绑定命名空间 Role，只有实际 CRD provider informer 所需的 `get/list/watch`，没有写入权限，也没有 ClusterRole。`disableClusterScopeResources=true` 使其无需读取 Node/IngressClass。proxy/后端关闭 ServiceAccount token 自动挂载。

清单提供命名空间范围的 NetworkPolicy：允许测试入口访问 Traefik 8000，同命名空间组件互通，DNS、Kubernetes API 和 proxy 到宿主机网桥的临时 Grayling 端口可达。因 Grayling 测试端口每次随机分配，网桥出口允许该单一主机 IP 的 TCP 端口范围；在固定端口部署时应缩窄该规则。**kind 默认 CNI 不执行 NetworkPolicy**，本次验收验证了策略对象可被 API server 接受，未声称已验证网络隔离执行；生产部署需使用支持 NetworkPolicy 的 CNI 并另行验证拒绝路径。

上游依据：[Traefik v3.7 Kubernetes CRD provider](https://doc.traefik.io/traefik/v3.7/reference/install-configuration/providers/kubernetes/kubernetes-crd/)、[IngressRoute parentRefs 语义](https://doc.traefik.io/traefik/reference/routing-configuration/kubernetes/crd/http/ingressroute/)、[kind 节点镜像版本固定方法](https://kind.sigs.k8s.io/docs/user/configuration/)。
