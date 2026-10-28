# Growth Copilot: evidence and proposal contracts

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
| Proposal contract | `DecodeProposal` / `ReviewProposal` in `internal/growth` | Strict, bounded planning JSON and evidence-based completeness for human review; no generation, review UI, approval or submission. |
| Hypotheses, model generation, proposal review UI, experiment submission | Requires later work | No growth API or model integration; no experiment draft submission. |
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

## Proposal contract

`DecodeProposal(io.Reader)` accepts at most **64 KiB** of UTF-8 JSON. It rejects
unknown members at every depth, duplicate names (including escaped aliases),
malformed or trailing JSON, null/array roots, and caller-supplied state,
approval/submission, readiness or embedded evidence-envelope members. It returns
only `ErrProposal`, without echoing model text. Semantic omissions may decode
successfully so that a draft can be edited; call `ReviewProposal` next.

`ReviewProposal(proposal, envelope)` takes the envelope **separately**, from
trusted caller code using `Reader.Read`. Never decode a model-provided envelope
and pass it as trusted evidence. These Go values are not signed capabilities;
callers own this boundary. Neither natural-language instructions nor server
reason strings authorize anything. Attestations must eventually be collected
from the implementing team through a trusted human flow; model-generated claims
alone are not verification. There is no model or attestation collection flow here.

Version `growth.proposal.v1` requires proposal ID/version, goal ID, workspace and
environment IDs, hypothesis, rationale, primary and guardrail metric references,
evidence references, flag key, variants with percentage allocation, audience,
randomization unit, `onRender` and its real display trigger, risk (`low`, `medium`,
`high`), implementation checklist, analysis and sample plans with provenance, and
rollback conditions. Bounds are 2–20 variants, 1–20 guardrails, 1–20 evidence
references, 1–20 rollback conditions and at most 40 attestations. Required prose
is nonblank and at most 4096 bytes per field. Total encoded proposal size also
applies to typed callers. Duplicate metric identities, evidence experiments,
variants and attestations block readiness. Percentages must be positive and sum
to 100 (rounding tolerance 1e-9).

Every proposal metric must match the envelope's workspace, key, version, hash
and exact canonical metric-version source path. Every evidence reference must
match its canonical report export path, experiment ID, epoch, evidence version,
window version/start/end/watermark/backend and completeness/recomputability.
The report's frozen primary and guardrail references must match the proposal.
Missing envelope/references, scope or version mismatches, missing paths, any
non-READY envelope or referenced evidence, or unusable windows block readiness.
Reader remains the authority for server health interpretation; this is not a
second experiment/statistics validator. There is no clock-based freshness claim:
watermarks are compared to the pinned snapshot, not an invented staleness policy.

Results contain ordered `{field, code}` blockers. States have this precedence:

| State | Meaning / example blocker |
| --- | --- |
| `draft` | Contract incomplete/invalid: `schemaVersion: unsupported_version`, `exposureMode: on_render_required`, `variants: allocation_total`. |
| `needs_data` | References absent/mismatched/unhealthy: `primaryMetric: metric_evidence_mismatch`, `evidenceRefs[0]: evidence_reference_mismatch`, `evidenceRefs: evidence_not_ready`. |
| `needs_implementation` | Missing explicit complete attestation with owner/source: `implementation.render_exposure: attestation_required`. Each variant needs its own attestation; stable identity, real render exposure and conversion unit must each attest the proposal's unit. |
| `ready_for_review` | No blockers; contract completeness for human review **only**. No approval, binding clearance, permission to publish or statistical/instrumentation validation. |

Sample-plan minimum sample per arm, runtime hours and MDE are **explicitly
supplied positive planning inputs** with source, supplier and timestamp. Analysis
method and attribution hours also require provenance. The library neither fills
in numbers nor computes/verifies power, adequacy, significance, causal effect or
winning probability. Units for MDE depend on the supplied metric/analysis plan;
this contract makes no statistical interpretation. Future preparation must use
the existing Grayling validators, RBAC, approval and binding checks for real
flags, revisions, metrics, conflicts and experiment legality.

Caller usage (error handling omitted here for brevity):

```go
envelope, err := reader.Read(ctx, pinnedQuery) // trusted configuration and query
if err != nil { return err }
proposal, err := growth.DecodeProposal(modelOrHumanJSON)
if err != nil { return err }
review := growth.ReviewProposal(*proposal, envelope)
// Display review.State and review.Blockers to a human. No write follows here.
```

This illustrative JSON uses synthetic identifiers, hashes and planning claims;
it is **not real Saker evidence**. Replace references with exact Reader values
and obtain implementation claims from the implementing team. Against absent or
local HOLD evidence it cannot become `ready_for_review`.

```json
{
  "schemaVersion": "growth.proposal.v1",
  "id": "proposal1",
  "version": 1,
  "goalId": "activation",
  "workspaceId": "ws1",
  "environmentId": "env1",
  "hypothesis": "A shorter introduction increases first task completion",
  "rationale": "Test introduction friction against the pinned evidence",
  "primaryMetric": {
    "ref": {"key": "activation", "version": 1, "definitionHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "sourcePath": "/api/v1/admin/workspaces/ws1/metrics/activation/versions/1"
  },
  "guardrails": [{
    "ref": {"key": "retention", "version": 1, "definitionHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "sourcePath": "/api/v1/admin/workspaces/ws1/metrics/retention/versions/1"
  }],
  "evidenceRefs": [{
    "sourcePath": "/api/v1/admin/experiments/exp1/report/export?format=json",
    "experimentId": "exp1", "epoch": 1,
    "evidenceVersion": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "window": {
      "version": "utc-exposure-v1",
      "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T01:00:00Z",
      "watermark": "2026-01-01T02:00:00Z",
      "complete": true, "recomputable": true, "backend": "postgresql-raw"
    }
  }],
  "flagKey": "onboarding",
  "variants": [{"key": "control", "allocationPercent": 50}, {"key": "treatment", "allocationPercent": 50}],
  "audience": "new users", "unit": "user_id",
  "exposureMode": "onRender", "exposureTrigger": "after the introduction is displayed",
  "risk": "low",
  "implementation": [
    {"kind": "variant", "variant": "control", "complete": true, "attestedBy": "developer", "source": "implementation review"},
    {"kind": "variant", "variant": "treatment", "complete": true, "attestedBy": "developer", "source": "implementation review"},
    {"kind": "stable_identity", "unit": "user_id", "complete": true, "attestedBy": "developer", "source": "identity review"},
    {"kind": "render_exposure", "unit": "user_id", "complete": true, "attestedBy": "developer", "source": "display instrumentation review"},
    {"kind": "conversion_unit", "unit": "user_id", "complete": true, "attestedBy": "developer", "source": "conversion instrumentation review"}
  ],
  "analysisPlan": {
    "method": "two-proportion-z", "attributionWindowHours": 24,
    "provenance": {"source": "planning worksheet v1", "suppliedBy": "growth owner", "suppliedAt": "2026-01-01T03:00:00Z"}
  },
  "samplePlan": {
    "minSamplePerArm": 100, "minRuntimeHours": 336, "mde": 0.02,
    "provenance": {"source": "planning worksheet v1", "suppliedBy": "growth owner", "suppliedAt": "2026-01-01T03:00:00Z"}
  },
  "rollbackConditions": ["guardrail breach: human review and existing rollback flow"]
}
```

`proposal_test.go` covers more than 20 deterministic contract cases, including
completeness, reference mismatches, HOLD/missing evidence, required attestations,
strict JSON and injected approval instructions. The real-handler integration
uses complete planning claims and confirms that local HOLD evidence returns
`needs_data`, including when rationale text demands approval and publication.
No storage, HTTP writes, experiment draft submission, second validator, model
integration, production deployment or additional DB/browser surface is added.
