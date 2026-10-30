# Growth Copilot: evidence and proposal contracts

Part of [#20](https://github.com/saker-ai/grayling/issues/20). `internal/growth`
is an optional Go library exposed by `cmd/grayling-growth-agent`, an independent
read-only command. No public route, SDK behavior, persistence, browser UI, model
call or production write is added. Never running the command leaves the
existing platform unchanged.

## Rejected collection attempts

The evidence allowlist now projects `report.diagnostics.rejectedByReason` using
only the fixed rejection codes from `internal/platform/collection_diagnostics.go`:
`invalid_event`, `unknown_experiment`, `stale_epoch`, `assignment_mismatch`,
`missing_exposure`, `missing_unit`, `invalid_exposure_token`,
`expired_exposure_token`, `late_event` and `future_event`.
Counts retain exact nullable int64 values. Unknown keys never leave the reader;
only `unknownRejectionReasons: true` signals their presence. The projection is
bounded to these ten codes, including when serialized by library callers.

Omitted, null and empty maps remain compatible because the server omits absent
counters. Every supplied known count must be explicitly zero. Nonzero (including
negative), null counts or unknown keys require HOLD, independently of summary
health and server verdict. Reader and Monitor share this validation; monitoring
uses `health_review_required` / `review_diagnostics` and preserves the server
verdict. Malformed values, overflow and duplicate JSON members reject the entire
response with no partial output, including malformed counts under unknown keys.
`duplicateEvents` remains separate: idempotent duplicate attempts alone do not
imply unhealthy collection, and `duplicate_event` is not a rejection code.

These durable counters cover the **experiment epoch**, not the report window.
HOLD requests investigation of rejected collection attempts; it does not establish
window-specific statistical bias or recalculate a statistical conclusion. Review
blocks approval, both preparation paths require fresh usable evidence, and
monitoring and learning round trips retain the investigation blocker. Local
library, actual-binary HTTP and real API-handler regressions check this behavior,
redaction and zero non-GET requests. These fixtures establish software behavior
only; model orchestration and real Saker pilot acceptance remain outside this
slice. All existing mandatory gates and their timeouts below remain unchanged.

Local validation on 2026-10-30 passed build, the complete short suite (rerun after
correcting real-handler fixture ordering), build/vet, core race, boot/E2E/smoke,
SDK, frontend/static, license and whitespace checks. Disposable PostgreSQL 16
platform/report/API race, full Agent integration race (227.087 seconds within
the unchanged five-minute timeout), and PostgreSQL E2E (3.667 seconds) passed.
The exit trap removed the container. These are local software checks only.

## Reuse and missing capabilities

| Capability | Existing surface | This slice / remaining boundary |
| --- | --- | --- |
| Exact metric definition | `GET /api/v1/admin/workspaces/{ws}/metrics/{key}/versions/{version}` | Reuse workspace-scoped viewer access; check workspace, key, version and hash. Metrics are workspace-level, with no environment field; report scope supplies the environment boundary. |
| Aggregate experiment evidence | `GET /api/v1/admin/experiments/{id}/report/export?format=json` | Reuse scoped report snapshot, generation time, epoch, evidence version, frozen primary/guardrail references, analysis window, watermark, recomputability, health, primary calculation errors, issues and decision. |
| Diagnostics | Already embedded in report export | Reuse embedded experiment/epoch, matching allowlisted exposureMode, generation time, window, issues and exact nullable crossVariantUnits/assignmentMismatch and supplied perVariant unexposed counts. No separate diagnostics request or independently timed snapshot. |
| Rollout observation | `GET /api/v1/admin/experiments/{id}` with embedded `releasePlan` | Pin workspace/environment, experiment, epoch and registration version; project bounded plan state/history without sensitive prose. Missing experiment and absent plan are separate observations; neither grants publication authority or establishes a business win. |
| Binding readiness | `GET /api/v1/admin/experiments/{id}/bind-check` between experiment GETs | Reuse the rollout identity manifest; validate nine prerequisite checks, revision consistency and unchanged surrounding snapshots. Non-atomic observation; never binding or publication authority. |
| Statistical decisions | Existing report builder and statistics packages | Preserve server verdict and reasons with analysis method; do not calculate significance, sample sizes, uplift, or winning probabilities. Parallel sequential/Bayesian/Bandit views are deliberately omitted, never merged into the fixed-sample decision. |
| RBAC and isolation | Existing admin API / `report.read` viewer permission | Fixed reader workspace/environment, existing bearer authentication, explicit response validation; no privileged endpoint. Workspace-wide viewer tokens are not environment-restricted by this library at the server. The reader rejects other environments before exposing evidence. |
| Flags, preregistration, exposure, conversion, approvals, rollout | Existing platform, SDK, collect and admin APIs | Unchanged and unwired. Reader has no write operations and does not authorize rollout. |
| Saker goal-to-metric mapping and activation baseline | No real Saker mapping or pilot evidence supplied | Caller must supply actual metric references and experiment IDs. No inferred mapping, product trends, BI connector or causal claim. |
| Proposal contract | `DecodeProposal` / `ReviewProposal` in `internal/growth` | Strict, bounded planning JSON and evidence-based completeness for human review; no generation, review UI or submission. Pure caller-attested review records are available separately. |
| Hypotheses, model generation, proposal review UI, experiment submission | Requires later work | No growth API or model integration; no experiment draft submission. |
| Durable orchestration, recovery, learning journal, business acceptance | Requires later design and evidence | No task storage/retries, restart recovery, model budget, review UI or real customer acceptance claim. |

## Read-only review command

From the repository root, install Go 1.27+ (the exact minimum is pinned in
`go.mod`), make, Node.js 26+ and pnpm 10. `make build` builds the frontend and
`bin/grayling-growth-agent`; `make build-go` builds the binaries using the existing
embedded assets. No database or browser setup is added by this command.

Obtain a bearer credential through your existing admin provisioning flow with
only the existing scoped viewer/report-read permissions needed for these GETs.
Supply it through the environment, never a flag or proposal member. For example,
with `GRAYLING_GROWTH_TOKEN` already populated by your secret manager:

```sh
./bin/grayling-growth-agent review \
  --proposal proposal.json \
  --base-url https://grayling.example.com \
  --workspace ws1 --environment env1 \
  --token-env GRAYLING_GROWTH_TOKEN --timeout 10s
```

Use actual IDs. The proposal JSON example below is synthetic; substitute real,
exact references and pinned report evidence/window values from trusted evidence.
The command accepts one regular proposal file of at most 64 KiB; stdin, FIFOs,
devices, unknown JSON members, embedded envelopes and approval/state fields are
rejected. All flags follow `review`; `--help` documents usage. There is no implicit workspace/environment selection. `prepare` additionally
requires a separate operator settings file; `review` does not accept `--settings`. `--token-env` defaults to
`GRAYLING_GROWTH_TOKEN`; it names a variable, never carries the token itself.

Treat the base URL, workspace/environment flags and credential selection as
trusted operator configuration, outside model/proposal control. The origin must
have no credentials, query, fragment or path other than `/`. Use HTTPS remotely;
HTTP is supported for local fixtures. The reader uses the host's proxy settings
and rejects redirects. Existing RBAC remains the authorization boundary: CLI
scope checks do not narrow a broader token at the server. Metrics are workspace
scoped; report environment is checked on the response before evidence is used.

Before network access the command rejects proposal/configuration scope mismatch,
noncanonical source paths, invalid/duplicate references and missing report
version pins. URLs are built exclusively by `Reader.Read` from validated IDs,
metric versions and hashes; proposal source paths are equality assertions, never
request destinations. No “latest” fallback or report pin update is performed.
Remote response scope/version mismatches fail the read without partial output.
Window differences remain actionable review blockers. Structurally incomplete
planning fields and missing implementation claims also remain review blockers.

The total sequential evidence read has a default 10-second deadline, adjustable
only to a positive duration at most 30 seconds. Each response is bounded to the
reader default 2 MiB after decompression; no retries occur. SIGINT/SIGTERM cancel
active requests. Proposal files must be regular, size-bounded files; the read
deadline covers HTTP execution. The command closes idle connections on exit.

Successful evidence reads print one JSON object to stdout, for example:

```json
{"evidenceStatus":"READY","review":{"state":"ready_for_review","blockers":[]}}
```

HOLD/NEEDS_DATA and field/code blockers come from `Reader.Read` and
`ReviewProposal`. CLI missing-variant blocker fields use
`implementation.variants[index]` rather than echoing the variant key. Output
contains no proposal prose, server reason text, raw evidence envelope, URL,
credential, approval or submission state. Errors use fixed stderr messages and
leave stdout empty (except an output-stream failure may leave partial JSON).

| Exit | Meaning |
| --- | --- |
| 0 | Evidence read completed and proposal is `ready_for_review`; human review still required. Also used for help. |
| 3 | Read completed with readiness blockers: `draft`, `needs_data` or `needs_implementation`, including evidence HOLD/NEEDS_DATA. |
| 2 | Invalid command/flags/configuration, missing credential, unreadable/nonregular/oversized/malformed proposal, proposal scope mismatch or invalid references; no evidence request occurs. |
| 1 | Execution failure: denied access, transport/deadline, invalid response, response scope/version mismatch or output failure. No approval or partial evidence is returned. |
| 130 | Signal cancellation (SIGINT or SIGTERM). |

This command only reviews supplied planning claims. It neither authenticates
implementation attestations nor generates proposals, calls models, approves,
submits, persists, writes to the platform or instruments Saker. A READY local
fixture proves command behavior only; it does not establish statistical adequacy,
real pilot acceptance or business impact. Existing experiment preparation,
approval, binding and rollout gates remain required for future execution.

`cmd/grayling-growth-agent/main_test.go` builds and invokes the actual binary
against local HTTP fixtures covering READY, HOLD, NEEDS_DATA, draft/implementation
blockers, malformed/oversized/injected input, scope/version mismatches, denied
access, redirects, deadlines, signal cancellation, secret-safe output and zero
non-GET requests. A real API-handler case with scoped viewer credentials confirms
that a local report conservatively remains HOLD/needs_data. These checks run with
the existing short/core race gates; no live API or paid service is contacted.

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
  hold/unknown, or has any entry in `decision.guardrailBreaches` (even with
  `healthy=true` or empty breach fields). The original server decision remains visible even if readiness
  is HOLD.
- `READY`: all requested evidence is present and passes these checks. This means
  usable evidence, **not** approval, a winner, or permission to publish. Server
  `continue` and `rollback` remain those exact verdicts.

Report metadata is checked at the shared evidence boundary: `registrationVersion`
must match `^sha256:[0-9a-f]{64}$` exactly, and `analysisMethod` must be one of
`two-proportion-z`, `welch`, `ratio-delta`, `quantile`, `cuped` or
`bandit-beta-bernoulli`. Missing, malformed, unsupported or instruction-bearing
values produce HOLD, block approval recording and block both plain preparation
and preparation linked to an earlier approval. Review reports `needs_data`;
blocked approval/preparation emits no artifact and uses fixed diagnostics without
echoing the invalid metadata. Rejection and request-changes can still record the
blockers. Monitor uses the same six-method allowlist; its existing registration
pin mismatch error remains unchanged.

Recognizing a Bandit report permits observation only. It does not convert its
server verdict to a fixed-sample conclusion or expand the draft compiler's
existing two-arm, fixed-split method restrictions. Server verdicts remain intact,
and all HTTP requests remain GET. Library and actual-binary regression fixtures
cover these local contracts; they do not establish model generation, durable
orchestration or real Saker pilot acceptance. All documented gates, including the
three disposable PostgreSQL gates below, remain required and unchanged. No schema,
public API or model integration is added.

Guardrail breaches block review with `needs_data`/exit 3, approval recording,
and both plain and review-linked preparation exports; no approval or preparation
artifact is emitted. Rejection and request-changes records retain readiness
blockers. Monitoring and learning retain HOLD, the exact server verdict, and
false publication/business-win claims. Fixed diagnostics do not echo breached
metric or detail prose, and all requests remain GET. Otherwise healthy reports
without breaches remain READY for `rollback`, `continue` and `ship`; no statistics
are calculated by this check. Reader and actual-binary fixtures cover this local
behavior; real Saker mapping, durable orchestration and pilot acceptance remain
outstanding.

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
required for this read-only slice. Full issue #20 acceptance (including real Saker
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

## Read-only prepare command

`prepare` reuses `Reader.Read`, `ReviewProposal` and `CompileCreateRequest`.
It re-reads the proposal's exact pinned evidence using the same scope,
credential, origin, GET allowlist and total timeout as `review`:

```sh
./bin/grayling-growth-agent prepare \
  --proposal proposal.json --settings preparation.json \
  --base-url https://grayling.example.com \
  --workspace ws1 --environment env1 \
  --token-env GRAYLING_GROWTH_TOKEN --timeout 10s > prepared.json
```

Keep `preparation.json` under operator control, separate from model/proposal
JSON. It is a regular UTF-8 JSON file of at most **16 KiB**. Missing files,
FIFOs/devices/directories, null/array roots, malformed/trailing JSON, unknown
members at every depth and duplicate members (including escaped aliases) are
rejected before network access. Authorization, approval, state and embedded
evidence members are outside the allowlist and are rejected. Credentials still
come only from the named environment variable. No settings are inferred from
proposal prose or supplied as statistical defaults.

Example explicit settings for the synthetic proposal below:

```json
{
  "allocationMode": "fixed-split",
  "integrationOwner": "onboarding team",
  "alpha": 0.05,
  "power": 0.8,
  "analysisWindowHours": 48,
  "guardrails": [{
    "ref": {
      "key": "retention", "version": 1,
      "definitionHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    "maxRelativeRegression": 0,
    "minSample": 100
  }]
}
```

These are illustrative planning inputs, not recommended statistical thresholds.
The compiler requires explicit positive alpha/power in (0,1), observation hours,
owner and exact ordered guardrail references with minimum samples. An explicit
zero regression threshold is valid; omission is rejected. Unsupported allocation,
analysis methods and variant counts are rejected by the existing compiler, with
no second statistical/platform validator. Semantic settings checks run after the
evidence read and readiness review.

On success stdout contains one `PreparedExperiment` JSON object with `request`
and `metadata` siblings. Only `request` is the existing experiment-create DTO;
`metadata` contains proposal ID/version, evidence pins, audience planning claims
and attribution hours. Preserve both for human review. The command never submits
an experiment, approves it, binds it or changes rollout state.

Exit 0 means export succeeded; 3 means proposal/evidence readiness blockers; 2
means invalid inputs, settings or unsupported compiler configuration; 1 means
execution/evidence failure (including access denial or timeout); 130 means signal
cancellation. Failures emit no draft on stdout and only bounded fixed or
compiler-owned field/code diagnostics on stderr. A shell redirection may create
an empty file on failure; check the exit code before consuming it. Review output
and exit behavior are unchanged.

Actual-binary local HTTP fixtures cover successful export, explicit zero,
malformed/duplicate/unknown/trailing and nonregular settings, embedded authorization
and evidence, missing statistical settings/thresholds, unsupported configurations,
HOLD/NEEDS_DATA, pinned scope/version mismatches, denied credentials, deadlines,
signal cancellation, secret-safe diagnostics and zero non-GET requests. These
fixtures establish local command behavior only; they do not validate real Saker
instrumentation or pilot/business outcomes. Existing Grayling validation, RBAC,
approval, targeting/binding and rollout gates remain required.

### Review-linked preparation

```bash
bin/grayling-growth-agent prepare --proposal proposal.json --settings preparation.json \
  --review-record review-record.json --base-url http://127.0.0.1:27002 \
  --workspace ws1 --environment env1
```

The optional `--review-record FILE` accepts the exported `growth.review.v1`
record from `record-review`. Before HTTP access, it reuses `DecodeReviewRecord`
and `VerifyReview` and requires `approve`, matching explicit workspace/environment,
proposal ID/version, complete proposal fingerprint, ordered primary/guardrail
roles and copied evidence pins including windows. Same-version edits and forged
copied pins fail even when the record's fingerprint string is unchanged.
`reject` and `request_changes` remain valid historical records but block this path.

Files must be regular, nonsymlink files, at most 128 KiB including whitespace;
actual reads are bounded too. Unknown/duplicate members (including escaped aliases
and nested members), invalid UTF-8, trailing JSON and null are rejected. Empty
paths, directories, FIFOs and devices fail without blocking. Invalid files exit 2
with `invalid review record file or JSON`; inconsistent or nonapproval records
exit 2 with `invalid or inconsistent preparation review`. These failures have
empty stdout and make zero HTTP requests. Diagnostics never echo paths or content.

After preflight the command reads fresh, exactly pinned evidence via GET and
reuses `CompileCreateRequest`; every readiness and compiler blocker remains.
Unhealthy/missing evidence exits 3 without a draft; execution errors retain the
existing exit 1/130 behavior, and invalid settings retain compiler diagnostics.
Successful output is deterministic JSON containing the existing `request` and
`metadata` plus separate `reviewProvenance`: a bounded `verification` summary
(scope, proposal identity/fingerprint, decision, recorded readiness and
`publicationAuthorized: false`), caller-attested `reviewer` and UTC `reviewedAt`.
Rationale, historical blocker prose and copied pins are not repeated in this
summary. The create request schema and payload remain unchanged.
Without the flag, preparation keeps its existing contract and output shape.

The pure library wrapper `CompileReviewedCreateRequest(proposal, record,
workspace, environment, freshEvidence, settings)` verifies linkage then delegates
to `CompileCreateRequest`, returning nil on any failure. Callers reading evidence
can first use pure `VerifyPreparationReview` for the same preflight. Evidence is
supplied independently by trusted caller code, never reconstructed from the review.
Records remain caller-attested consistency artifacts: they do not authenticate
reviewers, approve separately supplied preparation settings, prove historical
readiness, or authorize submission/publication. Current readiness is recomputed.
RBAC, platform approval, targeting/binding and rollout gates still apply.

Contract and actual-binary local HTTP fixtures cover approval linkage, stale
same-version edits, forged copied pins/roles, negative decisions, strict JSON and
file boundaries, fresh unhealthy/missing evidence, invalid settings, unchanged
requests, deterministic output, fixed diagnostics and zero non-GET requests.
These fixtures establish local behavior only, not real Saker pilot results.
All existing CI gates, including the disposable PostgreSQL invocation below,
remain required and unchanged.

Local validation on 2026-10-30 for review-linked preparation and the PostgreSQL
recovery fixture passed: `make build`, short Go tests, `go vet ./...`, core race
tests, boot/E2E/smoke tests, `make sdk-test` (25 Python and 23 TypeScript tests
plus typechecking), frozen-lockfile frontend tests, `make check-static`, license
audit and its unit tests, and `git diff --check`. All three PostgreSQL gates
passed against a disposable PostgreSQL 16 container with their original timeouts;
the full race-enabled Agent suite took 248.408 seconds and PostgreSQL E2E took
4.070 seconds. The container was removed on completion.

The restored SDK evaluation probe now clears `CollectToken` through its options
and closes explicitly before replay checks. A complete stored-event row comparison
before and after the probe verifies that evaluation leaves events unchanged.
Original collection, deduplication, recovery, SRM, per-variant and decision checks
remain intact; SDK runtime behavior is unchanged. This is local fixture evidence,
not real Saker pilot acceptance, durable orchestration or model integration.

## Optional create-request compiler

`growth.CompileCreateRequest(proposal, envelope, preparation)` is a pure library
function, exposed by the read-only `prepare` command below. It has no submission
transport and performs no platform writes. Supply the envelope separately from trusted
`Reader.Read` caller code; never decode evidence or preparation settings from
model output. `ReviewProposal` must return `ready_for_review` before compilation.
HOLD, NEEDS_DATA, missing implementation claims and scope/version/hash/window
mismatches produce a bounded `PreparationError` containing only a field and
code. All failures return a nil result, with no partial draft.

The initial supported subset is exactly two ordered variants, explicit
`fixed-split` allocation and the existing fixed-sample analysis methods (`welch`,
`two-proportion-z`, `ratio-delta`, `quantile`, `cuped`). Unsupported methods,
Bandit and multi-arm configurations are rejected. Compilation preserves variant
order and percentage allocations, workspace/environment scope, flag, hypothesis,
unit, onRender trigger, exact primary/guardrail metric key/version/hash, supplied
analysis method, MDE, minimum sample and runtime. It does not infer settings from
rationale, attestations or rollback prose.

Trusted `PreparationInputs` must separately provide allocation mode, integration
owner, alpha and power in (0,1), positive analysis observation hours and one
`GuardrailPreparation` per proposal guardrail in the same order. Each guardrail
requires the exact metric reference, a non-nil finite nonnegative regression
threshold (fraction, e.g. 0.05 for 5%) and a positive minimum sample. Zero is a
valid explicitly supplied threshold. No statistical settings are defaulted or
calculated. Nonfinite, missing, mismatched and overflowing inputs are rejected;
hour-to-nanosecond conversions check overflow before multiplication.

`AnalysisWindowHours` is supplied independently of proposal attribution hours;
it must cover every pinned primary and guardrail catalog attribution duration,
which may differ from one another. The API's `analysisWindow`
and `minRuntime` serialize as integer nanoseconds with both Go JSON packages.
The metric GET allowlist projects the exact `attributionWindow` integer
nanoseconds. New proposals require a positive, overflow-safe whole-hour
`analysisPlan.attributionWindowHours` exactly equal to the pinned primary
catalog duration. Missing/nonpositive catalog durations and mismatched claims
block readiness, approval and preparation; missing/nonpositive guardrail durations
also block readiness. Sub-hour or fractional-hour primary definitions cannot be
represented by this proposal schema and must be revised through human review,
never rounded. Guardrails may use sub-hour or longer durations; preparation's
analysis window must cover each exact duration. Invalid wire types or numbers
outside int64 fail evidence decoding. Historical monitoring and learning use the exact report/catalog comparison
below; they do not require whole-hour durations. Grayling still resolves exact metric references and authoritatively
validates experiment creation. These checks neither prove statistical adequacy
nor authenticate human attestations; approval, binding and publishing gates remain.
Local fixtures establish no real Saker instrumentation or pilot outcomes.

```go
threshold := 0.05 // explicit trusted preparation input, not extracted prose
prepared, err := growth.CompileCreateRequest(*proposal, envelope, growth.PreparationInputs{
    AllocationMode: "fixed-split", IntegrationOwner: "onboarding team",
    Alpha: 0.05, Power: 0.8, AnalysisWindowHours: 48,
    Guardrails: []growth.GuardrailPreparation{{
        Ref: proposal.Guardrails[0].Ref,
        MaxRelativeRegression: &threshold, MinSample: 100,
    }},
})
if err != nil { return err }
requestJSON, err := json.Marshal(prepared.Request) // only this DTO is API input
// prepared.Metadata is retained separately; no submission follows here.
```

`CreateRequest` explicitly allowlists fields compatible with
`platform.Experiment`. It omits identity, revision, epoch, lifecycle timestamps
and state, registration fingerprint, approvals and release plans. It emits
references without metric specifications; existing Grayling services resolve
those specifications, check published flags and metrics, enforce RBAC and final
legality, allocate epochs and register Draft state. Preparation metadata keeps
proposal ID/version and evidence pins outside the API request. Audience remains
a planning claim in that metadata because the experiment create API does not
encode targeting; a human must verify audience/flag targeting and binding.
Rollback prose is not converted into an executable release plan.

Compiler contract tests cover deterministic mapping, unsupported configurations,
HOLD/NEEDS_DATA, evidence mismatches, injection, duration overflow and input
immutability. An isolated in-memory real create-handler fixture round-trips the
generated JSON, checks viewer denial, exact frozen v1 metric references despite
a newer published version, Draft state and invalid/deprecated reference rejection.
These are local compatibility checks, not real Saker evidence or production
validation. Human review and explicit submission authorization, Grayling approval
and binding checks, real instrumentation, durable orchestration and pilot/business
acceptance remain outstanding. No existing CI jobs or platform gates are changed.


## Pure human-review records

`ProposalFingerprint(proposal)` returns `sha256:<lowercase hex>` over the complete
serialized proposal, including planning prose, implementation claims, nested
windows, metric references and array order. JSON uses deterministic encoding and
RFC 8785 canonical formatting with integer rounding disabled, so int64 versions
above 2^53 retain their exact value. Serialized null/empty distinctions remain significant.
It rejects invalid UTF-8, nonfinite values and proposals over 64 KiB. Fingerprinting
an incomplete draft does not make it ready. Any content change requires a new
fingerprint even when the proposal version was not incremented.

`RecordReview(proposal, envelope, ReviewInputs)` is a pure library contract in
`internal/growth/review.go`. Supply the envelope separately from trusted
`Reader.Read` code and the review inputs separately from proposal/model data:

```go
fp, err := growth.ProposalFingerprint(proposal) // pin what the human reviewed
if err != nil { return err }
record, err := growth.RecordReview(proposal, envelope, growth.ReviewInputs{
    ExpectedProposalFingerprint: fp,
    Decision: growth.ReviewApprove,
    Reviewer: "growth-owner",
    ReviewedAt: reviewedAt, // explicit caller-supplied time.Time
    Rationale: "Reviewed this pinned proposal and implementation claims.",
})
```

Retain the fingerprint from the reviewed snapshot; recomputing it after edits
would lose the stale-review protection. Decisions are exactly `approve`, `reject`
and `request_changes`. Reviewer and proposal/scope IDs use the existing bounded
identifier syntax (1–128 ASCII letters/digits/underscore/dot/hyphen, starting with
letter/digit/underscore; neither `.` nor `..`). Proposal version must be positive
and schema supported. Review time must be explicit, nonzero and within JSON's
UTC year range 1–9999; records normalize it to UTC. Rationale requires nonblank
valid UTF-8 of at most 4096 bytes and excludes controls other than newline/tab.
No identity, time, decision or rationale is inferred from proposal prose.

Approval requires the existing `ReviewProposal` result to be `ready_for_review`
against the separately supplied evidence. Missing/HOLD evidence, scope or evidence
pin mismatches and incomplete implementation claims block approval. Rejection and
requested changes remain recordable for those proposals, preserving readiness
state and blockers. Invalid inputs or stale fingerprints return a nil record and
fixed `ReviewError` field/code values, without caller text or a partial artifact.

The record preserves schema, proposal ID/version, workspace/environment, complete
proposal fingerprint, primary/guardrail metric pins, report evidence pins, reviewer,
UTC review time, rationale, decision and readiness blockers. Nested slices are
defensive copies; identical inputs replay deterministically without reading a
clock or writing state. Evidence pins are proposal claims checked by readiness;
a blocked rejection/changes record does not certify those claims or evidence
availability. The fingerprint covers the proposal, not the complete Reader envelope.

**Records are caller-attested review artifacts, not authenticated authorization,
durable audit storage, or permission to submit/publish.** The caller remains
responsible for authenticating the reviewer and retaining the reviewed proposal,
evidence and record. The review-record commands below expose this contract without platform writes,
model calls, submission, publication or durable workflow. Existing Grayling RBAC,
approval, binding and rollout gates remain mandatory. Real Saker instrumentation,
pilot evidence and full issue acceptance remain outstanding.

Contract tests cover full-proposal fingerprint coverage, large integer versions,
stale content under an unchanged version, malformed review inputs, missing/HOLD
and mismatched evidence, incomplete implementation, injected approval prose,
input immutability and deterministic replay. Existing real-handler integration
coverage shows local HOLD cannot produce an acceptance record, while rejection
and requested changes preserve blockers with zero HTTP writes. These fixtures
are local validation only; no new database or browser surface is introduced.

## Review-record commands

Fingerprint the exact proposal the operator reviewed, offline:

```sh
./bin/grayling-growth-agent fingerprint --proposal proposal.json
```

Stdout is one `sha256:<lowercase hex>` line. This command accepts only
`--proposal`, requires no credentials or API configuration and performs no network
access. It reuses `ProposalFingerprint`; incomplete drafts can be fingerprinted
without becoming ready. Proposal input is a regular UTF-8 JSON file of at most
64 KiB, with the same strict decoder as `review` and `prepare`.

Keep a separate operator-owned `review.json`, never extracted from model/proposal
prose, with exactly these five members:

```json
{
  "expectedProposalFingerprint": "sha256:REPLACE_WITH_THE_REVIEWED_PROPOSAL_FINGERPRINT",
  "decision": "request_changes",
  "reviewer": "growth-owner",
  "reviewedAt": "2026-10-28T12:00:00Z",
  "rationale": "Please resolve the recorded readiness blockers before approval."
}
```

The fingerprint placeholder is intentionally invalid: replace it with the saved
fingerprint of the reviewed snapshot. Never automatically recompute or refresh
the expected fingerprint when exporting a decision; proposal edits require a new
human review. Reviewer, timestamp, decision and rationale have no defaults.
The pure review contract's identifier, timestamp and rationale limits apply.

```sh
./bin/grayling-growth-agent record-review \
  --proposal proposal.json --review-input review.json \
  --base-url https://grayling.example.com \
  --workspace ws1 --environment env1 \
  --token-env GRAYLING_GROWTH_TOKEN --timeout 10s > review-record.json
```

Review input must be a regular UTF-8 JSON file of at most **16 KiB**.
Before network access, the command rejects missing/nonregular/oversized files,
null/array roots, invalid UTF-8, malformed or trailing JSON, unknown members,
duplicate members (including escaped aliases), embedded authorization/evidence,
invalid review fields and stale fingerprints. Proposal members are also strictly
allowlisted at every depth. A rationale is literal operator content, never an
instruction to bypass readiness or authorization.

`record-review` reuses `Reader.Read` and `RecordReview`. It reads exact pinned
metric/report evidence with existing trusted origin, workspace/environment,
environment-variable credentials, GET allowlist, total timeout (default 10s,
maximum 30s) and signal cancellation. It never replaces the supplied expected
fingerprint. `approve` requires `ready_for_review`; `reject` and `request_changes`
export records for successfully read HOLD/NEEDS_DATA evidence while retaining
readiness state and blockers. Transport, access denial and response scope/version
mismatches fail for every decision and emit no record.

Success emits exactly one `ReviewRecord` JSON object and exits 0, including
blocked rejections/changes. Exit 3 means blocked approval; 2 means invalid input
or configuration; 1 means evidence/execution failure; 130 means signal cancellation.
Failures emit no record on stdout and only fixed bounded secret-safe diagnostics
on stderr. Check exit status before consuming redirected output.

**Exported records contain operator-supplied review content. They are
caller-attested artifacts, not authenticated authorization, durable audit storage,
or permission to submit or publish.** Review content and proposal evidence pins
are deliberately preserved in the artifact; treat exported files accordingly.
Existing `review` and `prepare` behavior and all Grayling approval, binding and
rollout gates remain required.

Actual-binary local HTTP fixtures cover all three decisions, stale fingerprints,
malformed/unknown/duplicate/trailing and embedded authorization/evidence inputs,
blocked approvals, HOLD/NEEDS_DATA blocker retention, scope/version mismatch,
denied access, timeouts, cancellation and zero non-GET requests. Offline binary
fixtures cover fingerprint output, incomplete drafts and strict bounded input.
These establish command behavior only. No real Saker instrumentation, pilot
evidence, model generation, durable orchestration or full acceptance is claimed;
no new database, browser or SDK surface or CI job changes are introduced.

## Read-only monitor command

`monitor` observes the **actual linked experiment**, independently of proposal
baseline evidence. Supply a separate operator-owned manifest and the same trusted
origin, scope flags and environment credential as `review`:

```sh
./bin/grayling-growth-agent monitor --manifest monitor.json \
  --base-url https://grayling.example.com --workspace ws1 --environment env1 \
  --token-env GRAYLING_GROWTH_TOKEN --timeout 10s
```

Synthetic `monitor.json` (replace every ID and hash with trusted actual values):

```json
{
  "workspaceId": "ws1",
  "environmentId": "env1",
  "experimentId": "exp1",
  "epoch": 1,
  "registrationVersion": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "primaryMetric": {
    "key": "activation", "version": 1,
    "definitionHash": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  },
  "guardrails": [{
    "key": "retention", "version": 1,
    "definitionHash": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  }]
}
```

The manifest is a regular UTF-8 JSON file of at most 16 KiB. Unknown members,
trailing JSON, duplicate members (including escaped aliases), invalid references,
duplicate metric keys/roles and more than 16 guardrails fail before any HTTP
request. Use an empty guardrail list only for an experiment with no guardrails.
Workspace/environment must exactly match the trusted CLI flags. Registration is
a required SHA-256 pin; experiment epoch and primary/guardrail key/version/hash
are required and checked exactly against the report. Guardrail order is immaterial;
missing, additional, duplicate or swapped roles fail without a summary.
An optional `evidenceVersion` SHA-256 member pins one exact report snapshot.
Omitting it observes the current report, without changing the other pins.

`Reader.Monitor` reuses `Reader.Read`: only exact metric-version and experiment
report-export GETs, scoped credentials, bounded responses, no redirects, retries
or HTTP writes. The CLI retains the total positive timeout (default 10 seconds,
maximum 30 seconds), SIGINT/SIGTERM cancellation and fixed stderr diagnostics.
No proposal text, report health/reason/error prose or guardrail details are
exported or interpreted as instructions or causes.

Each successful read exports one `MonitorSummary`: scope, experiment/epoch,
exact metric references, observed evidenceVersion, canonical report source path,
generation time, registration version, analysis method and window/watermark.
The window omits its free-form issues. Unknown analysis methods/verdicts are
represented by empty strings and fixed blockers rather than arbitrary server text.
When a report is missing, observed report metadata remains empty/zero; no evidence
version, timestamp, method or verdict is invented. Missing evidence gives
`NEEDS_DATA`; otherwise blockers give `HOLD`. Error responses and scope/version,
registration or metric-role mismatches produce no partial stdout summary.

`serverVerdict` preserves `continue`, `ship`, `rollback` or `hold` separately from
`status` (`READY`/`HOLD`/`NEEDS_DATA`). Healthy `continue`, `ship` and `rollback`
can all be `READY`; a server `hold` stays `HOLD`. **READY means evidence can be
inspected and never authorizes publication.** `publicationAuthorized` is always
false. Existing RBAC, human approval, binding and rollout gates remain mandatory.
Exit codes: 0 READY; 3 HOLD/NEEDS_DATA; 2 invalid manifest/configuration;
1 evidence execution failure; 130 signal cancellation.

Blockers are deduplicated in deterministic check order, bounded by the checks
below. Codes and remediation actions are implementation-owned:

| Blocker | Remediation |
| --- | --- |
| `evidence_missing` | `restore_evidence_access` |
| `metric_lifecycle_invalid`, `primary_attribution_invalid` | `review_metric_definitions` |
| `verdict_invalid`, `server_hold`, `health_review_required`, `srm_review_required` | `review_diagnostics` |
| `analysis_method_invalid` | `review_registration` |
| `exposure_mode_invalid` | `review_registration` |
| `generation_time_missing`, `diagnostics_missing`, `diagnostics_metadata_invalid` | `refresh_report` |
| `window_metadata_invalid` | `review_window` |
| `window_incomplete` | `wait_for_complete_window` |
| `window_not_recomputable` | `restore_recomputable_evidence` |
| `watermark_late` | `wait_for_watermark` |
| `aggregates_missing` | `restore_aggregate_evidence` |

<a id="server-report-guardrail-checks"></a>
### Server report guardrail checks

The evidence projection retains only each `report.guardrails` entry's `breached`
boolean. Metric names, details, thresholds and unknown fields are omitted.
Reading and monitoring share validation: any supplied `breached:true`, missing
boolean, null boolean or null entry requires HOLD, even when `healthy:true` and
`ship`, `continue` or `rollback` disagree. Monitor records the fixed
`health_review_required` / `review_diagnostics` blocker.

Omitted, null and empty guardrail arrays remain compatible. This check does not
infer missing guardrail coverage or recompute thresholds. Each supplied check
must explicitly report `breached:false` to pass; all other existing checks still
apply. Malformed array, entry or boolean types fail with `ErrResponse` and no
partial artifact or response-body diagnostics.

Proposal review inherits HOLD and refuses approval; rejection and requests for
changes retain blockers. Both preparation paths check fresh evidence, including
when using a prior approval. Learning export, offline verification and search
preserve the observed HOLD without reconstructing or authenticating server
checks. Library and actual-command fixtures cover all supported methods and
non-HOLD verdicts, mixed checks, missing/null booleans, malformed values, text
redaction, false controls and GET-only access. Real-handler compatibility checks
compare the boolean projection with the existing server report.

Local validation for slice 29 on 2026-10-30 passed all three supplied gate
commands: build, short tests, all-package build/vet, core race, boot/E2E/smoke,
SDK tests, frozen-lockfile install, frontend tests/static build, license audit
and its unit tests, and diff whitespace checks. Disposable PostgreSQL 16 tests
passed for platform/report/API, the full race-enabled Agent integration suite
(228.191 seconds), and PostgreSQL E2E (4.395 seconds). The cleanup trap removed
the container; no gate or timeout was weakened.

These local fixtures establish software behavior only. Real Saker pilot
acceptance, model integration and durable orchestration remain outstanding.
No schema, persistence, SDK, public API, statistical decision or publication
behavior changes; all documented mandatory gates and timeouts remain unchanged.

<a id="server-srm-health"></a>

Fixed-split evidence requires an explicitly true server `srm.healthy` for every
supported fixed-split analysis method. Omitted/null `srm`, omitted/null `healthy`,
and `healthy: false` produce HOLD even when report `healthy` is true, issue lists
are empty and the server verdict is `ship`, `continue` or `rollback`.
`bandit-beta-bernoulli` may omit SRM because its assignment semantics differ;
an explicitly supplied null, incomplete or unhealthy SRM still blocks it.
Malformed SRM objects or non-boolean healthy values fail the entire read with a
fixed response error, without exporting partial evidence or server text.

The evidence allowlist retains only SRM health, preserving omitted/null members;
it drops SRM counts, statistics, thresholds and extra prose. One shared check
serves reading and monitoring. Monitor exports `srm_review_required` /
`review_diagnostics`, and learning export, offline validation and search retain
that blocker. Offline learning artifacts contain the observation, not the raw
SRM report: verification checks local consistency and cannot reconstruct or
authenticate server SRM health. Approval and both preparation paths refuse blocked
evidence; rejection/request-changes preserve readiness blockers.
No statistics or thresholds are recomputed. True SRM health passes only this
check: every other health, metadata, attribution and readiness gate still applies.

Unhealthy flags, calculation errors, issue presence or guardrail breaches request
a generic diagnostics review; the command uses structured SRM health, explicit
contamination/assignment counts and supplied journey maturity counts. It does not
infer these causes from free-form server text. Window
completeness/recomputability, watermark, metadata and aggregate presence use
structured fields. Both report and embedded diagnostics windows are checked.

Deterministic contract tests and more than 20 actual-binary HTTP cases cover
healthy verdicts, health/injection text, missing evidence, window failures,
strict manifests, registration/metric-role/epoch/scope/version conflicts,
authorization, timeout/cancellation and zero non-GET requests. The real-handler
library and actual-binary fixtures establish conservative local HOLD. These
fixtures prove local command behavior only, not real Saker instrumentation,
statistical/business acceptance or a successful pilot. Durable monitoring,
rollout-history linkage, model generation and real Saker acceptance remain future
work; no persistence, database schema, SDK, browser or CI behavior changes.

## Read-only learning records

`Reader.RecordLearning(ctx, proposal, monitorManifest, LearningInputs)` returns a
`growth.learning.v1` library artifact. Supply an explicit
`ExpectedProposalFingerprint` from `ProposalFingerprint`, `Recorder` identifier,
`RecordedAt` timestamp and 1–20 nonblank `Followups` (each at most 4096 UTF-8
bytes; control characters other than newline/tab are rejected). Timestamps are
normalized to UTC without reading a clock. Identical inputs and report snapshots
produce identical records; nested slices are detached from caller inputs.

Before any HTTP request, the library checks the proposal schema, identity and
positive version, required hypothesis, complete proposal fingerprint, reader and
manifest workspace/environment, and exact primary/guardrail metric-role agreement.
Changing proposal content at the same version invalidates the expected fingerprint.
The manifest supplies the actual experiment ID, epoch, registration version and
optional evidence-version pin. `Reader.Monitor` performs the existing read and
report-pin, health and window checks; learning adds no statistical decision logic.
Authorization, transport, cancellation and report-pin failures return no record.

The record retains proposal identity/fingerprint and the original hypothesis,
`proposalBaselineReferences` separately from `linkedExperiment`, and the
`observation` monitor status, fixed blocker/remediation codes, server verdict,
observed evidenceVersion, registration version, source path, generation time and
window/watermark. Baselines are copied proposal claims and are not fetched as the
linked run. When a report is missing, observed report metadata stays empty,
including the source path; requested pins remain only in `linkedExperiment`.
A present report can still retain observed metadata with NEEDS_DATA when a metric
is missing. Server reason/issue prose is never exported in the observation.

The experiment link, original hypothesis, baseline references, recorder, timestamp
and follow-ups are **caller attestations**. They do not prove the proposal was
executed, that an action occurred, or that the recorder is authenticated. Treat
retained caller text as inert untrusted content. HOLD and NEEDS_DATA remain
explicit. `continue`, `ship` and `rollback` retain their server meaning; READY or
`ship` does not establish business superiority. `businessWinEstablished` and
`publicationAuthorized` are always false. Existing RBAC, human approval, binding
and rollout gates remain mandatory.

The library and export command add no persistence/search, authenticated audit
storage, model integration, public API, SDK or database changes. Table-driven local
HTTP fixtures cover healthy verdicts, health/window failures, missing evidence,
stale fingerprints, malformed operator inputs, scope/metric/registration/epoch
conflicts, injection, cancellation, deterministic replay, nested immutability and
zero HTTP writes. The in-memory real-handler fixture preserves HOLD; these are
library behavior checks, not real Saker instrumentation, pilot or business-win
validation. The documented build, short tests, vet, core race, boot/E2E/smoke,
frontend and static gates remain required and unchanged.


## Record-learning command

Export a single caller-attested LearningRecord with independently supplied files:

```sh
./bin/grayling-growth-agent record-learning \
  --proposal proposal.json --manifest monitor.json --learning-input learning.json \
  --base-url https://grayling.example --workspace ws1 --environment env1 \
  --token-env GRAYLING_GROWTH_TOKEN --timeout 10s > learning-record.json
```

`proposal.json` uses the existing strict proposal contract; `monitor.json` uses
[the monitor manifest](#read-only-monitor-command). The operator-owned
`learning.json` must be a regular UTF-8 JSON file of at most 128 KiB:

```json
{
  "expectedProposalFingerprint": "sha256:<64 lowercase hex digits pinned when reviewed>",
  "recorder": "growth-owner",
  "recordedAt": "2026-10-28T03:00:00Z",
  "followups": ["Review the next complete observation window with the owner."]
}
```

Replace the fingerprint placeholder with the previously pinned value from
`fingerprint --proposal proposal.json`. Never refresh the expected fingerprint
merely to bypass a stale-input failure. The command passes the supplied pin
unchanged to `Reader.RecordLearning`; it never replaces it with a fresh value.
All 20 bounded follow-ups supported by the library fit within this file limit.
Unknown members, duplicate members (including escaped aliases), trailing JSON,
invalid UTF-8, nonregular files, invalid operator fields, stale fingerprints and
local scope/metric conflicts are rejected before HTTP access. The existing
proposal and manifest bounds and strict decoding also apply.

Trusted origin, scope, credential environment-variable selection, GET-only
transport, redirect restrictions, total timeout (default 10s, maximum 30s), and
SIGINT/SIGTERM cancellation follow the existing commands. Proposal baselines are
retained separately from the actual linked experiment; only that linked run is
read. Caller follow-ups and hypotheses remain inert text, never instructions.

Exit 0 exports exactly one record for READY. Exit 3 also exports exactly one
record, preserving HOLD or NEEDS_DATA and observed provenance. Exit 2 indicates
invalid local input/configuration; exit 1 indicates an evidence failure or
timeout; signal cancellation exits 130. These failures emit no record and fixed,
secret-safe diagnostics. Both businessWinEstablished and publicationAuthorized
remain false, regardless of the server verdict. Records are caller attestations,
without authenticated audit storage, persistence, model integration,
experiment submission or real Saker pilot/business acceptance.

`cmd/grayling-growth-agent/learning_test.go` builds and invokes the actual binary
against local HTTP fixtures for healthy/blocked/missing evidence, strict files,
stale pins, local and server conflicts, access denial, timeout, both signals,
injection and zero non-GET requests. These fixtures establish local command
behavior only. All existing CI jobs and documented gates remain required.

## Offline search-learning command

Search explicitly supplied exported `growth.learning.v1` records without a
server, credentials, evidence refresh or an index:

```sh
./bin/grayling-growth-agent search-learning \
  --workspace ws1 --environment env1 --text 'first task' \
  --record learning-record.json --record earlier-learning-record.json
```

All three scope/text flags are required. `--record` is repeatable; directories,
symlinks, FIFOs, devices and other nonregular inputs are rejected. There is no
recursive discovery, stdin input, glob expansion by the command, credential
option or network destination. Shell expansion remains the caller's responsibility.
Limits are 64 supplied files, 128 KiB per file and 4 MiB total actual input bytes
(including whitespace and repeated files). Text is a nonblank UTF-8 substring of
at most 4096 bytes, matched case-sensitively against `originalHypothesis` or any
`followups` entry. No regular expressions, semantic matching or text execution
are performed. Proposal baseline text and recorder identifiers are not searched.

Every file is strictly decoded and validated before any output, including files
outside the selected scope. Unknown and duplicate members (including escaped
aliases), trailing JSON, invalid UTF-8, unsupported schemas, invalid identities,
versions, text bounds and linked/observed scope, experiment, epoch, metric-role,
registration, evidence-pin or source-path conflicts fail the whole request.
`businessWinEstablished`, either `publicationAuthorized` field, and inconsistent
missing-evidence snapshots are rejected. Monitor's fixed status/blocker pairs
and missing-window explanations are checked structurally; no statistical or
health decision is recomputed. Baseline experiment references stay separate from
the linked run and must carry valid experiment/epoch/evidence/source references.
A missing report is valid only as `NEEDS_DATA` with the missing-evidence blocker
and empty observed provenance; a requested manifest pin remains a caller claim.

Success returns a single `growth.learning-search.v1` JSON object containing
`workspaceId`, `environmentId` and `matches`. Matches preserve complete record
snapshots, including the proposal fingerprint, baseline references, linked
experiment, observed evidence version/window/watermark, HOLD/NEEDS_DATA, recorder,
time and inert follow-ups. Other scopes and input file paths are never included.
Results sort lexicographically by canonical record JSON bytes, independent of
input order; byte-identical canonical records collapse to one match. No matches
returns `matches: []`. Exit 0 means the search completed, including when results
are HOLD/NEEDS_DATA; input/configuration errors return 2 with fixed diagnostics
and no JSON, and output failures return 1. Output is fully prepared before writing.

The pure `growth.SearchLearning` contract and bounded `DecodeLearningRecord`
reuse the existing `LearningRecord` schema. Contract and actual-binary tests
cover literal matching, scope isolation, deterministic ordering/deduplication,
provenance conflicts, missing evidence, strict JSON, file/byte limits, nonregular
files and inert injection text. These are caller-attested local artifacts:
structural consistency does not authenticate a recorder, verify the proposal
fingerprint against an absent proposal, establish freshness or causality, prove
execution, or authorize publication. Search provides neither authenticated audit
storage nor the full learning journal, durable orchestration, model integration,
real Saker pilot or complete issue acceptance. Existing PostgreSQL and all other
CI jobs remain required; no database, public API, SDK or browser surface is added.

## Read-only rollout observation

`Reader.Rollout(ctx, RolloutManifest)` and the independent command inspect only
`GET /api/v1/admin/experiments/{id}`. The embedded `releasePlan` comes from that
same snapshot; no separately timed, unscoped release-plan response is used.
Existing monitor and learning contracts remain unchanged.

```sh
./bin/grayling-growth-agent rollout --manifest rollout.json \
  --base-url https://grayling.example.com --workspace ws1 --environment env1 \
  --token-env GRAYLING_GROWTH_TOKEN --timeout 10s
```

An operator-owned manifest contains exactly these members (replace synthetic IDs
and fingerprint with actual pinned values):

```json
{"workspaceId":"ws1","environmentId":"env1","experimentId":"exp1","epoch":1,"registrationVersion":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

The manifest must be a regular file at most 16 KiB. Unknown/duplicate members,
trailing JSON, malformed references and scope conflicts fail before HTTP (exit 2).
The command reuses Reader origin, environment-variable credentials, redirect rejection,
total timeout (positive, at most 30 seconds), signal cancellation (exit 130), and fixed
non-sensitive diagnostics. Denial, malformed responses and identity/scope/epoch/version
conflicts return exit 1 with no observation output. There are no retries or writes.

`availability: available` (exit 0) means the experiment snapshot was validated;
`planPresent: false` means that existing experiment has no plan. HTTP 404 instead
returns `availability: experiment_missing` (exit 3), without experiment timestamps
or a plan. A missing experiment cannot validate the manifest pins against a response.
Availability is independent of the plan's `armed`, `running`, `paused`, `completed`
or `rolled_back` lifecycle. `publicationAuthorized` is always false. Completed
never establishes a business win or authorizes publication.

Output carries the pinned identity, internally constructed source path, experiment
updatedAt, plan presence, known state/current step, numeric step configuration
(ratio, minRuntime, observationWindow, autoAdvance), manual override, plan timestamps,
and recent history timestamps/actions/step indices. Durations are integer nanoseconds.
Names, actors, creators, pause reasons and history detail prose are omitted even if
containing instructions. Unknown response members are ignored; duplicate members
and malformed allowlisted fields fail the entire read. Ratios strictly increase in
(0,100], minimum runtime is positive, observation window is nonnegative, and their
sum cannot overflow int64. At most 32 steps and 64 history events are accepted,
matching current platform bounds. Current/history indices must refer to existing
steps; armed starts at zero and completed refers to the final step. Running and
completed require a step start timestamp. Unsupported states/actions fail closed.

History is bounded recent server history, **not a complete authenticated audit export**.
Local contract, actual-binary HTTP and real-handler viewer/isolation fixtures verify
these boundaries and zero non-GET requests; they do not prove real Saker acceptance.
Durable orchestration, model generation and real pilot acceptance remain outstanding.

The Agent integration gate remains the full race-enabled package with its original
five-minute timeout. Its independent preparation, registration, release-loop,
discovery, applied/runtime WSS and embedded app/naming fixtures, plus isolated
repreparation, activation refresh, Applied storage, preparation gate and revocation
proof worker scenarios, overlap with at most four active fixtures; they own separate PostgreSQL databases where needed, ephemeral listeners
and temporary runtime directories. The slot is retained through client, worker,
server, pool and database cleanup. Real
polling, renewal, expiry and safety waits and all assertions remain unchanged.
The blocked-builder revocation fixture explicitly closes its client. The bundle
refresh rejection fixture compares complete durable resource, head and operation
rows, including the publication-created revision-zero revocation head. These are
local software checks, not pilot or business acceptance. The uint64 policy fixture
advances only the routing head, preserving the independent revocation catalog.

### Shared local PostgreSQL for integration gates

Use one disposable, loopback-only PostgreSQL 16 server for the three PostgreSQL
gates in [Go quality](../.github/workflows/go-quality.yml). With Docker available,
run this whole block in a Bash shell from the repository root:

```bash
bash -euc '
export PATH="$HOME/.npm-global/bin:/usr/local/bin:/usr/local/go/bin:$PATH"
export GOWORK=off
unset GRAYLING_TEST_PG_SKIP
pg_container=$(docker run -d --rm -e POSTGRES_PASSWORD=grayling-test \
  -p 127.0.0.1::5432 postgres:16-alpine)
trap "docker rm -f \"$pg_container\" >/dev/null" EXIT
ready=0
for attempt in $(seq 1 60); do
  if docker exec "$pg_container" pg_isready -U postgres >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
test "$ready" = 1
pg_port=$(docker port "$pg_container" 5432/tcp | sed -n "s/^127\\.0\\.0\\.1://p")
test -n "$pg_port"
docker exec -e PGPASSWORD=grayling-test "$pg_container" \
  psql -h 127.0.0.1 -U postgres -d postgres -v ON_ERROR_STOP=1 -c "SELECT 1" >/dev/null
export GRAYLING_TEST_PG_DSN="postgres://postgres:grayling-test@127.0.0.1:$pg_port/postgres?sslmode=disable"

go test -race -count=1 -timeout=15m \
  ./internal/platform ./internal/experiment ./internal/api \
  -run "TestGORMPostgres|TestPostgresReport|TestQuotaAdminReplicaRead"
go test -tags=integration -race -count=1 -timeout=5m ./test/agent
go test -count=1 -timeout=10m ./test/e2e -run "^TestE2EPostgres"
'
```

The password above is disposable fixture configuration. The SQL probe checks
TCP authentication as well as readiness; a TCP listener alone is insufficient.
The trap stops the container even when a gate fails. Alternatively, use a dedicated
local PostgreSQL server (version 14 or newer) with an explicit admin DSN and an
account allowed to create and drop databases; never use production data.

With an explicit DSN, `test/pgtest` creates unique scratch databases and drops them
during test cleanup. A bad explicit DSN fails instead of silently falling back to
Docker. Reuse the server across all three commands. Keep the full Agent package,
race detection, assertions and original timeouts shown above; the other build,
unit, vet, SDK, frontend and static gates remain required. A run with
`GRAYLING_TEST_PG_SKIP=1`, missing Docker or unavailable fixtures does not validate
these PostgreSQL gates. Local database and WSS checks establish fixture behavior,
not real Saker pilot results or production acceptance.


Local validation on 2026-10-30 passed all three PostgreSQL gates against a
disposable PostgreSQL 16 container: the full race-enabled Agent package completed
all 144 top-level scenarios without skips in 259.421 seconds under the unchanged
five-minute timeout. Scratch database cleanup left zero test databases. Build,
short tests, vet, core race tests, binary/E2E/smoke tests, frontend tests, static
consistency and whitespace checks also passed. These timings describe this local
run, not a CI performance guarantee.

The Python collect scheduling repair was validated locally on 2026-10-30:
all 25 Python SDK tests passed in `make sdk-test`. Explicit-flush cases use the
existing 3600-second interval while preserving their assertions. A separate
real-binary background case covers conversion and manual-exposure delivery with
bounded polling of cumulative counters, zero drops, server diagnosis/report
checks and an empty subsequent explicit flush. SDK behavior is unchanged.
The background case also passed in isolation. Build, short tests (after installing
locked frontend dependencies), vet, core race, boot/E2E/smoke, frontend tests,
static consistency and whitespace checks passed again for this repair.
All three PostgreSQL gates passed again against a disposable PostgreSQL 16
container, including the full race-enabled Agent package in 243.750 seconds
under the unchanged five-minute timeout and PostgreSQL E2E in 4.071 seconds.
The container was removed on completion.

The TypeScript collect scheduling repair was validated locally on 2026-10-30:
`make sdk-test` passed TypeScript typechecking, all 23 TypeScript tests and all
25 Python tests. The previously failing attribution case now uses the existing
3600000-millisecond interval for explicit flushing, as do the other explicit-flush
cases, preserving attribution, rejection, diagnosis and report assertions.
A separate real-binary background case passed both in isolation and in the full
suite, checking conversion and manual-exposure delivery with bounded cumulative
counter polling, zero drops, diagnosis/report deltas and an empty subsequent
explicit flush. SDK runtime behavior is unchanged. Build, short tests (after
installing locked frontend dependencies), vet, core race, boot/E2E/smoke,
license, frontend tests, static consistency and whitespace checks also passed.
All three PostgreSQL gates passed against a disposable PostgreSQL 16 container,
including the race-enabled Agent suite in 247.950 seconds under the unchanged
five-minute timeout and PostgreSQL E2E in 3.945 seconds. The container was removed
on completion.
This gate repair does not establish model generation, durable orchestration,
real Saker instrumentation, pilot outcomes or complete Growth Agent acceptance.

## Offline verify-learning command

Compare a caller-supplied exported record with its original proposal:

```sh
./bin/grayling-growth-agent verify-learning \
  --proposal proposal.json --record learning-record.json \
  --workspace ws1 --environment env1
```

The pure `growth.VerifyLearning(proposal, record, workspace, environment)` function
reuses `ProposalFingerprint` and the existing learning-record validation. It requires
exact proposal schema, identity, version and explicit scope; recomputes the fingerprint
of every proposal field (including same-version edits); and compares the original
hypothesis, every baseline field in array order, and exact primary/guardrail metric
roles (key, version and definition hash). Guardrails are compared as a role set.
Proposal baselines remain distinct from the linked experiment: they may reference
other experiments, epochs and windows. A matching fingerprint string alone cannot
excuse inconsistent copied fields. This does not require proposal readiness or add
statistical decisions.

Inputs are strict UTF-8 JSON: unknown or duplicate members (including escaped
aliases), trailing values, malformed JSON and null roots are rejected. Proposal
files are limited to 64 KiB; records to 128 KiB, including whitespace. Reads are
bounded even if files grow, reject symlinks and require regular files; nonblocking
opens reject FIFOs and other special files. The documented proposal, record, goal and scope flags are
accepted: credentials, API origins, timeouts and network configuration cannot be
supplied. No credentials or proxy environment variables are read, no network
requests occur, and no artifact or index is written. Hypotheses and follow-ups are
inert data, never instructions or shell commands.

Success emits one deterministic `growth.learning-verification.v1` JSON summary
with explicit scope, proposal identity/version/fingerprint, linked experiment ID
and epoch, observed evidenceVersion (empty when missing), observationStatus and
`publicationAuthorized: false`, `businessWinEstablished: false`. Prose and file paths
are omitted. READY, HOLD and NEEDS_DATA can all be consistent and exit 0; blocked
observations retain their status and never become successful experiment claims.
Invalid configuration, files or mismatched artifacts exit 2 with fixed diagnostics
and no summary; output failures exit 1. Serialization completes before writing.

Verification establishes local consistency of caller-supplied artifacts only.
It does not authenticate authors or recorder identity, prove execution, establish
freshness or causality, refresh evidence, authorize publication or establish a
business win. A caller can fabricate a mutually consistent pair. Durable
orchestration, model generation and real Saker pilot acceptance remain outstanding.
Contract and actual-binary tests cover valid/blocked observations, same-version
edits, mismatched copies despite matching fingerprint strings, role/scope conflicts,
strict input and size boundaries, inert injection text and deterministic output.
Existing CI jobs and mandatory gates remain unchanged; no DB, SDK, public API or
browser behavior is added.

## Read-only binding readiness

`Reader.BindCheck` and the optional `bind-check` command reuse the strict rollout
operator manifest (at most 16 KiB):

```json
{"workspaceId":"ws1","environmentId":"env1","experimentId":"exp1","epoch":1,"registrationVersion":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

Replace these synthetic pins with trusted registration values. With a scoped
viewer credential already in `GRAYLING_GROWTH_TOKEN`, run:

```sh
./bin/grayling-growth-agent bind-check --manifest experiment.json \
  --base-url https://grayling.example.com --workspace ws1 --environment env1
```

The command reads the experiment, calls
`GET /api/v1/admin/experiments/{id}/bind-check`, then reads the experiment again.
It rejects mismatched IDs, scope, epoch, registration version or registered flag
revision, and changes to `flagRevision` or `updatedAt` across the experiment
reads. All nine checks are required exactly once: `state`, `published_flag`,
`draft_clear`, `registered_revision`, `fallback`, `unit`, `split`, `bandit_rule`,
and `existing_binding`. Required booleans and nonnegative revisions must be
present and valid; aggregate readiness must agree with the checks. A changed
current flag revision is a `registered_revision` blocker. Unknown/duplicate
checks or malformed evidence are execution failures, with no partial output.

Output contains pinned provenance, source paths, observed timestamp/revisions,
bounded check codes and fixed implementation-owned remediation text. Server
`detail` prose is omitted. Missing experiment or bind-check evidence yields an
explicit `experiment_missing` or `bind_check_missing` blocker. Exit 0 means all
observed prerequisites passed; exit 3 means missing evidence or failed
prerequisites. Existing codes remain: 2 invalid input/configuration, 1 access,
execution, mismatch or malformed evidence failure, and 130 cancellation.
Credentials, server prose and input values are never echoed in errors.

This is a **non-atomic observation** (`nonAtomic=true`): surrounding reads cannot
exclude intervening changes or changes after the final read. It never authorizes
binding or publication and always emits `publicationAuthorized=false`. Existing
human authorization, binding rechecks, approval and publication gates remain
mandatory. No submission, model integration, durable workflow, database schema,
SDK behavior, browser flow or real Saker acceptance is added or claimed.

## Offline verify-review command

Verify an exported caller-attested human review against the complete original
proposal, without credentials, network requests, persistence or authorization effects:

```bash
./bin/grayling-growth-agent verify-review \
  --proposal proposal.json --goal goal.json --record review-record.json \
  --workspace ws1 --environment env1
```

The command accepts the four required input flags and optional `--goal FILE` (plus help). It does not
accept API origins, tokens, timeouts, implicit environment scope or positional
arguments. Proposal decoding and complete canonical SHA-256 fingerprinting reuse
the existing contracts, preserving exact int64 versions above 2^53. All supplied input
paths must open as bounded regular files; symlinks, directories, FIFOs and devices
are rejected without waiting for a writer. The proposal limit is 64 KiB and the
review record limit is **128 KiB**, including whitespace; actual reads enforce
these limits even if files grow. Strict JSON rejects unknown members at every
depth, duplicate/escaped duplicate names, invalid UTF-8, trailing JSON and null roots.

`--goal FILE` is required when the proposal contains `goalPin`; omission exits 2
with `goal file required for pinned proposal`. Unpinned proposals preserve legacy
verification when omitted and may opt into goal checking. Explicit empty paths,
including `--goal=`, fail. Before opening the review record or producing output,
`readGoal` reuses the bounded strict goal decoder (64 KiB including whitespace),
and `growth.VerifyGoal` checks the separately supplied operator workspace and
environment, goal identity, exact primary/guardrail roles and metric references,
and any goal version/content fingerprint pin. Integers remain exact above 2^53;
changes to goal content at the same ID/version fail a pinned fingerprint.
Missing/unreadable/nonregular/symlink/oversized or invalid goal files use
`invalid goal file or JSON`; linkage mismatches use
`invalid or inconsistent goal artifacts`. Both exit 2 with empty stdout and
never echo goal prose, file paths or secrets. Goal consistency does not clear
historical blockers or confer readiness or approval. Updating goal and proposal
consistently still leaves earlier review fingerprints stale. Matching goals add
no fields to the existing verification output.

`growth.VerifyReview` compares supported schemas, explicit workspace/environment,
proposal identity/version, the recomputed complete proposal fingerprint, every
copied primary/guardrail metric field and ordered evidence pins including all
window fields. Matching fingerprint strings cannot excuse forged copied fields.
Reviewer identity, review timestamp and rationale use the same rules as
`RecordReview` (identifier, nonzero supported timestamp, nonblank valid UTF-8
rationale at most 4096 bytes, no controls except newline/tab). Text stays inert.

Supported decisions are `approve`, `reject` and `request_changes`. Recorded
readiness must be `draft`, `needs_data`, `needs_implementation` or
`ready_for_review`. Approval requires recorded `ready_for_review` with zero
blockers. All ready records have zero blockers; blocked states retain 1–256
blockers, each with a nonblank valid UTF-8 field and code of at most 256 bytes
(no controls except newline/tab). Rejection/request_changes and incomplete
proposals remain valid historical artifacts when these structural rules and
copied pins agree. The verifier does not reconstruct an evidence envelope or
rerun historical readiness checks.

Success emits one prose-free deterministic `growth.review-verification.v1` JSON
line with scope, proposal identity/version/fingerprint, `decision`,
`recordedReadiness` and `publicationAuthorized=false`. It omits reviewer rationale
and proposal prose. Exit status is **0** for consistent artifacts (including
negative decisions or recorded blockers), **2** for invalid/mismatched input,
and **1** for output failure. Errors use fixed diagnostics and no artifact output.

Consistency of caller-supplied artifacts does not authenticate reviewers,
reconstruct historical evidence, prove historical readiness, authorize submission
or publication, or establish real Saker pilot outcomes. Model integration and
durable orchestration remain outstanding. Contract and credential-free actual
binary tests cover stale/forged artifacts, strict input boundaries, inert
injection text and deterministic output; these are local fixture checks only.

## Metric lifecycle validation

The internal allowlisted metric DTO now preserves the catalog `status` for
`draft`, `published` and `deprecated`. Missing or unrecognized status is exported
as an empty value; unknown server text is never echoed. No catalog schema,
public API or SDK contract changes are introduced.

New proposals require **published primary and guardrail definitions**, in
addition to the existing scope, exact version/hash, source path, report and
implementation checks. A matching definition with draft, deprecated, missing
or unknown lifecycle produces the fixed field/code blocker
`primaryMetric` (or `guardrails[i]`) / `metric_not_published` and `needs_data`.
An absent definition retains `metric_evidence_mismatch` and missing-evidence
handling. Approval records are refused. Rejection and request-changes records
retain these blockers. Both plain and review-linked preparation recheck fresh
evidence and fail without partial artifacts, including when a historical
approval exists. Published definitions remain eligible for the other checks;
publication alone does not establish proposal readiness.

Historical report observation uses a different rule: published and deprecated
frozen definitions remain valid evidence when their exact references match.
Deprecation cannot by itself clear or add a report health HOLD. Draft, missing
or unknown lifecycle evidence cannot become READY; invalid lifecycle yields
HOLD with `metric_lifecycle_invalid` / `review_metric_definitions` in monitoring
and learning, while absent definitions retain NEEDS_DATA / `evidence_missing`.
Learning verification and search accept this fixed blocker without clearing it.
Server verdicts and existing health checks remain authoritative; no observation
or review artifact authorizes publication or establishes a business win.

Library and actual-binary fixtures cover both metric roles, published controls,
negative review decisions, preparation with historical approvals, deprecated
historical monitoring/learning, missing lifecycle members, unknown text and
zero write requests. The real in-memory catalog/API fixture exercises
`draft → published → deprecated`, stable definition hashes, new-version
projection and frozen references. Its actual report remains conservatively
HOLD; synthetic healthy fixtures demonstrate software behavior only. Lifecycle
can change after any read: the existing create API must still revalidate it.
These local checks do not establish real Saker instrumentation, pilot outcomes,
model generation, durable orchestration or full issue acceptance.

Local validation for this lifecycle slice on 2026-10-30 passed `make build`,
the documented short Go suite, `go vet ./...`, the full core race suite,
boot/E2E/smoke, `make sdk-test` (25 Python and 23 TypeScript tests plus
TypeScript typechecking), locked frontend installation/tests, `make check-static`,
`make license-check` and `git diff --check`. The initial short run preceded
frontend dependency installation and fixture updates; the completed short and
race runs passed after those updates. No gate, assertion or timeout was weakened.
All three PostgreSQL gates also passed against one disposable, loopback-only
PostgreSQL 16 server: the GORM/report/quota race suite, the full race-enabled
Agent integration suite (222.576 seconds under the unchanged five-minute
timeout), and PostgreSQL E2E (4.173 seconds). The container was removed by the
cleanup trap. These results establish local fixture behavior only.


### Historical report exposure modes

Report evidence recognizes exactly `onDecision` and `onRender`. An omitted,
empty, unknown, differently cased or whitespace-padded `exposureMode` produces
HOLD even when all other evidence is healthy. Monitoring exports only the fixed
`exposure_mode_invalid` / `review_registration` blocker; unknown server text is
never copied into diagnostics or learning records. Learning recording, offline
verification and search retain that HOLD and blocker.

Proposal review inherits evidence HOLD: approval is refused, while rejection
and requests for changes retain the readiness blockers. Both ordinary and
review-linked preparation refuse invalid evidence without partial artifacts.
A supported historical `onDecision` baseline remains usable for review and
preparation of a new `onRender` proposal; new proposals still require real
render exposure and the existing implementation attestations.

Library and actual CLI process regressions cover omitted/empty/unknown/injection
values, both supported modes, negative reviews, preparation refusal, learning
export/verification/search round trips and zero non-GET requests. The existing
local real report-handler fixture covers both supported modes; incomplete local
reports still HOLD for their existing health/window reasons. These fixtures
establish software compatibility only, not real Saker instrumentation, pilot
outcomes, model generation, durable orchestration or full issue acceptance.
No public API, persistence, SDK, model integration or publication behavior changes.

### Report aggregate population consistency

Each variant must supply nonnegative counts satisfying
`convertedUnits <= randomizedUnits <= exposedUnits <= exposures`. Converted
units belong to the randomized analysis population; exposed units additionally
include deterministic targeting, and exposure events may repeat for a unit.
Equality, repeated exposure events, deterministic-only exposure populations and
explicit zero counts remain valid inputs to the other evidence checks. Counts
are compared directly as int64 values, including values above 2^53, without
subtraction, summation or conversion to floating point.

Missing, negative or contradictory aggregates produce evidence HOLD. Monitoring
retains the existing fixed `aggregates_missing` /
`restore_aggregate_evidence` blocker. Proposal approval and both ordinary and
review-linked preparation are refused without partial artifacts; rejection and
request-changes reviews preserve the readiness blockers. Learning export,
offline verification and search retain HOLD and its remediation rather than
turning a contradictory snapshot into READY.

This checks population consistency only. It does not recompute statistics,
require exposed and randomized populations to be equal, or interpret
`primaryValue` as a probability: mean, ratio and quantile reports can use other
units. Passing these comparisons does not establish sample adequacy, healthy
instrumentation, freshness, a business win or publication authorization.
Library and actual-command fixtures exercise all three ordering violations,
zero-count and maximum-int64 boundaries, valid populations, metric values outside
[0,1], negative reviews, preparation refusal, learning round trips and zero
non-GET requests. Existing real-handler compatibility fixtures remain in use;
local fixtures do not establish real Saker instrumentation, pilot outcomes,
model generation or durable orchestration. No persistence, public API, SDK,
browser or model integration is added.

Local validation for this aggregate slice on 2026-10-30 passed all three supplied
gate commands: `make build`, short tests, `go build ./...`, `go vet ./...`, the
full core race suite, boot/E2E/smoke, SDK tests (25 Python and 23 TypeScript plus
typechecking), locked frontend installation/tests/build, static consistency,
license inventory, all five license-audit unit tests and `git diff --check`.
The initial pre-change short run lacked frontend dependencies; the locked build
installed them and the complete subsequent short and race suites passed.
Disposable PostgreSQL 16 validation also passed the GORM/report/quota race
suite, full Agent integration race suite (266.756 seconds within the unchanged
five-minute timeout), and PostgreSQL E2E (4.674 seconds). The cleanup trap removed
the container. No gate, assertion or timeout was weakened; these are local
software checks only.

### Embedded diagnostics generation time

Embedded `diagnostics.generatedAt` must represent the same instant as the
report's `generatedAt`, compared with `time.Time.Equal` at nanosecond precision.
Earlier or later diagnostics produce HOLD, including a one-nanosecond difference.
Different timezone offsets for the same instant remain valid. Both generation
timestamps must still be present and nonzero; two missing times cannot become
READY merely because they compare equal.

Monitoring uses the existing `diagnostics_metadata_invalid` / `refresh_report`
blocker for mismatched or missing diagnostic generation times and retains
`generation_time_missing` / `refresh_report` for a missing report time. Proposal
review inherits evidence HOLD, approval is refused, and both ordinary and
review-linked preparation refuse the snapshot without partial artifacts.
Rejection and requests for changes retain readiness blockers. Learning export,
offline verification and search preserve HOLD and the same remediation.

Library and actual-command regressions cover earlier/later seconds and
nanoseconds, missing diagnostic/report/both times, and matching fractional
instants represented in UTC and positive/negative timezone offsets. They cover
review, approval refusal, negative reviews, both preparation paths, monitoring,
and learning export/verification/search while asserting zero non-GET requests.
Existing real-handler compatibility fixtures remain in use. These local fixtures
establish software consistency only; real Saker pilot evidence, model integration,
durable orchestration and full issue #20 acceptance remain outstanding. No public
API, schema, SDK, browser, statistical decision or publication behavior changes.
All documented mandatory gates and their timeouts remain unchanged.

Local validation for slice 25 on 2026-10-30 passed all three supplied gate
commands: `make build`, short tests, `go build ./...`, `go vet ./...`, the full
core race suite, boot/E2E/smoke, SDK tests (25 Python and 23 TypeScript plus
typechecking), locked frontend installation/tests/build, static consistency,
license inventory, all five license-audit unit tests and `git diff --check`.
The pre-change short run lacked frontend dependencies; after the locked build
installed them, the complete short and race suites passed. Existing real-handler
compatibility fixtures passed in the library and actual-command suites.
Disposable PostgreSQL 16 validation passed the GORM/report/quota race suite,
full Agent integration race suite (258.275 seconds within the unchanged
five-minute timeout), and PostgreSQL E2E (4.306 seconds). The cleanup trap removed
the container. No gate, assertion or timeout was weakened; these results establish
local software consistency only.

### Watermark generation bound

The report window and embedded diagnostics window must each have
`watermark <= report.generatedAt`. Comparisons use instants at nanosecond
precision; equality and equivalent positive/negative timezone offsets remain
valid. This is an internal consistency check, independent of the local clock:
there is no freshness limit. Existing missing-time, window order, completeness,
recomputability and matching-diagnostics requirements still apply.

A watermark after generation produces evidence HOLD and the existing fixed
`window_metadata_invalid` / `review_window` monitor blocker. A diagnostic window
that differs from the report also retains `diagnostics_metadata_invalid` /
`refresh_report`. Review inherits HOLD, approval is refused, and ordinary and
review-linked preparation refuse the evidence without partial artifacts.
Rejection and request-changes records retain readiness blockers.

Learning export, offline verification and search preserve properly blocked HOLD
records. Offline validation rejects a copied report window with a future
watermark unless its observation retains `window_metadata_invalid` /
`review_window`; READY or unrelated HOLD blockers cannot conceal it. Embedded
diagnostic windows are not copied into learning observations, so their fixed
blockers remain the historical observation of that check.

Library and actual CLI process regressions cover future seconds/nanoseconds,
report-only and diagnostics-only contradictions, equality, earlier watermarks,
equivalent offsets, negative reviews, both preparation paths, monitoring,
learning round trips and forged offline records, with zero non-GET requests.
Existing real-handler compatibility fixtures remain in use. All documented
mandatory gates, the full Agent integration suite and existing timeouts remain
required. Local fixtures establish software behavior only; real Saker
instrumentation, pilot acceptance, model generation and durable orchestration
remain outside this slice.

Local validation for slice 26 on 2026-10-30 passed all three supplied gate
commands: `make build`, the complete short suite, `go build ./...`,
`go vet ./...`, the full core race suite, boot/E2E/smoke, SDK tests (25 Python,
23 TypeScript plus typechecking), locked frontend installation/tests/build,
static consistency, license inventory, all five license-audit unit tests and
`git diff --check`. The pre-change short run lacked frontend dependencies;
after the locked build installed them, the complete short and race suites
passed, including existing real-handler fixtures. Disposable PostgreSQL 16
validation passed GORM/report/quota race tests, the full Agent integration race
suite (229.415 seconds under the unchanged five-minute timeout), and PostgreSQL
E2E (4.372 seconds). The cleanup trap removed the container. No gate, assertion
or timeout was weakened; these results establish local software behavior only.

## Primary report attribution consistency

The report allowlist includes `primaryAttributionWindow` as exact int64
nanoseconds. Before report evidence can be `READY`, it must be positive and
equal to `attributionWindow` from the catalog definition with the report's exact
primary metric key, version and definition hash. Reader and monitor share this
comparison. Omitted, null, zero, negative or mismatched durations give `HOLD`,
including a one-nanosecond difference. No rounding, floating-point conversion or
substitution from a guardrail/current metric version is allowed. Exact positive
sub-hour durations and int64 maximum values remain valid historical evidence
for published or deprecated frozen definitions, subject to all other health gates.
Malformed numeric types, fractions, exponent notation and values outside int64
fail response decoding with no partial artifact. Missing metric/report evidence
continues to give `NEEDS_DATA`.

Monitor exports the fixed `primary_attribution_invalid` /
`review_metric_definitions` blocker. Proposal review inherits evidence HOLD,
approval fails, and rejection/request-changes retain readiness blockers. Both
preparation paths require fresh ready evidence, including preparation with an
older approval. The existing new-proposal schema still requires whole-hour
attribution claims; historical sub-hour acceptance does not change that schema.

Learning export, offline verification and search preserve the recorded HOLD and
fixed remediation. Offline artifacts do not contain the historical catalog
snapshot needed to reconstruct this comparison; offline verification checks local
consistency and never establishes catalog validity, freshness, real instrumentation
or a business win. Library and actual-binary regressions exercise wire boundaries,
review/preparation, learning round trips and zero non-GET requests. Real report
handler compatibility tests retain the server's duration and conservative HOLD.
These local fixtures establish software behavior only. Public APIs, statistics,
persistence and publication behavior remain unchanged. Durable orchestration,
model integration and real Saker pilot evidence remain required for full #20
acceptance.

## Supplied journey maturity evidence (slice 30)

The read-only report projection retains `journeys[].arms[].immature` and only
`excluded.immature_cohort` / `excluded.attribution_window_short`. It discards
metric labels, journey definitions, variant labels, numerator/denominator
statistics and arbitrary exclusion keys. Counts remain exact int64 values;
wrong JSON types, duplicate members and overflow fail the read without a partial
artifact or server-body diagnostics.

Reading and monitoring share the maturity check. Every supplied section needs
at least one arm; every supplied arm needs an explicit non-null `immature: 0`.
Positive or negative immature counts, or nonzero counts under either fixed
exclusion key, force HOLD even with `healthy: true` and a server verdict of
ship, continue or rollback. Omitted/null/empty journey lists remain compatible
with non-journey reports. This checks supplied evidence only: it does not infer
missing journey/arm coverage, calculate retention, or recompute statistics.

Review inherits HOLD, approval fails, and both ordinary preparation and
preparation linked to an earlier approval reject the evidence. Monitoring keeps
`health_review_required` / `review_diagnostics`, the original server verdict,
and `publicationAuthorized: false`. Learning export, verification and search
retain the blocked observation; offline verification cannot reconstruct or
authenticate journey evidence from the server.

Library and actual-binary HTTP fixtures cover missing/null/empty sections and
arms, missing/null/positive/negative immature counts, both fixed exclusions,
mixed arms/sections, healthy zero controls, malformed JSON, secret-safe output,
and zero non-GET requests across review, approval, both preparation paths,
monitoring and learning. These are local software checks, not real Saker pilot
acceptance. The documented build, short, vet, race, process/E2E, SDK,
frontend/static, license and all three disposable PostgreSQL gates remain
mandatory and unchanged. No schema, public API, model service, platform write,
publication authority or real pilot acceptance is included. Model integration,
durable orchestration and full issue acceptance remain outstanding.

Local validation for slice 30 on 2026-10-30 passed the supplied build, short,
Go build/vet, core race, boot/E2E/smoke, SDK, frontend/static, license-audit and
whitespace checks. After `make build` installed the missing frontend test
dependencies, the full short suite passed. Disposable PostgreSQL 16 validation
passed platform/report/API race tests, the full Agent integration race suite
(221.585 seconds, original five-minute timeout), and PostgreSQL E2E (4.930
seconds). The cleanup trap removed the container. No gate, assertion or timeout
was weakened; these results establish local software behavior only.


## Explicit contamination and assignment counts (slice 31)

The embedded diagnostics allowlist preserves `crossVariantUnits` and
`assignmentMismatch` as exact nullable signed 64-bit integer counts, following
`internal/experiment/diagnostics.go`. Both must be explicitly zero for READY.
Missing or null values project as null; negative or positive values, including
both int64 boundaries, require HOLD even with `healthy: true`, empty issues and
`ship`, `continue` or `rollback`. This applies to all six supported analysis
methods, including Bandit. Reading and monitoring share the same validation.
Malformed types, fractional numbers, int64 overflow and duplicate members reject
the entire read with the fixed response error; no decoder or response text is
included in diagnostics.

Review remains `needs_data`; approval recording and both plain and historically
approved preparation refuse artifacts. Rejection and request-changes retain the
blockers. Monitoring emits `health_review_required` / `review_diagnostics`, keeps
the original server verdict and never authorizes publication. Learning export,
offline verification and search preserve HOLD and false business-win/publication
claims. Unknown diagnostic fields, unit identities and free-form server details
are not added to the projection or fixed diagnostics.

Library and actual-binary HTTP regressions cover both fields independently,
missing/null counts, exact int64 boundaries, malformed input, zero controls,
all supported methods, negative reviews, earlier approvals, blocked learning
round trips, secret-safe output and zero non-GET requests. The existing real
report-handler fixture also compares the projected counts against server output.
These are local software contracts, not real contamination measurements or Saker
pilot acceptance. No statistics are recalculated, core APIs changed, production
writes made or model integration added. Full issue acceptance remains outstanding.
All existing build, short, vet, race, process/E2E, SDK, frontend/static, license
and three disposable PostgreSQL gates above remain mandatory with their original
timeouts and assertions.


Local slice 31 validation on 2026-10-30 passed `make build`, the full documented
short/build/vet/core-race and boot/E2E/smoke commands, SDK tests, frozen pnpm
installation, workspace frontend tests, static consistency, license checks and
license-audit unit tests. All three disposable PostgreSQL 16 gates passed:
platform/report/API race, full Agent integration race (224.073 seconds within the
unchanged five-minute timeout), and PostgreSQL E2E (4.232 seconds). The container
was cleaned up by the exit trap. `git diff --check` passed. These results establish
local software behavior only; full issue and real pilot acceptance remain open.


## Embedded diagnostics exposure mode

Embedded `diagnostics.exposureMode` must explicitly be `onDecision` or `onRender`
and exactly match `report.exposureMode`. Both matching modes permit historical
observation; new proposals retain their existing `onRender` restriction. Missing,
null, empty, unknown, differently cased, whitespace-padded or mismatched diagnostic
modes produce HOLD even with healthy counts and a server ship/continue/rollback
verdict. Unknown diagnostic mode text is cleared before exporting the reduced
evidence envelope. Wrong JSON types, duplicate members and malformed JSON reject
the entire read with the existing fixed response error and no partial evidence.

Monitor reports `diagnostics_metadata_invalid` / `refresh_report` for invalid or
mismatched modes. Absent diagnostics keep `diagnostics_missing` / `refresh_report`.
The server verdict is preserved, approval recording and both preparation paths
remain blocked, and learning export, offline verification and search retain HOLD.
Publication authority and business-win claims remain false.

Library lifecycle and actual-binary HTTP fixtures cover both matches, both mismatch
directions, missing/null/invalid modes, malformed responses, secret-bearing text,
review and preparation blocking, learning round trips and zero non-GET requests.
The existing real-handler fixture checks server-generated diagnostic modes when
present against the report in both historical modes. These local consistency
checks do not establish real Saker instrumentation, pilot outcomes, authenticated
human approval or completion of #20. All documented mandatory gates remain required.


Local validation on 2026-10-30 passed all three documented gate sequences:
`make build`, the complete short suite, build/vet, core race, boot/E2E/smoke,
SDK, frontend/static, license and whitespace checks. The disposable PostgreSQL 16
platform/report/API race suite, full Agent integration race suite (220.326 seconds
within the unchanged five-minute timeout) and PostgreSQL E2E (3.864 seconds) passed.
The cleanup trap removed the container. The focused diagnostics exposure lifecycle
tests also passed under the race detector. These are local software checks only.

## Optional embedded diagnostics SRM results (slice 33)

The diagnostics allowlist retains `assignmentSrm` and `exposureSrm` using the
same presence-aware, healthy-only projection as report SRM. Each optional result
may be omitted: the real diagnostics builder legitimately omits unavailable
results, including onDecision and Bandit observations. A supplied result must
explicitly contain `healthy: true`. Explicit null, an empty object, missing/null
health or `healthy: false` independently requires HOLD, regardless of report
health, report SRM, issue strings or the server's ship/continue/rollback verdict.
A healthy sibling cannot override an unhealthy result. This rule applies to all
supported analysis methods and both historical exposure modes.

Only the supplied health state crosses the boundary; statistics, thresholds,
identities and arbitrary server prose are discarded. Omission, null and missing
versus null health remain distinguishable in reduced evidence. Wrong JSON types,
duplicate members and malformed responses reject the entire read without partial
evidence or response-body disclosure. Reader and Monitor share validation;
Monitor uses the existing `srm_review_required` / `review_diagnostics` blocker.
The server verdict remains unchanged and publication authority stays false.

Library lifecycle and actual-binary HTTP regressions cover both fields separately
and together, omission, null/incomplete/unhealthy/healthy results, malformed wire
values, all supported methods and both exposure modes. They cover approval
blocking, rejection and change requests, both preparation paths, monitoring,
learning export/offline verification/search, secret-safe output and zero non-GET
requests. The real-handler fixture compares optional SRM health with server
output, including balanced assignments with skewed delivery (healthy assignment
SRM and unhealthy exposure SRM), and legitimate omission in both exposure modes.
All original build, short, vet, race, boot/E2E/smoke, SDK, frontend/static,
license, whitespace and three disposable PostgreSQL gates remain mandatory with
unchanged timeouts. These fixtures establish local software behavior only; full
model orchestration, real Saker pilot evidence and full #20 acceptance remain
outstanding. This slice adds no schema, public API, browser surface, model call
or platform write.

Local slice 33 recovery validation on 2026-10-30 retained the previous run's
completed non-database gates (build, short, vet, race, boot/E2E/smoke, SDK,
frontend/static, license and whitespace). The previously interrupted disposable
PostgreSQL sequence was rerun in full: platform/report/API race checks passed
(5.305 / 2.172 / 2.036 seconds), the full Agent integration race suite passed
in 217.766 seconds within the unchanged five-minute timeout, and PostgreSQL E2E
passed in 4.016 seconds. The cleanup trap removed the disposable container.
Targeted library and actual-binary diagnostic SRM lifecycle race checks passed,
including review, approval, both preparation paths, monitoring and learning.
After expanding the real-handler fixture to cover populated diagnostics and
legacy whole-object omission, the complete growth library race suite also passed
(129.744 seconds), followed by the growth library short tests, vet and whitespace
checks. These recovery results establish local software behavior only.

## Supplied diagnostic delivery gaps

The embedded `diagnostics.perVariant` projection retains only `unexposed` as an
exact nullable signed 64-bit integer. Variant labels, unit identities, other flow
counts and arbitrary diagnostic fields are discarded. For `onRender` reports,
every supplied entry must explicitly contain `unexposed: 0`. Missing/null entries
or counts, negative counts and positive counts independently require HOLD, even
with healthy summaries, empty issues and a server ship/continue/rollback verdict.
Reader and Monitor share validation; Monitor emits `health_review_required` /
`review_diagnostics` and preserves the server verdict.

Omitted, null and empty lists remain compatible. This validates supplied entries
only: omitted variant coverage is not inferred. Historical `onDecision` evidence
retains its existing semantics and is not blocked by supplied unexposed counts.
Wrong JSON types, fractions, exponent notation, int64 overflow and duplicate
members reject the response without partial evidence or server-body diagnostics.

Proposal review inherits HOLD, approval recording refuses it, and both ordinary
and earlier-approval-linked preparation refuse artifacts. Learning export,
offline verification and search preserve the blocked observation, false business
win claims and false publication authority. Offline artifacts cannot reconstruct
or authenticate the original diagnostic population.

Library lifecycle and actual-command HTTP regressions cover mixed entries,
missing/null/empty lists, both exposure modes, exact int64 boundaries, malformed
input, redaction and zero non-GET requests. The existing real-handler integration
fixture compares projected unexposed counts with server output, including its
balanced-assignment, skewed-delivery population. These are local software checks;
real Saker pilot evidence, model integration, durable orchestration and full #20
acceptance remain outstanding. No platform writes, statistical calculations or
publication authority are added. All documented gates and timeouts remain
mandatory and unchanged.

Local validation on 2026-10-30 passed all three documented gate sequences:
build, complete short suite, Go build/vet, core race, boot/E2E/smoke, SDK,
frontend/static, license and whitespace checks. The initial pre-build short run
lacked frontend test dependencies; after `make build` installed them, the full
short and race suites passed. Disposable PostgreSQL 16 checks passed for
platform/report/API, full Agent integration race (227.471 seconds within the
unchanged five-minute timeout), and PostgreSQL E2E (3.967 seconds). The exit trap
removed the container. These results establish local software behavior only.

## Caller-attested goals and offline linkage (slice 36)

`internal/growth/goal.go` adds `GrowthGoal` (`growth.goal.v1`): `id`, positive
int64 `version`, `workspaceId`, `environmentId`, caller-attested `owner`,
`objective`, `primaryMetric`, `guardrails`, and `baselineWindow: {start, end}`.
Identifiers and owner use the existing 1–128 byte identifier grammar; objective
is nonblank UTF-8 text up to 4096 bytes; only newline/tab control characters are
allowed. The observation
interval requires nonzero RFC3339 timestamps and end strictly after start. It
makes no completeness, watermark, baseline value or statistical adequacy claim.

Metric pins reuse proposal `{ref: {key, version, definitionHash}, sourcePath}`
objects: positive versions, lowercase `sha256:` hashes and exactly
`/api/v1/admin/workspaces/{workspace}/metrics/{key}/versions/{version}`.
There are 1–20 guardrails. Each metric key occurs once across primary and
guardrail roles; duplicate keys, conflicting hashes/versions and role overlap
are rejected. Paths are assertions, never network destinations.

```sh
grayling-growth-agent verify-goal --goal goal.json --proposal proposal.json \
  --workspace ws1 --environment env1
```

`DecodeGoal` strictly rejects unknown and duplicate members at every depth,
null roots, invalid UTF-8, malformed/trailing JSON, invalid fields/windows and
inputs larger than 64 KiB including whitespace. `ValidateGoal` and the pure
`VerifyGoal` also validate programmatic artifacts. The CLI requires explicit
operator scope and bounded regular files, opens with nonblocking/no-follow flags,
checks actual read limits, and accepts no credentials, API origin or network
options. All failures use fixed diagnostics without paths or input prose. It
prepares the complete output before writing; stdout write errors exit 1, invalid
input/linkage exits 2, and consistent linkage exits 0 without a readiness check.

Verification requires exact `proposal.goalId`, workspace/environment, primary
pin and guardrail pin set equality. Guardrail order does not affect matching.
Output (`growth.goal-verification.v1`) contains only bounded scope/goal/proposal
identifiers, exact versions, deterministic goal/proposal SHA-256 fingerprints,
and `publicationAuthorized: false`. Fingerprints cover complete canonical typed
JSON, preserve array order and exact int64 values (including above 2^53), and
change when caller text changes. Matching order independence does not make
reordered artifacts fingerprint-identical. Fingerprints are content identifiers,
not signatures or independently trusted expected hashes.

Existing proposal/review/preparation/learning schemas and workflows are unchanged.
Proposals carry only goal ID, so verification identifies the supplied goal
version; it cannot prove which historical version a proposal intended or prevent
a caller replacing both artifacts. It does not authenticate owners, validate BI
facts, establish readiness/approval, submit experiments, or prove real baseline
data, adequate samples or causal effects. Text stays inert. Default goal
enforcement, persistence, target modelling, BI facts, model generation, durable
orchestration and real Saker pilot acceptance remain later work; full #20
acceptance remains outstanding. Original mandatory gates/timeouts are unchanged.

Slice 36 local validation on 2026-10-30 passed all three supplied mandatory gate
sequences with unchanged timeouts: `make build`, complete short Go suite,
`go build ./...`, `go vet ./...`, complete core race suite, boot/E2E/smoke,
`make sdk-test`, frozen pnpm install, workspace frontend tests/build,
`make check-static`, license inventory check and its five unit tests, and
`git diff --check`. Core race results included growth library 228.092 seconds
and growth command 328.118 seconds. Disposable PostgreSQL 16 platform/report/API
race checks passed; full Agent integration race passed in 228.940 seconds within
the original five-minute timeout; PostgreSQL E2E passed in 3.923 seconds. The
exit trap removed the container. Focused goal library and actual-binary
regressions also passed. These are local software checks, not real baseline
validation, model evaluation, business outcomes or Saker pilot acceptance.


## Optional goal linkage before evidence reads (slice 37)

```sh
./bin/grayling-growth-agent review --proposal proposal.json --goal goal.json \
  --base-url http://127.0.0.1:8080 --workspace ws1 --environment env1
./bin/grayling-growth-agent record-review --proposal proposal.json --goal goal.json \
  --review-input review-input.json --base-url http://127.0.0.1:8080 \
  --workspace ws1 --environment env1
./bin/grayling-growth-agent prepare --proposal proposal.json --goal goal.json \
  --settings preparation.json --base-url http://127.0.0.1:8080 \
  --workspace ws1 --environment env1
# The reviewed preparation path also accepts --goal goal.json:
# add --review-record review-record.json to the prepare command above.
```

All three commands accept optional `--goal FILE`. When explicitly supplied,
including `--goal ""`, the command reuses `readGoal` / `DecodeGoal` and then
`growth.VerifyGoal` against the proposal and separately supplied operator
workspace/environment before any HTTP request. Empty paths/files, unreadable or
missing files, directories, pipes/devices, symlinks, files above 64 KiB and
invalid JSON/goal fields fail with exit 2, empty stdout and fixed diagnostic
`invalid goal file or JSON`. Valid artifacts with inconsistent goal identity,
scope, primary metric or guardrail sets fail with exit 2, empty stdout and fixed
`invalid or inconsistent goal artifacts`. Neither diagnostic echoes paths,
credentials, input text or decoder errors. Guardrail ordering does not affect
linkage; roles, keys, versions, definition hashes and canonical source paths
remain exact pins.

For unpinned proposals, omitting `--goal` preserves existing behavior. Supplying a matching goal does
not change any output schema or export a separate verification result. Fresh
GET-only evidence, readiness, explicit review decisions, expected proposal
fingerprints, preparation settings and any supplied review record remain
required under the existing rules. Goal consistency cannot clear HOLD or
NEEDS_DATA, imply approval, or make an older approval sufficient for preparation.
Reject/request_changes retain their existing ability to record successfully
read blocked evidence; approve still requires readiness. Both preparation paths
retain every readiness and compiler check, and no command submits anything.

Goals are caller-attested planning artifacts. Verification does not authenticate
ownership, prove baseline facts, or establish which historical goal version the
proposal intended when unpinned: legacy proposals pin only goal ID. Callers can replace both
artifacts. This adds no public API, persistence, model integration, submission,
production migration or real Saker pilot claim. Local actual-binary HTTP fixtures
exercise matching/reordered pins, mismatches, strict files and redaction,
request suppression, and HOLD preservation across both preparation paths; they
do not validate real instrumentation or business outcomes. The original build,
short/vet/race, boot/E2E/smoke, SDK/frontend/static/license and disposable
PostgreSQL acceptance gates remain mandatory and unchanged.


## Optional proposal goal pins (slice 38)

A proposal may include `"goalPin":{"version":9007199254740993,"fingerprint":"sha256:<64 lowercase hex digits>"}`.
Use the exact `GoalFingerprint` of the validated caller-supplied goal, and its
positive int64 version. JSON integers remain exact above 2^53. Missing members,
null pins, nonpositive/overflow/fractional versions, malformed or uppercase hashes,
unknown members and duplicate members are rejected. Direct library review marks
invalid pin values as draft blockers; `VerifyGoal` rejects them.

Absent pins are omitted from serialization, preserving legacy proposal fingerprints.
Supplied pins participate in the full proposal fingerprint, so editing either value
invalidates earlier review inputs and approval records even at the same proposal version.
`VerifyGoal` checks both values against the validated goal in addition to all existing
identity, scope and metric checks. Changing goal content at the same ID/version
therefore fails fingerprint matching.

For pinned proposals, `review`, `record-review` and `prepare` (with or without
`--review-record`) require `--goal FILE`. Omission exits 2 with empty stdout and
`goal file required for pinned proposal`. Missing/unreadable or invalid goal files
use `invalid goal file or JSON`; mismatched pins use
`invalid or inconsistent goal artifacts`. These failures precede all HTTP access.
A matching pin preserves fresh GET-only evidence checks, HOLD/NEEDS_DATA,
explicit review decisions and every preparation gate. Unpinned proposals keep
their existing behavior, including optional goal verification.

Pins establish caller-attested consistency only. They do not authenticate ownership,
prove baseline truth, establish readiness or grant publication authority. Callers
can replace both artifacts; this is no trusted history or signature mechanism.
Library and actual-binary local fixtures cover exact integers, content/version
mismatches, malformed pins, missing goals, legacy fingerprints, stale approvals,
HOLD preservation and zero non-GET requests. These establish software behavior
only; no public API, schema, model integration, platform writes or real pilot
acceptance is added. All previously documented mandatory gates remain unchanged.


## Learning goal linkage (slice 39)

`record-learning` and offline `verify-learning` accept `--goal FILE`:

```sh
./bin/grayling-growth-agent record-learning --proposal proposal.json --goal goal.json \
  --manifest monitor.json --learning-input learning.json \
  --base-url https://grayling.example --workspace ws1 --environment env1
./bin/grayling-growth-agent verify-learning --proposal proposal.json --goal goal.json \
  --record learning-record.json --workspace ws1 --environment env1
```

Pinned proposals require the goal: omission exits 2 with
`goal file required for pinned proposal`. Unpinned proposals retain existing
behavior when the flag is omitted and may opt into verification. Explicitly
supplied empty paths (including `--goal=`) are checked and rejected. Both commands
reuse `readGoal`, bounded `DecodeGoal` and `growth.VerifyGoal` with separately
supplied operator workspace/environment before HTTP access or learning output.
Missing, unreadable, symlink, nonregular, oversized or malformed goal files use
`invalid goal file or JSON`; valid but inconsistent identity, scope, metric roles,
version or complete content pins use `invalid or inconsistent goal artifacts`.
These failures exit 2 with empty stdout and never echo paths, goal text or secrets.
The 64 KiB limit includes whitespace; unknown/duplicate members at every depth,
invalid UTF-8, null roots and trailing JSON are rejected. Versions remain exact
int64 values above 2^53. Metric roles compare as sets; goal array order remains
part of a pinned goal's complete fingerprint.

Matching goals preserve the existing learning schemas and complete proposal
fingerprints, including goal pins. Changing both goal and proposal consistently
still invalidates earlier learning inputs and exported records. Export always
performs fresh GET-only monitoring; READY exits 0, HOLD/NEEDS_DATA export records
and exit 3. Offline verification preserves all three statuses and exits 0 for
consistent records without network access. Other exit codes are unchanged.
`publicationAuthorized` and `businessWinEstablished` remain false.

This checks caller-attested consistency only, not authenticated ownership,
historical execution, baseline truth, freshness or business causality. Actual
binary regressions in `learning_goal_linkage_test.go` use local fixtures only.
No public API, database schema, authenticated approvals, model integration or
production actions are added. All original gates and timeouts remain mandatory;
full Issue acceptance, durable orchestration and real Saker pilot evidence remain
outstanding.


## Offline review goal linkage (slice 40)

Offline `verify-review` now accepts optional `--goal FILE` and requires it for
pinned proposals, using the same strict bounded decoder and explicit-scope
linkage verification as learning commands. See [the command contract](#offline-verify-review-command)
for diagnostics, file limits and historical review semantics.
Actual-binary regressions in `verify_review_goal_test.go` cover exact pins and
versions above 2^53, missing/mismatched goals, scope and metric roles, same-version
content edits, strict JSON/file boundaries, stale review fingerprints, legacy
compatibility, historical decisions/readiness, redaction and offline execution.
These local fixtures establish software behavior only. No model integration,
persistence, platform writes, authenticated approvals or real pilot acceptance
is included. All documented mandatory gates, including disposable PostgreSQL
acceptance and their original timeouts, remain unchanged.

Local slice 40 validation on 2026-10-31 passed all three prescribed gate groups:
`make build`, the complete short suite, build/vet, full core race,
boot/E2E/smoke, SDK tests, frozen frontend installation/tests/build/static,
license checks and whitespace checks. The disposable PostgreSQL replica/report,
full Agent integration race (215.380 seconds, unchanged five-minute timeout),
and PostgreSQL E2E suites passed; the trap removed the container. The initial
pre-build short run lacked frontend test dependencies; after `make build`
installed them, the full short suite passed. These results establish local
software behavior only.
