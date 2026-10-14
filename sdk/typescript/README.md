# Grayling TypeScript SDK

`details.result.decisionId` 可用 `client.diagnose(id)` 查询同作用域的近期
求值依据和曝光/转化状态；`status().state` 报告 NOT_READY、READY、ERROR、
STALE。完整字段与保留窗口见[求值诊断与事件](../../docs/求值诊断与事件.md)。

远程求值优先的 Grayling TypeScript/Node SDK（首版，`docs/技术方案.md` §8 受限投影），与 Python SDK 契约完全一致：

本 SDK 使用 Grayling HTTP API，不实现 Nacos 原生协议。官方 Nacos 客户端兼容矩阵目前只验证 Go `nacos-sdk-go/v2 v2.3.5`；见 [原生客户端支持边界](../../docs/Nacos原生客户端.md#固定版本兼容矩阵issue-17)。

- **不下发全量快照**——求值走 `POST /api/v1/sdk/eval`（可指向 `cmd/grayling-proxy` 独立中继），内部规则/segment/AI 内容永不出服务端，适合浏览器伴随服务、Node 后端与边缘运行时。
- **配置缓存 + 轮询**——结果按 (flagKey, 规范化 context) 缓存；后台轮询 `GET /api/v1/sdk/revision`（仅 revision 数字的受限投影），revision 变化即整表失效；eval 响应携带 `manifestRevision` 联动失效。
- **按曝光模式采集**——`onDecision` 求值时服务端落幂等曝光；`onRender` 只记录分配，实际展示后调用 `trackEvaluatedExposure(details)`。短时凭据过期后需重新求值；采集被拒会报错或进入错误回调。
- **失败绝不猜测**——传输/鉴权/类型错误一律返回调用方默认值 + `failed` Result（`FLAG_NOT_FOUND`/`TYPE_MISMATCH`/`MISSING_BUCKET_KEY`/`ERROR`）。
- 零运行时依赖：只用 Node ≥ 22.6 内建能力（`fetch`、`node:crypto`）；源码即 TypeScript（Node 原生 type-stripping 直接运行）。

## 用法

```ts
import { GraylingClient } from "@grayling/sdk"; // 仓库内: ../src/client.ts

const client = new GraylingClient({
  baseUrl: "http://127.0.0.1:8688",   // 或 grayling-proxy 地址
  sdkToken: "grayling_sdk_...",           // 环境级 sdk 凭证（租户由凭证决定）
  collectToken: "grayling_collect_...",   // 独立采集凭证（转化上报）
  pollIntervalMs: 30_000,
});

const { value, details } = await client.boolVariation(
  "checkout-router", false, { attributes: { user_id: uid, tenant: "acme" } });

if (details.result.experiment) {
  if (details.result.experiment.exposureMode === "onRender") {
    client.trackEvaluatedExposure(details); // 页面或服务端动作已实际生效
  }
  client.trackConversion(details, "conversion", 1, { sessionId });
}

await client.flush();   // 立即上报排队事件
await client.close();
```

其他 API：`stringVariation` / `numberVariation` / `jsonVariation`、`trackExposure`（旧式外部决策曝光回填，仅适用于 onDecision）、`syncRevision()`、`status()`。

`src/bucketing.ts` 提供 `grayling-fnv1a-10000-v1` 的纯 TS 实现（BigInt FNV-1a 64），与 Go 参考实现共享同一组黄金向量，测试逐条校验——用于离线复算/分析，客户端求值不依赖它。

## 测试

集成测试运行**真实 grayling 二进制**（构建 → 抓取一次性 owner token → 建租户/凭证 → 发布 flag/实验 → 真发 HTTP 请求）：

```bash
cd sdk/typescript
npm install        # 仅 devDependencies（typescript/@types/node，用于 typecheck）
npm test           # node --test（原生运行 .ts）
npm run typecheck  # tsc --noEmit
```

覆盖与 Python SDK 相同的契约面：定向/默认规则、四类型 variation、类型不匹配/未知 flag/传输失败/错误受众兜底、实验入组 + 服务端曝光 + 确定性重评、`MISSING_BUCKET_KEY`、转化归因进报告、collect 去重/缺 unit/epoch 混入拒绝、缓存命中、轮询失效、eval revision 联动失效、revision 投影最小化、黄金向量全量一致。

## 边界

- 首版为远程求值：每次未命中缓存的求值是一次 HTTP 调用；高 QPS 本地求值请使用 Go SDK。
- 浏览器直接持 sdk token 不可取（token 即服务端凭证）——浏览器场景请经 `cmd/grayling-proxy` 并由你的后端签发受限访问，或仅使用受限投影（§8）。
- npm 发布（构建产物 dist/）属后续工程化事项；当前以源码形式被仓库内工程直接引用。
