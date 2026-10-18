# perf summary (raw: perf-results.json)

| page | cold FCP | cold LCP | cold load | cold TTI-proxy | warm FCP | warm LCP | warm load | warm TTI-proxy |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| overview | 220ms | 324ms | 99ms | 212ms | 204ms | 320ms | 82ms | 190ms |
| flags | 220ms | 300ms | 99ms | 206ms | 208ms | 304ms | 87ms | 192ms |
| flag-detail | 224ms | 384ms | 104ms | 214ms | 204ms | 364ms | 86ms | 199ms |
| experiments | 220ms | 220ms | 103ms | 213ms | 204ms | 204ms | 88ms | 194ms |
| exp-detail | 216ms | 292ms | 102ms | 215ms | 204ms | 272ms | 81ms | 199ms |
| ai | 212ms | 316ms | 102ms | 205ms | 196ms | 296ms | 83ms | 184ms |

- 1000-flag filter latency median: 15.7ms (n=5)
- experiment report longtasks: 0 entries >50ms, total 0.0ms

## budgets

- index-C99EfY_W.js: 444402 / 471040 bytes PASS
- index-tZBqJxoB.css: 32003 / 32768 bytes PASS
- web-pages/src/viz/**: 15360 / 15360 bytes PASS
- lockfile runtime deps: added=0 removed=0 PASS
