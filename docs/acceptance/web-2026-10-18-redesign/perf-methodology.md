# perf methodology (R5A.2)

Raw numbers: `perf-results.json`; table fragment: `perf-summary.md`. Producer:
`scripts/acceptance/u4/perf-baseline.mjs`.

## System under test

`bin/grayling` built from this checkout (embedded `web/static` bundle), booted
with a fresh isolated store per run (`-data <mktemp>` + embedded gonacos on
ephemeral loopback ports, `GRAYLING_DEV_AUTOLOGIN=1`). Chromium headless
(Playwright), desktop viewport 1440×1000, locale zh-CN, theme light, seeded
dataset (workspace `u4acc` / env `prod`, flag `u4-bool` at 2 revisions,
running-state experiment, AI resource). No mocks anywhere in the path.

## Metrics

Per key page (overview, flags, flag detail, experiments, experiment detail
summary tab, AI):

- **FCP / LCP** — `PerformanceObserver` entries (`paint` /
  `largest-contentful-paint`), captured from an init script injected before
  page scripts run.
- **DCL / load** — `PerformanceNavigationTiming.domContentLoadedEventEnd` /
  `loadEventEnd`, relative to `startTime`.
- **TTI-proxy** — timestamp of the second `requestAnimationFrame` callback
  after the `load` event fires (load + 2×rAF). It is a *proxy*: grayling has
  no longtask-free "consistently interactive" definition, and the bundle is a
  single synchronous chunk, so load + two frames is the earliest moment the
  shell and first paint are guaranteed settled. It is deterministic and cheap
  to reproduce; treat cross-tool comparisons (Lighthouse TTI etc.) as not
  equivalent.

## Protocol

- **Cold**: fresh browser context per iteration → all caches empty. 5
  iterations per page, median reported.
- **Warm**: one primed context per page (bundle + API cache warm), navigating
  to `about:blank` between iterations. 5 iterations per page, median reported.
- The 1000-flag bulk seed runs **after** the medians so the timing pages are
  measured on the small baseline dataset.
- **Filter latency**: with 1000 flags in the list, set the filter input via
  the native value setter + `Event('input')`, then measure until the DOM is
  quiet (MutationObserver on `#main`, double-rAF quiet window). 5 iterations,
  median.
- **Longtasks**: `PerformanceObserver({type:'longtask'})` on the experiment
  evidence tab, 3 s dwell; count and total blocking duration reported.

## Budgets (hard assertions, run fails on breach)

| budget | limit | measured on |
| --- | --- | --- |
| main chunk `web/static/assets/index-*.js` | ≤ 460 KiB raw | file bytes |
| CSS `web/static/assets/index-*.css` | ≤ 32 KiB raw | file bytes |
| viz sources `web-pages/src/viz/**` | ≤ 15 KiB | summed file bytes |
| runtime dependencies | zero growth | `pnpm-lock.yaml` importers `web*` `dependencies:` vs `HEAD~` (only devDependency additions allowed) |

KiB = 1024 bytes. All numbers are raw (uncompressed) bytes; the transfer size
with gzip/brotli is smaller.
