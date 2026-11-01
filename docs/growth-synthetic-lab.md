# Local synthetic growth application

This optional executable exercises the complete engineering path without user
data, a cloud deployment, a real identity provider or a payment system. It is not
started by `grayling` or by the Growth service. Its explicit synthetic mode uses
owned SQLite stores, literal loopback listeners and an injected clock; production
store constructors always use wall time. There is no HTTP clock override or
simulation setting on the production server. Collection lateness, assignment,
exposure tickets, RBAC and publication checks remain enabled.

```sh
go build -o bin/grayling-growth-lab ./cmd/grayling-growth-lab
mkdir -m 700 /absolute/owned/private-lab
bin/grayling-growth-lab --synthetic --data-dir /absolute/owned/private-lab
```

The application prints its participant URL only. Private `endpoints.json` lists
application/platform/Growth origins. `state.json` contains generated local fixture
credentials, catalog definitions and a clock checkpoint; keep it private. Both
files are 0600. Never commit this directory or copy its tokens into a real system.
A second process cannot own the directory concurrently. All listeners require
literal loopback addresses; the participant host/origin check blocks rebinding
and cross-origin login. The command fails closed without `--synthetic`.

Two source-authored business paths are available: first key task completion and
simulated paid upgrade after activation. Each has a versioned primary metric, D1
retention guardrail, stable `user_id`, actual visible-render exposure, same-unit
conversion, and a 72-hour attribution window. No payment is attempted. The
simulated IdP accepts only `sim-000` through `sim-999`. It issues an opaque,
HttpOnly, SameSite=Strict session cookie. The same subject on two devices gets
stable bucketing. This is an identity simulator, not authentication of real users.
No SDK/collect/admin credential is returned to participant pages.

Open the Growth workbench origin from `endpoints.json`. Use the private editor
or reviewer token from `state.json`; `samples/activation.json` and `upgrade.json`
are usable create-goal bodies with exact catalog hashes. The explicit lab planner
produces clearly labelled synthetic hypotheses. It is not a live-model success
claim and it never contacts a model service. The normal Growth service still
defaults to AI off; see `growth-service.md` for opt-in provider configuration.

Select a hypothesis in the workbench, configure the implemented flag
(`activation_entry` or `upgrade_entry`), stable unit and visible-render trigger,
and supply implementation entries referencing the owned sample source/browser
evidence. Calculate a sample plan with explicit MDE/alpha/power/runtime inputs.
Review, prepare, explicitly submit, review on Grayling and bind. These steps use
the regular Growth endpoints and existing core validators. Binding does not
publish. The separately provisioned fixture owner/publisher can explicitly
approve/publish the bound flag and transition the experiment to running through
normal core Admin APIs. No Growth publish or transition tool exists.

Participants sign in, choose a business path and open the experience. The two
variants have different instructions and action presentation. Evaluation records
assignment only. The browser emits exposure after two animation frames while
visible, then conversion on a user action. Repeated conversions reuse the event
ID. Arbitrary event types, subjects and admin operations are not participant
inputs. Expired or forged assignment tickets are rejected by existing collection
validation. No collecting from real users is authorized by this sample.

The fixture operator alone can POST `{"hours":24}` to `/operator/advance` with
its separate bearer. Advances are bounded to 1–720 hours per request and durably
checkpointed. Use the same displayed decision's Return action on the next virtual
UTC calendar day for D1, then advance sufficiently for attribution and reporting.
An old exposure ticket expires normally; it cannot be reused to fake a later
render. Reports are recomputed by the real engine using the store clock.
Virtual days never establish actual elapsed retention or a seven-day pilot.
Small samples/data defects remain HOLD and cannot produce a winning learning.

`go test -race -count=1 ./internal/growthlab` runs both business paths, Chromium
workbench + participant flows, simulated cross-device identity, forged/expired
exposure rejection, origin/host boundaries, participant/admin separation, virtual
report maturity, durable learning and SQLite restart. A standalone lab SIGKILL test also restores both registered experiments and
actual browser-collected exposures without duplication. The Growth service
suite additionally kills a real service process mid-model call and verifies
bounded recovery, lost-write-response reconciliation and backup restoration.
Set `GRAYLING_GROWTH_LAB_EVIDENCE` to a private absolute output directory to retain
synthetic reports/results. The tests never enable live model calls implicitly.

For independent quality review, GET `/v1/tasks/{id}/study`: it includes anonymous
recommendation IDs in hash order, exact evidence and a blank rubric. POST a
version-checked `study` action to record an operator-supplied duration or review
score. These entries have authenticated actor/time provenance, but
`independentHumanVerified` always remains false. Never fill annotations on behalf
of a human or infer signatures/consent from authentication. The export separates
workflow elapsed time, readiness, blocked checkpoints, linked runs and learning
from supplied human-effort observations. Repeated blocks are not distinct defects;
valid data does not establish superiority. Real benefit/independent review remains
unmeasured until genuine evidence exists.

Use authenticated `/v1/archive` for a bounded consistent JSON evidence archive;
use the documented offline SQLite backup/verify/restore flow for recovery. No
evidence is silently deleted when a budget fills. Stop the lab before copying its
full directory; restart with the same directory to recover the platform, task,
learning and virtual clock. Participant sessions are intentionally invalidated
and must sign in again; the synthetic subject's deterministic assignment persists.
