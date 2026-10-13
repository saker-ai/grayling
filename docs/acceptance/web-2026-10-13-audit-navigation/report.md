# 实验放量到审计记录验收（2026-10-13）

## 交付

实验放量时间轴增加“查看完整审计记录”。审计 API 新增可与 `q`、`before` 组合的 `object` 参数，在工作空间内按对象 ID 精确匹配，再做游标分页。筛选对象写入控制台 URL，刷新和分享深链可恢复；页面显示筛选标签，并可清除回到工作空间审计列表。手机上逐条展示动作、时间、对象、操作人、环境及可展开的详情。

## 验证

- `GOLAB_TEST_PG_SKIP=1 GOWORK=off go test ./web ./internal/api ./internal/platform -count=1` 通过；`git diff --check` 通过。
- 全量 `GOLAB_TEST_PG_SKIP=1 GOWORK=off go test ./... -count=1` 中，相关包及其余已完成包通过；`test/gateway` 的 `TestTraefikGatewayE2E/F_KVRedisProvider` 因测试 Redis 在 90 秒内未就绪而失败（TCP 读取超时）。该网关链路与本次审计查询无关，未把全量测试记为通过。
- 平台测试验证 `exp-8` 不会命中 `exp-80`，组合文本搜索和游标分页仍正确；HTTP 测试验证精确筛选、跨工作空间拒绝与超长参数 400。
- 在隔离进程和真实浏览器中，从运行中 `exp-8` 的放量页进入审计，URL 为 `#/console/acceptance/prod/audit/exp-8`；页面返回 4 条记录，所有记录的对象均为 `exp-8`。清除筛选后 URL 回到审计根路径，显示工作空间的 42 条记录。未执行写操作。
- [桌面截图](desktop.png)与 [390 px 手机截图](mobile.png)记录精确筛选结果。初次手机检查发现桌面表格裁切对象列；改为手机记录卡后复测，页面无整体横向滚动，对象与动作均可见。

## 限制

对象筛选只约束精确对象 ID。审计文本搜索仍是原有的子串查询；审计记录本身的生成、保留策略及跨副本一致性不在本次变更范围内。
