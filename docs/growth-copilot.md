# Growth Copilot: evidence and proposal contracts

Part of [#20](https://github.com/saker-ai/grayling/issues/20). `internal/growth`
is an optional Go library exposed by `cmd/grayling-growth-agent`, an independent
read-only command. No public route, SDK behavior, persistence, browser UI, model
call or production write is added. Never running the command leaves the
existing platform unchanged.

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
it must cover the latter, but they are never equated. The API's `analysisWindow`
and `minRuntime` serialize as integer nanoseconds with both Go JSON packages.
Attribution hours remain planning metadata: metric attribution specifications
are resolved by Grayling from the exact metric references.

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
| `verdict_invalid`, `server_hold`, `health_review_required` | `review_diagnostics` |
| `analysis_method_invalid` | `review_registration` |
| `generation_time_missing`, `diagnostics_missing`, `diagnostics_metadata_invalid` | `refresh_report` |
| `window_metadata_invalid` | `review_window` |
| `window_incomplete` | `wait_for_complete_window` |
| `window_not_recomputable` | `restore_recomputable_evidence` |
| `watermark_late` | `wait_for_watermark` |
| `aggregates_missing` | `restore_aggregate_evidence` |

Unhealthy flags, calculation errors, issue presence or guardrail breaches request
a generic diagnostics review; the command does not infer SRM, contamination,
retention maturity or any other cause from free-form server text. Window
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

This milestone adds no CLI/export command, persistence/search, authenticated audit
storage, model integration, public API, SDK or database changes. Table-driven local
HTTP fixtures cover healthy verdicts, health/window failures, missing evidence,
stale fingerprints, malformed operator inputs, scope/metric/registration/epoch
conflicts, injection, cancellation, deterministic replay, nested immutability and
zero HTTP writes. The in-memory real-handler fixture preserves HOLD; these are
library behavior checks, not real Saker instrumentation, pilot or business-win
validation. The documented build, short tests, vet, core race, boot/E2E/smoke,
frontend and static gates remain required and unchanged.
