# GoLab Python SDK

`details.result.decision_id` 可用 `client.diagnose(id)` 查询同作用域的近期
求值依据和曝光/转化状态；`status()["state"]` 报告 NOT_READY、READY、ERROR、
STALE。完整字段与保留窗口见[求值诊断与事件](../../docs/求值诊断与事件.md)。

远程求值优先的 GoLab Python SDK（首版，`docs/技术方案.md` §8 受限投影）：

本 SDK 使用 GoLab HTTP API，不实现 Nacos 原生协议。官方 Nacos 客户端兼容矩阵目前只验证 Go `nacos-sdk-go/v2 v2.3.5`；见 [原生客户端支持边界](../../docs/Nacos原生客户端.md#固定版本兼容矩阵issue-17)。

- **不下发全量快照**——求值走 `POST /api/v1/sdk/eval`（可指向 `cmd/golab-proxy` 独立中继），内部规则/segment/AI 内容永不出服务端，适合脚本、Web 后端薄客户端与边缘运行时。
- **配置缓存 + 轮询**——结果按 (flagKey, context) 缓存；后台轮询 `GET /api/v1/sdk/revision`（仅 revision 数字的受限投影），revision 变化即整表失效；每次 eval 响应也携带 `manifestRevision`，活跃客户端即使轮询失败也能收敛。
- **按曝光模式采集**——`onDecision` 求值时服务端落幂等曝光；`onRender` 只记录分配，实际展示后调用 `track_evaluated_exposure(details)`。短时凭据过期后需重新求值；采集被拒会报错或进入错误回调。
- **失败绝不猜测**——传输/鉴权/类型错误一律返回调用方默认值，`Result.failed=True` 且带 reason/errorCode（`FLAG_NOT_FOUND`/`TYPE_MISMATCH`/`MISSING_BUCKET_KEY`/`ERROR`）。
- 纯标准库（`urllib`/`threading`），Python ≥ 3.9，无第三方运行时依赖。

## 安装

```bash
cd sdk/python
python3 -m venv .venv && .venv/bin/pip install -e .   # 或直接 sys.path 引用
```

## 用法

```python
from golab_sdk import GolabClient, Options

client = GolabClient(Options(
    base_url="http://127.0.0.1:8688",   # 或 golab-proxy 地址
    sdk_token="glab_sdk_...",           # 环境级 sdk 凭证（租户由凭证决定）
    collect_token="glab_collect_...",   # 独立采集凭证（转化上报）
    poll_interval=30.0,
))

value, details = client.bool_variation(
    "checkout-router", False, attributes={"user_id": uid, "tenant": "acme"})

if details.result.experiment is not None:
    if details.result.experiment.exposure_mode == "onRender":
        client.track_evaluated_exposure(details)  # 页面或服务端动作已实际生效
    client.track_conversion(details, "conversion", 1.0, session_id=sess)

client.flush()   # 立即上报排队事件（close() 也会 flush）
client.close()
```

其他 API：`string_variation` / `number_variation` / `json_variation`、
`track_exposure`（旧式外部决策曝光回填，仅适用于 onDecision）、`sync_revision()`（显式新鲜度探测）、`status()`（revision/缓存命中/错误/事件计数观测）。

`golab_sdk.bucketing` 提供 `golab-fnv1a-10000-v1` 的纯 Python 实现，用于离线复算/分析——与 Go 参考实现共享同一组黄金向量（`pkg/bucketing/testdata/golden_vectors.json`），测试逐条校验。

## 测试

集成测试运行**真实 golab 二进制**（test/boot 同款启动范式：构建 → 抓取一次性 owner token → 建租户/凭证 → 发布 flag/实验 → 真发 HTTP 请求）：

```bash
cd sdk/python
python3 -m venv .venv && .venv/bin/pip install pytest
.venv/bin/pytest tests/ -q
```

覆盖：定向/默认规则求值、四类型 variation 与类型不匹配兜底、未知 flag 兜底、传输失败/错误受众兜底、实验入组 + 服务端曝光 + 确定性重评、`MISSING_BUCKET_KEY` 兜底、转化归因进报告、collect 去重/缺 unit/epoch 混入拒绝、缓存命中与轮询失效、eval 响应 revision 联动失效、revision 投影最小化、黄金向量全量一致。

## 边界

- 首版为远程求值：每次未命中缓存的求值是一次 HTTP 调用（有超时与限流），高 QPS 本地求值请使用 Go SDK（`pkg/sdk`）。
- 不实现本地 DSL 求值——快照不下发，浏览器/脚本场景遵循 §8 受限投影要求。
