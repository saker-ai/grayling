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
| Proposal contract | `DecodeProposal` / `ReviewProposal` in `internal/growth` | Strict, bounded planning JSON and evidence-based completeness for human review; no generation, review UI, approval or submission. |
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
