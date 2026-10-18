# U4 acceptance harness (redesign R5)

U4 is the standing completion gate for the web redesign: axe audit (R5A.1),
performance baseline with hard budget assertions (R5A.2) and a keyboard-only
walkthrough (R5A.3), all running against a **real** `bin/grayling` (built from
this checkout) with a fresh isolated store — zero mocks. Evidence lands in
`docs/acceptance/web-2026-10-18-redesign/`.

## Layout

- `run-server.sh` — build (if `bin/grayling` is missing) + boot an isolated
  instance (fresh mktemp store, embedded gonacos on ephemeral loopback ports,
  `GRAYLING_DEV_AUTOLOGIN=1`), wait for `/api/v1/health`, print the base URL,
  tear down on exit.
- `lib/server.mjs` — the same lifecycle for the Node scripts
  (`attachOrStart` honours `U4_BASE_URL` to drive an already-running server).
- `lib/browser.mjs` — Playwright context factory (desktop 1440×1000 / mobile
  390×844, `grayling_lang`/`grayling_theme` presets, native-dialog probe,
  pageerror capture). `@playwright/test` and `@axe-core/playwright` resolve
  through `createRequire(web/package.json)` — no `NODE_PATH`, no script-local
  `package.json`.
- `lib/seed.mjs` — data seeding through the real REST API (workspace `u4acc`,
  env `prod`, flag `u4-bool` at two revisions, flag `u4-json` submitted,
  segment, metric, experiment, AI resource). Dev-autologin only signs GET/HEAD
  in, so the seeder harvests the session cookie from a GET and sends it (plus
  a same-origin `Origin` header) with mutations.
- `routes.json` — every console route (enumerated from
  `web/src/routes.tsx`), `${PLACEHOLDER}`s bound from the seed summary.
- `axe-audit.mjs` — R5A.1. Full pass over all routes (desktop/zh/light), plus
  the six key pages (experiment detail expanded to its five zone tabs) under
  zh/dark, en/light, en/dark and mobile 390px. Writes `axe-results.json` +
  `axe-summary.md`/`axe-summary.json`. Exit 1 on any open violation, native
  dialog call, or page error. Exceptions are **never** silently disabled:
  `axe-exceptions.json` is the machine-readable exception list (matched
  entries are printed at the end of the run) and must stay in sync with
  `docs/acceptance/web-2026-10-18-redesign/axe-exceptions.md`.
- `perf-baseline.mjs` — R5A.2. FCP/LCP/DCL/load/TTI-proxy medians (cold +
  warm, 5 iterations) for the six key pages; 1000-flag filter latency; report
  longtask capture; budget assertions (main chunk ≤ 460 KiB, CSS ≤ 32 KiB,
  viz sources ≤ 15 KiB, zero new runtime deps vs `HEAD~`). Writes
  `perf-results.json` + `perf-summary.md`. Methodology:
  `docs/acceptance/web-2026-10-18-redesign/perf-methodology.md`.
- `keyboard-walkthrough.mjs` — R5A.3. Skip-link, sidebar nav reachability +
  `aria-current`, ZoneNav manual activation, dialog focus trap/escape/restore,
  two-step confirm by keyboard, viz-SVG tab-order exclusion, focus-ring
  screenshots (zh/light + zh/dark). Writes `keyboard-walkthrough.{json,md}` +
  `keyboard-*.png`.
- `matrix.mjs` — R5B.1. Role × state × device × language matrix against a
  fresh isolated store. Sections: **R** roles (mints publisher/approver/
  analyst/viewer tokens via the real token API; capability maps checked
  against RBAC; a dual-admin draft→submit→approve→publish loop driven
  entirely through the UI with audit attribution verified server-side;
  analyst/viewer write rejections checked at API 403 + UI gating layers);
  **S** error states (401, 409 conflict panel, 429 quota toast, offline
  degradation — cached chunk → in-page banner, uncached chunk → shell keeps
  a reload card, recovery after reconnect); **D** data states (empty
  workspace, loading skeletons via latency injection, missing-flag error,
  zero-data running experiment, small-sample watermark, healthy CI charts,
  SRM-skewed allocation, 1000-flag list); **V** 390/768/1024/1440 widths
  with horizontal-overflow assertions on overview/flags/flag-detail/
  experiment-summary; **L** zh/en × light/dark on the six key pages; **A**
  200% zoom equivalent, reduced-motion, forced-colors, contrast-more;
  **T** a11y-tree YAML snapshots of the six key pages. Writes
  `matrix-results.json` + `matrix-summary.md` and `matrix-*.png`
  screenshots into the evidence dir; exit 1 on any failed check.
- `smoke.mjs` — harness self-check (server + seed + one axe run); not a gate.

## Running

```sh
# everything self-contained (builds bin/grayling when missing):
export TMPDIR=$HOME/tmp-go        # Go builds must not touch the root partition
node scripts/acceptance/u4/axe-audit.mjs
node scripts/acceptance/u4/perf-baseline.mjs
node scripts/acceptance/u4/keyboard-walkthrough.mjs
node scripts/acceptance/u4/matrix.mjs

# or boot once, run many:
scripts/acceptance/u4/run-server.sh          # prints http://127.0.0.1:28090
U4_BASE_URL=http://127.0.0.1:28090 node scripts/acceptance/u4/axe-audit.mjs
```

Knobs: `U4_PORT` (first port tried, default 28090), `U4_TMP` (scratch root),
`U4_REBUILD=1` (force `make build`), `U4_BASE_URL` (attach, skip boot/teardown).

Note on the browser: `@playwright/test` is pinned to the release whose
Chromium build is present in `~/.cache/ms-playwright` (the public CDN is
unreachable from this box; binaries come from the npmmirror CfT mirror).
