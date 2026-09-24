# GoLab × Traefik 3.6 流量灰度架构（multi-layer routing）

> 依据 Traefik 官方 multi-layer routing 文档与 PR #12130（milestone 3.6）设计。
> 该方案替代早期"local plugin 内嵌 SDK 写 header"的不成立路线；ForwardAuth 为标准 middleware，无 Yaegi 兼容性问题。

## 请求链路总览

```mermaid
flowchart TB
    Client["客户端<br/>携带 bucketingKey<br/>(userID / deviceID / sessionID)"]

    subgraph TF["Traefik ≥3.6 · multi-layer routing"]
        direction TB
        EP["EntryPoint<br/>websecure"]
        subgraph PARENT["父 Router: api-parent（无 service，独占 entryPoints/tls/models）"]
            direction TB
            MW1["① headers middleware<br/>X-Golab-Variant 置空<br/>（剥离外部伪造变体）"]
            MW2["② forwardAuth middleware"]
            MW1 --> MW2
        end
        subgraph CHILD["子 Router 层（按修改后的 header 匹配，恰好命中一个）"]
            direction LR
            RC["api-canary<br/>Header(`X-Golab-Variant`, `canary`)"]
            RD["api-default<br/>PathPrefix(`/api`) 兜底"]
        end
        EP --> MW1
        MW2 --> RC
        MW2 --> RD
    end

    subgraph EVALSVC["GoLab 求值服务（P4 远程评估 proxy · 独立进程）"]
        direction TB
        FA["/evaluate 端点<br/>fail-open：内部错误时返回默认变体"]
        BUCKET["原生评估器<br/>固定分桶 mod 10000<br/>实验互斥 / 分层"]
        SNAP["本地配置快照<br/>版本化 manifest · 可离线求值"]
        FA --> BUCKET --> SNAP
    end

    subgraph CTRL["GoLab 控制面"]
        direction TB
        ADMIN["管理 API<br/>创建 → 审核 → 发布 → 回滚<br/>灰度比例调整只改分桶参数"]
        EVENTS["实验事件管道<br/>曝光（决策点记录）/ 转化"]
        PG[("PostgreSQL<br/>实验与事件库")]
        EVENTS --> PG
    end

    GONACOS[("gonacos<br/>配置存储 / 历史 / 推送<br/>AI 资源版本")]

    V2["service-v2（灰度版本）"]
    V1["service-v1（稳定版本）"]

    Client -->|"① 请求"| EP
    MW2 -->|"② 求值请求（bucketingKey）"| FA
    FA -->|"② 回写 X-Golab-Variant<br/>X-Golab-Flag-Key"| MW2
    FA -.->|"③ 曝光事件（集中归因）"| EVENTS
    RC -->|"④a 转发"| V2
    RD -->|"④b 转发"| V1

    ADMIN -->|"发布 flag / 实验配置"| GONACOS
    GONACOS -->|"gRPC 推送 + 轮询兜底"| SNAP
```

## 关键设计性质

| 性质 | 说明 |
|---|---|
| 比例调整零配置传播 | 5% → 20% 只改 GoLab 侧分桶参数，Traefik 动态配置不动，无 provider 传播延迟 |
| 与 SDK 同算法 | 求值服务与应用内 Go SDK 使用同一原生评估器和黄金向量，同一 bucketingKey 两层结果一致 |
| 曝光集中记录 | ForwardAuth 决策点产生曝光事件，直接写入 P2 实验事件管道，归因不依赖后端各自上报 |
| 复用 P4 组件 | 求值服务即技术方案 P4 的远程评估 proxy，Traefik 集成（P5）只是新增一个消费方 |

## 约束与必须处理的风险

1. **Provider 限制**：multi-layer routing 仅支持 file、KV（Consul/etcd/Redis/ZooKeeper）、Kubernetes CRD（IngressRoute）；Docker label、K8s Ingress、Gateway API 不支持。GoLab 管理面需将实验 router 配置生成到受支持的目标。
2. **层级规则**：根 router 独占 `entryPoints`/`tls`/models；中间 router 不得定义 service；叶子 router 必须有 service。控制面生成配置时须按此校验。
3. **求值服务可用性**：ForwardAuth 失败默认拒绝请求。求值服务须高可用部署，且内部错误 fail-open 返回默认变体，避免网关 5xx；对应 P5 验收的"紧急撤销安全降级"。
4. **安全边界**：外部请求自带的 `X-Golab-Variant` 必须在父 router middleware 链最前剥离；变体 header 只接受求值服务经 `authResponseHeaders` 回写的值。

## 参考

- Traefik multi-layer routing 文档：`docs/content/reference/routing-configuration/http/routing/multi-layer-routing.md`（traefik/traefik master）
- PR #12130（merged 2025-10-22，milestone 3.6）：<https://github.com/traefik/traefik/pull/12130>
