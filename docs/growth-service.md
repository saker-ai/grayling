# Optional Growth Copilot service

`grayling-growth-agent serve --config FILE` runs a separate, loopback-only HTTP
process and workbench. It reuses `internal/growth` for evidence, pinned goals,
proposal completeness, review, draft compilation, health gates, rollout observation
and learning. The existing one-shot commands retain their read-only/offline
behavior. Neither this process nor a model is started by `grayling`, SDKs,
collection, reporting or rollout.

## Start and authorization

Build with `make build`. Create an owned **0700** directory and put the following
configuration there. Use absolute database paths. Supply independently generated
service tokens of at least 32 characters through the named environment variables;
never reuse the platform reader token as a service credential.

```json
{
  "database": "/absolute/private/growth/state.sqlite",
  "address": "127.0.0.1:27003",
  "baseUrl": "http://127.0.0.1:27002",
  "workspace": "ws_id",
  "environment": "env_id",
  "readTokenEnv": "GRAYLING_GROWTH_READER",
  "principals": [
    {"name": "analyst", "tokenEnv": "GRAYLING_GROWTH_EDITOR", "role": "editor"},
    {"name": "reviewer", "tokenEnv": "GRAYLING_GROWTH_REVIEWER", "role": "reviewer"},
    {"name": "observer", "tokenEnv": "GRAYLING_GROWTH_VIEWER", "role": "viewer"}
  ]
}
```

The configuration contains environment variable names, not secrets. Issue a
least-privilege Grayling **viewer** credential for the reader; its configured
workspace/environment is immutable for a database. The service never uses that
credential for a platform write. Service roles are separately administered:
viewers read; editors create goals, edit, prepare, retry, abort, observe and record
learning; reviewers additionally accept/reject/request changes and perform
explicit platform actions. These are local authenticated principals, not model
claims or authentication of an artifact exported by the old CLI. Revoke or
rotate a principal by updating its environment token and restarting the service.
No production configuration, migration or rollout is enabled by this delivery.

Open the printed loopback address. Paste the service token into Access. It stays
in page memory, with no localStorage, sessionStorage or cookie. Each platform
write also requires a human's **ephemeral Grayling token** in the separate input;
the page clears it after the action. Existing platform RBAC independently checks
that token. Reverse proxies must provide TLS and preserve same-origin policy;
remote exposure is an operator deployment decision, not an included acceptance.

## Workflow

1. Create `{goal, manifest?}` with the existing `growth.goal.v1` and optional
   `MonitorManifest` schemas. A manifest refers to an **existing authorized
   experiment report**, not invented BI facts. Set a distinct `Idempotency-Key`.
   Creation returns HTTP 202 and a durable task ID immediately; poll the task.
   The UI polls during QUEUED/ANALYZING. Missing baselines produce NEEDS_DATA.
2. Read exact metric pins, baseline window, watermark, health and evidenceVersion.
   Optional hypotheses are unreviewed suggestions. Copy/edit a complete proposal
   in the workbench, advancing its version by exactly one. Its ID, goal pin,
   scope and metric roles must match the goal. All proposal versions are retained.
   Supply genuine implementation sources for variants, stable identity,
   real-render exposure and conversion unit. JSON checkboxes alone do not prove
   instrumentation or consent.
3. A reviewer accepts, rejects or requests changes with a rationale. Reviewer and
   timestamp are assigned by the authenticated service, not accepted from JSON.
   Acceptance requires a fresh evidence read and the existing completeness gates.
   Editing invalidates review and preparation. Failures retain readiness blockers
   or a fixed remediation message; stale revisions return 409.
4. Prepare with explicit `PreparationInputs` from the existing compiler contract:
   fixed split, owner, alpha, power, observation hours and guardrail thresholds.
   Plans and MDE/sample/runtime inputs remain human-supplied with provenance;
   neither the model nor this service estimates sample sizes or invents a
   statistical threshold. Preparation pins the current published flag revision.
5. Click **Submit experiment draft** with the human Grayling credential. The
   service checks fresh evidence, the prepared flag revision and competing live
   registrations before saving a submission intent. Flag drift invalidates local
   preparation and review. A positive `flagRevision` in the existing create API
   now acts as an optional precondition; zero/omission retains automatic selection.
   The store atomically checks the admin-observed revision at registration.
6. Platform review and bind are **separate buttons**. They call existing Admin
   endpoints under the human token; bind also observes the existing bind-check.
   Binding rechecks its own constraints in core and creates a scheduled flag
   draft. Publication/production approval stays in the existing console. No
   publish, transition-to-running, rollout-advance, marketing, shell or business
   code tools exist here. Aborting a task stops future work and polling; it cannot
   undo a platform action that already completed. Use authorized existing
   emergency/rollback procedures for an actual running experiment.
7. Confirmed submissions receive read-only report/rollout observations every
   30 seconds and on request. HOLD, NEEDS_DATA and server verdict remain distinct;
   blocker remediation comes from existing Monitor. No AI statistical or winning
   interpretation overrides the report. Record learning explicitly with follow-ups.
   Search retains immutable observations, including failures, and groups identical
   normalized hypothesis text only when exact metric roles/versions agree. Grouping
   preserves every record and declares transferability limitations. Cross-environment
   importing is not automatic; the old explicit offline export/search remains usable.

The primary mapping may be first-task activation, paid-upgrade conversion or
another authorized metric. The second-path test is synthetic metric reuse,
**not a second real product pilot**. The real Saker chain, D1 guardrail definitions,
identity/exposure implementation attestations and pilot approvals remain human inputs.

## Durability, limits and recovery

SQLite WAL with FULL synchronous commits stores tasks, immutable proposal
versions, learning snapshots, submission intent and a chained audit in transactions.
The audit includes principal, action/outcome, time, goal/proposal fingerprints,
version and checkpoint hash. One OS file lock excludes another service process.
Database/lock files are 0600; final-component symlinks and nonregular files are
rejected; the immediate data directory must be owned and 0700. Ancestor directories
must also be operator-trusted. This is single-host durability, not replicated HA.

Queued/interrupted analyses resume on restart, with at most three actual attempts
(the fourth checkpoint records budget exhaustion). Model budget is reserved
**before** a call. If the process dies after reservation, it does not spend that
reservation twice: missing output with an exhausted budget yields HOLD. A reviewer
can use the evidence and supply a proposal manually or authorize a new task.
Separate evidence/goal-monitor reads must agree on evidenceVersion, registration
and verdict; a changed snapshot produces HOLD before planning.
Invalid model JSON/schema, fabricated numeric claims and missing/incorrect source
pins fail closed. Task revision CAS prevents stale model/monitor results replacing
an edit or abort.

Before creating an experiment, the service durably reserves an explicit epoch and
request hash. Retries/restarts reconcile the same flag/epoch against the complete
create DTO and pinned flag revision. The core's unique flag/epoch constraint blocks
a second creation. A lost response is reconciled with the resulting experiment ID;
an incompatible collision is a conflict requiring operator review/new task, never
an automatic alternate epoch. Once confirmed, a missing experiment is not recreated.
Review/bind retries observe existing core state; neither republishes. Core and
service audit chains are separate, and hashes detect corruption rather than
protecting against a hostile database owner rewriting an entire chain.

Limits: 64 tasks per database, 128 KiB HTTP bodies, 2 MiB platform responses,
30-second operation deadline, 30-request burst then one request/second per
principal, 64 learning snapshots/4 MiB total journal. The journal fails closed
when full; export and archive through an operator-reviewed retention process,
with no automatic deletion of evidence. No raw events, raw model prompts or raw
model responses are persisted. Known credentials are rejected in inputs; server
free-text health/error strings are replaced with fixed markers before persistence.
Human-entered planning text may still contain sensitive business information:
access and backups must follow the operator's retention policy.

Stop the service before taking an offline backup:

```sh
bin/grayling-growth-agent backup --database /absolute/private/growth/state.sqlite \
  --output /absolute/private/backups/growth-snapshot.sqlite
```

The destination directory must be 0700 and the destination must be new. Backup
verifies audit/proposal/journal integrity then uses `VACUUM INTO` for a complete,
WAL-independent 0600 snapshot. To restore, stop the service, retain the failed
set for diagnosis, copy the snapshot to a **new private directory**, update the
configuration's database path, and start. Do not mix a restored main file with an
old `-wal`/`-shm` set. Startup verifies revisions, audit links and historical
proposal/journal pins, refusing a corrupt or incomplete restore.

## Optional local planner

AI defaults off. To test a local adapter, set `localPlannerUrl` to an explicit
`http://127.0.0.1:PORT` (or IPv6 loopback) origin. The service POSTs `/plan` once per
task. The adapter receives only a metric pin, evidenceVersion and exact allowlisted
per-arm aggregate values indexed by arm (at least 20 exposed units in each arm,
a conservative disclosure floor separate from statistical adequacy); no goal prose, report text, descriptions,
identities, credentials or tools. It must return a strict JSON array of two or
three `{hypothesis, mechanism, falsification, risk, effort, evidenceVersion}` objects.
Risk is low/medium/high; effort is small/medium/large. Text is bounded qualitative
planning content; digits, percent and currency signs, unknown members, duplicate
hypotheses and incorrect pins are rejected. This structural filter does not prove
natural-language truth or recommendation quality; the text is presented only as
unreviewed hypotheses, never metric facts. Numerical facts come from the separately
rendered server snapshot. The adapter has a 20-second timeout, 16 KiB output cap,
no redirects, credentials, retries, paid/cloud fallback or arbitrary tool calls.
The Go `Planner` interface supports trusted replacement adapters with the same
boundary. No paid or real model was used for local acceptance.

## API and verification

The private service API is [documented separately](growth-service.openapi.yaml).
It adds no `/api/v1/growth` route to Grayling's stable public API. Authentication
is bearer-only, JSON bodies are strict/bounded, cross-origin requests are denied,
and the UI uses a restrictive CSP and text rendering.

Run `GOWORK=off go test -race -count=1 -v ./internal/growthservice` after
`make frontend`. This includes the fixed 28-case synthetic safety set, actual-platform
RBAC/create/review/bind/report tests, response-loss restore, SQLite corruption
checks, cancellation/budget limits, a second synthetic metric, real binary SIGKILL
recovery and Playwright Chromium UI acceptance. The browser script resolves the
repository-pinned Playwright dependency; install its matching Chromium locally if
missing. Full repository build, short/core race, boot/E2E/smoke, SDK, frontend/static,
license, compatibility, generated-contract and disposable PostgreSQL gates remain
required. Local software passes do not replace blind recommendation review,
licensed data, approved identities/consent, measured operator time, production
readiness or a healthy real randomized Saker experiment.

## Existing API reuse and gaps

| Need | Existing path/component reused | Gap handled by this delivery |
| --- | --- | --- |
| Frozen metric definitions | `GET /api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}` → `growth.Reader.Read` | No new metric/statistics authority |
| Report, diagnostics and watermarks | `GET /api/v1/admin/experiments/{id}/report/export?format=json` → Reader/Monitor/CheckGoal | Durable task linkage and consistent snapshot pins |
| Experiment and rollout history | `GET /api/v1/admin/experiments/{id}` → `growth.Reader.Rollout` | Read-only periodic observation, no second rollout engine |
| Binding prerequisites | `GET /api/v1/admin/experiments/{id}/bind-check` → `growth.Reader.BindCheck` | Explicit human action still rechecked by core |
| Create, review, bind | Existing workspace/environment experiment POST and experiment review/bind POST | Durable intent/reconciliation; optional atomic flag-revision precondition |
| Goal, proposal review, tasks, learning | Existing strict growth artifacts and compilers | Separate private API/UI, authenticated actors, SQLite recovery and immutable journal |
| Product/BI aggregate baseline | No authorized provider is assumed | NEEDS_DATA until an approved source exists; no collect warehouse or invented trend |
