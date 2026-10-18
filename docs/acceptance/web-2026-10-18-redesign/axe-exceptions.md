# axe exceptions (R5A.1)

Machine-readable source: `scripts/acceptance/u4/axe-exceptions.json`
(currently an empty list — **there are no exceptions**).

Audit of 2026-10-18 (`docs/acceptance/web-2026-10-18-redesign/axe-results.json`,
64 runs: 24 routes × desktop/zh/light + 10 key-page routes × zh/dark, en/light,
en/dark, mobile-390px; rule tags wcag2a/2aa/21a/21aa/22aa) ended with **zero
open violations**, zero native-dialog calls and zero page errors, so no rule
needed an exception.

Policy for future runs (the script enforces it):

- A violation may only be silenced by adding an entry to
  `axe-exceptions.json` **and** documenting it here (rule id, route, combo,
  node count, reason, follow-up issue).
- Matched exceptions are printed loudly at the end of every audit run and
  listed in `axe-summary.md` under "Excepted".
- Anything else fails the run; there is no per-rule `disable` in the scripts.

## Violations found during R5A and fixed in code (not excepted)

| rule | impact | where | fix |
| --- | --- | --- | --- |
| select-name | critical | `#exp-to` (experiment transition select), `#ai-r-type` (AI resource type select) | added `aria-label` bound to new i18n keys `exp.transitionTo` / `ai.resolveType` (`web-pages/src/pages/ExperimentDetailPage.tsx`, `web-pages/src/pages/AiPage.tsx`, `web/src/i18n/{en,zh}.ts`) |
| target-size | serious | `#flag-mode-structured` / `#flag-mode-json` partially obscured on mobile 390px | `.sticky-actions` was `position:sticky; bottom:0` inside a card taller than the viewport, so it clamped to the card's top edge at scroll 0 and covered the mode buttons; now `position:fixed` bottom bar on mobile + `scroll-padding-bottom` (`web/src/styles/global.css`) |

Also fixed from walkthrough prep (not axe rules): the skip-link used
`href="#main"`, which replaced the hash route and unmounted the app on
activation — it now focuses `#main` programmatically; sidebar nav buttons
carry `aria-current="page"` (`web/src/pages/ShellLayout.tsx`).
