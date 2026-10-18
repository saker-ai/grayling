# perf summary (raw: perf-results.json)

| page | cold FCP | cold LCP | cold load | cold TTI-proxy | warm FCP | warm LCP | warm load | warm TTI-proxy |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| overview | 228ms | 344ms | 99ms | 214ms | 208ms | 328ms | 85ms | 197ms |
| flags | 228ms | 320ms | 101ms | 217ms | 212ms | 304ms | 88ms | 207ms |
| flag-detail | 220ms | 408ms | 98ms | 215ms | 216ms | 392ms | 89ms | 190ms |
| experiments | 220ms | 220ms | 101ms | 214ms | 208ms | 208ms | 86ms | 182ms |
| exp-detail | 224ms | 300ms | 104ms | 207ms | 208ms | 268ms | 91ms | 200ms |
| ai | 216ms | 320ms | 98ms | 216ms | 200ms | 300ms | 88ms | 194ms |

- 1000-flag filter latency median: 17.8ms (n=5)
- experiment report longtasks: 1 entries >50ms, total 53.0ms

## budgets

- index-Du8ZIdKt.js: 445337 / 471040 bytes PASS
- index-tZBqJxoB.css: 32003 / 32768 bytes PASS
- web-pages/src/viz/**: 15360 / 15360 bytes PASS
- lockfile runtime deps: added=0 removed=0 PASS
