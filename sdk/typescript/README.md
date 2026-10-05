# GoLab TypeScript SDK

`details.result.decisionId` 可用 `client.diagnose(id)` 查询同作用域的近期
求值依据和曝光/转化状态；`status().state` 报告 NOT_READY、READY、ERROR、
STALE。完整字段与保留窗口见[求值诊断与事件](../../docs/求值诊断与事件.md)。

远程求值优先的 GoLab TypeScript/Node SDK（首版，`docs/技术方案.md` §8 受限投影），与 Python SDK 契约完全一致：

- **不下发全量快照**——求值走 `POST /api/v1/sdk/eval`（可指向 `cmd/golab-proxy` 独立中继），内部规则/segment/AI 内容永不出服务端，适合浏览器伴随服务、Node 后端与边缘运行时。
- **配置缓存 + 轮询**——结果按 (flagKey, 规范化 context) 缓存；后台轮询 `GET /api/v1/sdk/revision`（仅 revision 数字的受限投影），revision 变化即整表失效；eval 响应携带 `manifestRevision` 联动失效。
- **曝光由服务端记录**——远程求值进入实验时服务端自动落幂等曝光；转化由 SDK 按 collect 契约批量上报（独立 collect 凭证、队列 + 定时 flush + 一次重试 + 丢弃计数）。
- **失败绝不猜测**——传输/鉴权/类型错误一律返回调用方默认值 + `failed` Result（`FLAG_NOT_FOUND`/`TYPE_MISMATCH`/`MISSING_BUCKET_KEY`/`ERROR`）。
- 零运行时依赖：只用 Node ≥ 22.6 内建能力（`fetch`、`node:crypto`）；源码即 TypeScript（Node 原生 type-stripping 直接运行）。

## 用法

```ts
import { GolabClient } from "@golab/sdk"; // 仓库内: ../src/client.ts

const client = new GolabClient({
  baseUrl: "http://127.0.0.1:8688",   // 或 golab-proxy 地址
  sdkToken: "glab_sdk_...",           // 环境级 sdk 凭证（租户由凭证决定）
  collectToken: "glab_collect_...",   // 独立采集凭证（转化上报）
  pollIntervalMs: 30_000,
});

const { value, details } = await client.boolVariation(
  "checkout-router", false, { attributes: { user_id: uid, tenant: "acme" } });

if (details.result.experiment) {
  // 已进入实验：曝光已由服务端记录（details.exposureEventId 可观测）
  client.trackConversion(details, "conversion", 1, { sessionId });
}

await client.flush();   // 立即上报排队事件
await client.close();
```

其他 API：`stringVariation` / `numberVariation` / `jsonVariation`、`trackExposure`（外部决策曝光回填）、`syncRevision()`、`status()`。

`src/bucketing.ts` 提供 `golab-fnv1a-10000-v1` 的纯 TS 实现（BigInt FNV-1a 64），与 Go 参考实现共享同一组黄金向量，测试逐条校验——用于离线复算/分析，客户端求值不依赖它。

## 测试

集成测试运行**真实 golab 二进制**（构建 → 抓取一次性 owner token → 建租户/凭证 → 发布 flag/实验 → 真发 HTTP 请求）：

```bash
cd sdk/typescript
npm install        # 仅 devDependencies（typescript/@types/node，用于 typecheck）
npm test           # node --test（原生运行 .ts）
npm run typecheck  # tsc --noEmit
```

覆盖与 Python SDK 相同的契约面：定向/默认规则、四类型 variation、类型不匹配/未知 flag/传输失败/错误受众兜底、实验入组 + 服务端曝光 + 确定性重评、`MISSING_BUCKET_KEY`、转化归因进报告、collect 去重/缺 unit/epoch 混入拒绝、缓存命中、轮询失效、eval revision 联动失效、revision 投影最小化、黄金向量全量一致。

## 边界

- 首版为远程求值：每次未命中缓存的求值是一次 HTTP 调用；高 QPS 本地求值请使用 Go SDK。
- 浏览器直接持 sdk token 不可取（token 即服务端凭证）——浏览器场景请经 `cmd/golab-proxy` 并由你的后端签发受限访问，或仅使用受限投影（§8）。
- npm 发布（构建产物 dist/）属后续工程化事项；当前以源码形式被仓库内工程直接引用。
