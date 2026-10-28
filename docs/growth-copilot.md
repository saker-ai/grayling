# Growth Copilot: read-only evidence foundation

Part of [#20](https://github.com/saker-ai/grayling/issues/20). `internal/growth`
is an optional, unwired Go library. No process, public route, SDK behavior,
persistence, browser UI, model call or production write is added. Disabling or
never importing it leaves the existing platform unchanged.

## Reuse and missing capabilities

| Capability | Existing surface | This slice / remaining boundary |
| --- | --- | --- |
| Exact metric definition | `GET /api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}` | Reuse workspace-scoped viewer access; check workspace, key, version and hash. Metrics are workspace-level, with no environment field; report scope supplies the environment boundary. |
| Aggregate experiment evidence | `GET /api/v1/admin/experiments/{id}/report/export?format=json` | Reuse scoped report snapshot, generation time, epoch, evidence version, frozen primary/guardrail references, analysis window, watermark, recomputability, health, primary calculation errors, issues and decision. |
| Diagnostics | Already embedded in report export | Reuse embedded experiment/epoch, generation time, window and issues. No separate diagnostics request or independently timed snapshot. |
| Statistical decisions | Existing report builder and statistics packages | Preserve server verdict and reasons with analysis method; do not calculate significance, sample sizes, uplift, or winning probabilities. Parallel sequential/Bayesian/Bandit views are deliberately omitted, never merged into the fixed-sample decision. |
| RBAC and isolation | Existing admin API / `report.read` viewer permission | Fixed reader workspace/environment, existing bearer authentication, explicit response validation; no privileged endpoint. Workspace-wide viewer tokens are not environment-restricted by this library at the server. The reader rejects other environments before exposing evidence. |
| Flags, preregistration, exposure, conversion, approvals, rollout | Existing platform, SDK, collect and admin APIs | Unchanged and unwired. Reader has no write operations and does not authorize rollout. |
| Saker goal-to-metric mapping and activation baseline | No real Saker mapping or pilot evidence supplied | Caller must supply actual metric references and experiment IDs. No inferred mapping, product trends, BI connector or causal claim. |
| Hypotheses, model generation, proposal review, experiment submission | Proposal only for this slice | Not implemented; no growth API or draft generation assumed. |
| Durable orchestration, recovery, learning journal, business acceptance | Requires later design and evidence | No task storage/retries, restart recovery, model budget, review UI or real customer acceptance claim. |

## Library contract

Construct `growth.NewReader(Config{BaseURL, WorkspaceID, EnvironmentID,
BearerToken})`, defer `Close()`, and call `Read(ctx, Query{Metrics, Experiments})`.
Use actual workspace/environment **IDs**, not slugs. The base URL is a trusted
origin with no path, credentials, query or fragment; use HTTPS for remote use.
Do not derive configuration or credentials from model output. A reader owns its
HTTP transport and rejects every redirect, including same-origin redirects.

Every metric reference requires key, positive version and `sha256:` definition
hash. Every experiment reference requires ID and positive epoch. An optional
`EvidenceVersion` pins a previously observed report; first observation validates
and retains the returned `sha256:` evidence version. This checks identity and
server version equality, not a client-side recomputation of the full report hash.
All primary and guardrail references returned by a report must exactly match
explicit metric references in the query. Legacy reports without frozen metric
references cannot be READY. Duplicate/invalid references are rejected before
network access. Each query permits at most 100 metrics and 100 experiments.

Only the two GET paths above can be constructed. Metric and report 404s are
missing evidence; a scoped export with a null/absent report is also missing.
401/403, other non-200 responses, malformed JSON, duplicate JSON member names,
scope mismatches, metric mismatches and epoch/evidence-version mismatches fail
the read and return no partial envelope. A 404 may conceal an inaccessible
resource; it never authorizes another request or establishes resource existence.
Unknown response fields are discarded through explicit DTO allowlists.

The envelope contains source paths (including the JSON format query), fixed
workspace/environment IDs, validated metric identities and aggregate report
fields. It excludes metric descriptions, owners, raw identities/events, arbitrary
JSON, and parallel statistical views. It preserves server-generated window and
report times rather than replacing them with the client's clock. This is a
reduced evidence projection, not an archive from which the full evidence hash
can be reconstructed. Server issue and reason strings remain untrusted data:
never treat them as instructions or executable authorization.

Adapter readiness is separate from `report.decision.verdict`:

- `NEEDS_DATA`: any requested metric/report is missing, or either input list is
  empty. Missing source paths are retained; no statistics are filled in. Missing aggregate values are omitted rather than represented as zeros.
- `HOLD`: a present report is unhealthy, incomplete, non-recomputable, missing
  required metadata/diagnostics/references/aggregates, has inconsistent diagnostic
  windows or issues, has a watermark before window end, or the server verdict is
  hold/unknown. The original server decision remains visible even if readiness
  is HOLD.
- `READY`: all requested evidence is present and passes these checks. This means
  usable evidence, **not** approval, a winner, or permission to publish. Server
  `continue` and `rollback` remain those exact verdicts.

Missing evidence takes precedence over HOLD at the envelope level; individual
present reports retain their HOLD status. Local incremental stores cannot
establish durable recomputability and therefore produce HOLD. The adapter does
not turn local fixtures into real pilot evidence.

Default bounds are 10 seconds and 2 MiB per response, configurable up to 30
seconds and 8 MiB. The body bound applies after HTTP decompression. A caller
context cancels requests and can impose a deadline on the whole sequential read;
otherwise each request has its own timeout. There are no retries. Errors are
fixed sentinel messages (or context cancellation/deadline errors), with no
credentials, response body, decoder detail, or URL. The reader does not log.
Callers must likewise avoid logging configuration or treating server text as
trusted. It retains only aggregates, without adding storage or telemetry.

## Verification and limitations

`internal/growth/evidence_test.go` uses local HTTP fixtures for healthy and
unhealthy decisions, missing evidence, scope/version/hash mismatches, malformed
and oversized bodies, authorization, allowlisting, secret-safe errors,
redirects, deadlines, cancellation, invalid input and zero write requests.
`internal/growth/integration_test.go` calls the real API handler with a local
store, a published metric and workspace viewer credentials, checking pinned
snapshots, environment/workspace rejection, unauthorized access, missing reports
and conservative HOLD on local evidence. Neither suite measures real customer
outcomes or validates a Saker activation experiment.

The mandatory repository build, short tests, vet and whitespace checks still
apply, along with the Go quality workflow's core race and binary boot/E2E/smoke
gates. Existing CI is unchanged. No additional database/browser acceptance is
required for this unwired slice. Full issue #20 acceptance (including real Saker
metric mapping, business variants/instrumentation, pilot evidence, proposal
review, durable orchestration and business outcomes) remains outstanding.
