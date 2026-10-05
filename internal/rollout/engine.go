// Package rollout implements the staged-release auto-advance engine
// (docs/技术方案.md §5.3 自动推进, P4): pre-registered plan steps and
// observation windows advance automatically — or the plan pauses — based on
// the fixed-sample decision gate of internal/experiment.
//
// Contract (§5.3: 自动推进只在观测窗口、数据完整性、最小样本及全部护栏均满足
// 时执行；统计不可用、SRM、推送异常时暂停，不自动宣称成功):
//
//   - Steps ramp the experiment ELIGIBILITY only. Eligibility is a prefix
//     interval on its own hash coordinate, so raising the ratio never moves
//     or un-enrolls an already-enrolled unit (§6). Proportions, unit, salt
//     and bindings belong to the pre-registered epoch and are never touched.
//   - Every engine action is IDEMPOTENT: state is recorded before the
//     republish, and each pass repairs a published ratio that disagrees
//     with the current step, so crashes/retries converge without duplicate
//     revisions or traffic oscillation.
//   - The engine PAUSES (and never auto-resumes) when the report is
//     unhealthy — SRM alarm, data-pipeline problems, or no exposures inside
//     the whole observation window. Rollback verdicts (guardrail breach or
//     significantly worse primary) stop enrollment by transitioning the
//     experiment to Stopped, which republishes the bound flag through the
//     normal convergence path.
//   - A ship verdict on the FINAL step completes the plan; the flag stays
//     at the final step's ratio. Full rollout remains a human publish
//     decision — the engine never auto-claims victory (§5.3).
//   - ManualOverride is the 人工接管开关 (§9): while set, the engine takes
//     no automatic action; operators drive the plan through the admin API.
//
// Each GoLab replica may run an engine. Plan actions take a per-experiment
// PostgreSQL advisory lock and reload state before deciding; the in-process
// mutex covers concurrent ticks and manual operations on one replica.
// The engine holds no credentials — it acts as an internal system identity that is
// audited like any admin actor.
package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/godeps/golab/internal/admin"
	"github.com/godeps/golab/internal/experiment"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/internal/stats"
	"github.com/godeps/golab/pkg/model"
)

// EngineActor is the system identity every automatic action is audited
// under. It is an in-process control-plane identity, never minted as a
// token, so it cannot be presented over the API (§9: 自动动作采用专用服务
// 令牌…任何信号不得绕过发布权限与安全默认值 — the engine still goes through
// the same admin.Service publish pipeline and RBAC-checked methods).
var EngineActor = admin.Actor{
	TokenID:     "system:rollout-engine",
	Name:        "rollout-engine",
	Kind:        platform.TokenAdmin,
	Role:        platform.RoleOwner,
	WorkspaceID: "",
}

// Engine drives release plans. It is safe to Tick from tests
// deterministically (inject Now) or run on a ticker in production.
type Engine struct {
	Store  *platform.Store
	Admin  *admin.Service
	Now    func() time.Time
	Logger *log.Logger
	mu     sync.Mutex
}

// NewEngine assembles an engine. now may be nil (wall clock).
func NewEngine(store *platform.Store, adm *admin.Service, logger *log.Logger) *Engine {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Engine{Store: store, Admin: adm, Now: time.Now, Logger: logger}
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// TickSummary counts what one engine pass did (observability + tests).
type TickSummary struct {
	Inspected     int      `json:"inspected"`
	Started       []string `json:"started,omitempty"`
	Advanced      []string `json:"advanced,omitempty"`
	Repaired      []string `json:"repaired,omitempty"`
	Paused        []string `json:"paused,omitempty"`
	RolledBack    []string `json:"rolledBack,omitempty"`
	Completed     []string `json:"completed,omitempty"`
	Errors        []string `json:"errors,omitempty"`
	BanditUpdated []string `json:"banditUpdated,omitempty"`
	BanditFrozen  []string `json:"banditFrozen,omitempty"`
}

// Tick runs one pass over every experiment with an engine-attention plan.
// Deterministic order (store returns ID-sorted); every action is idempotent.
func (e *Engine) Tick() TickSummary {
	now := e.now()
	var sum TickSummary
	for _, candidate := range e.Store.ExperimentsWithActivePlans() {
		sum.Inspected++
		release, err := e.lockPlan(candidate.ID)
		if err != nil {
			sum.Errors = append(sum.Errors, candidate.ID+": "+err.Error())
			continue
		}
		func() {
			defer release()
			exp, err := e.Store.GetExperiment(candidate.ID)
			if err != nil {
				sum.Errors = append(sum.Errors, candidate.ID+": "+err.Error())
				return
			}
			e.tickPlan(now, exp, &sum)
		}()
	}
	for _, candidate := range e.Store.ExperimentsWithBandit() {
		sum.Inspected++
		release, err := e.lockPlan(candidate.ID)
		if err != nil {
			sum.Errors = append(sum.Errors, candidate.ID+": "+err.Error())
			continue
		}
		func() {
			defer release()
			exp, err := e.Store.GetExperiment(candidate.ID)
			if err != nil || exp.State != model.ExperimentRunning {
				return
			}
			rep, err := experiment.BuildBanditReport(e.Store, exp, now)
			var state *model.BanditBinding
			if err != nil {
				state = exp.ExperimentBinding().Bandit
				if current := e.Store.CurrentFlagContents(exp.WorkspaceID, exp.EnvironmentID)[exp.FlagKey]; current != nil && current.Content.Experiment != nil && current.Content.Experiment.Bandit != nil {
					cp := *current.Content.Experiment.Bandit
					state = &cp
				}
				state.Frozen, state.FreezeReason = true, "feedback ledger unhealthy: "+err.Error()
			} else {
				state = experiment.PublishedBanditState(exp, rep.Bandit)
			}
			changed, publishErr := e.Admin.ApplyBanditState(EngineActor, exp.ID, state)
			if publishErr != nil {
				sum.Errors = append(sum.Errors, exp.ID+": "+publishErr.Error())
				return
			}
			if changed {
				sum.BanditUpdated = append(sum.BanditUpdated, exp.ID)
			}
			if state.Frozen {
				sum.BanditFrozen = append(sum.BanditFrozen, exp.ID)
			}
		}()
	}
	return sum
}

func (e *Engine) lockPlan(experimentID string) (func(), error) {
	e.mu.Lock()
	releaseDB, err := e.Store.PlanLock(experimentID)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	if err := e.Store.ReloadFromBackend(); err != nil {
		releaseDB()
		e.mu.Unlock()
		return nil, err
	}
	return func() {
		releaseDB()
		e.mu.Unlock()
	}, nil
}

// Run ticks on a fixed interval until ctx is cancelled. interval <= 0
// disables the loop (manual-only operation).
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		e.Logger.Printf("rollout: auto-advance disabled (interval %s); manual operations only", interval)
		<-ctx.Done()
		return
	}
	e.Logger.Printf("rollout: auto-advance engine started (interval %s)", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Logger.Printf("rollout: engine stopped")
			return
		case <-t.C:
			sum := e.Tick()
			if len(sum.Started)+len(sum.Advanced)+len(sum.Paused)+len(sum.BanditUpdated)+len(sum.BanditFrozen)+
				len(sum.RolledBack)+len(sum.Completed)+len(sum.Errors) > 0 {
				e.Logger.Printf("rollout: tick started=%v advanced=%v repaired=%v paused=%v rolledBack=%v completed=%v banditUpdated=%v banditFrozen=%v errors=%v",
					sum.Started, sum.Advanced, sum.Repaired, sum.Paused, sum.RolledBack, sum.Completed, sum.BanditUpdated, sum.BanditFrozen, sum.Errors)
			}
		}
	}
}

// tickPlan advances ONE plan. Errors never abort the pass; they are logged,
// counted and retried on the next tick (all actions are idempotent).
func (e *Engine) tickPlan(now time.Time, exp *platform.Experiment, sum *TickSummary) {
	plan := exp.ReleasePlan
	if plan == nil {
		return
	}
	if plan.ManualOverride {
		return // 人工接管: engine stands down completely
	}
	switch plan.State {
	case platform.ReleasePlanArmed:
		e.tickArmed(now, exp, sum)
	case platform.ReleasePlanRunning:
		e.tickRunning(now, exp, sum)
	case platform.ReleasePlanPaused:
		// Paused plans wait for an operator (§5.3: 暂停后人工恢复).
	}
}

func (e *Engine) tickArmed(now time.Time, exp *platform.Experiment, sum *TickSummary) {
	if exp.State != model.ExperimentRunning {
		return // waits for the human/scheduled start of the experiment
	}
	// Record-then-publish: mark the plan running first; if the republish
	// fails the next tick's repair path converges (idempotent).
	err := e.mutate(exp.ID, func(p *platform.ReleasePlan) error {
		p.State = platform.ReleasePlanRunning
		p.CurrentStep = 0
		started := now
		p.StepStartedAt = &started
		p.AppendHistory(platform.PlanEvent{At: now, Actor: EngineActor.Name,
			Action: "plan.start", Step: 0,
			Detail: fmt.Sprintf("experiment running; entering step %q at %.4g%%", p.Steps[0].Name, p.Steps[0].Ratio)})
		return nil
	})
	if err != nil {
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	if _, err := e.Admin.ApplyExperimentEligibility(EngineActor, exp.ID, exp.ReleasePlan.Steps[0].Ratio,
		fmt.Sprintf("release plan start: step %q", exp.ReleasePlan.Steps[0].Name)); err != nil {
		e.Logger.Printf("rollout: %s step-0 publish failed (next tick repairs): %v", exp.ID, err)
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	sum.Started = append(sum.Started, exp.ID)
}

func (e *Engine) tickRunning(now time.Time, exp *platform.Experiment, sum *TickSummary) {
	plan := exp.ReleasePlan
	if exp.State != model.ExperimentRunning {
		switch exp.State {
		case model.ExperimentPaused, model.ExperimentScheduled:
			return // operator-paused experiment: engine holds position
		default: // stopped / analyzed / archived
			e.pause(exp, now, fmt.Sprintf("experiment entered terminal-ish state %s", exp.State), sum)
		}
		return
	}
	step := plan.Steps[plan.CurrentStep]

	// Repair/convergence: the published ratio must equal the current step's
	// ratio (a crashed pass between record and publish lands here).
	changed, err := e.Admin.ApplyExperimentEligibility(EngineActor, exp.ID, step.Ratio,
		fmt.Sprintf("release plan repair: step %q ratio %.4g%%", step.Name, step.Ratio))
	if err != nil {
		e.Logger.Printf("rollout: %s repair publish failed: %v", exp.ID, err)
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	if changed {
		sum.Repaired = append(sum.Repaired, exp.ID)
		_ = e.record(exp.ID, now, "repair_publish", plan.CurrentStep,
			fmt.Sprintf("republished step %q ratio %.4g%% (converged after failed pass)", step.Name, step.Ratio))
	}

	if plan.StepStartedAt == nil {
		// Defensive: running without a step clock — stamp and wait.
		_ = e.mutate(exp.ID, func(p *platform.ReleasePlan) error {
			started := now
			p.StepStartedAt = &started
			return nil
		})
		return
	}
	dwell := now.Sub(*plan.StepStartedAt)
	if dwell < step.MinRuntime+step.ObservationWindow {
		return // 最短运行时间/观察窗未满足 — never consult the gate early (§10.3 peeking)
	}

	// Observation window elapsed: consult the pre-registered decision gate.
	report, err := experiment.BuildReport(e.Store, exp.ID, now)
	if err != nil {
		// 统计不可用 → 暂停 (§5.3).
		e.pause(exp, now, "report unavailable: "+err.Error(), sum)
		return
	}
	if !report.Healthy {
		issues := "unhealthy data"
		if len(report.HealthIssues) > 0 {
			issues = report.HealthIssues[0]
		}
		e.pause(exp, now, "data health gate: "+issues, sum)
		return
	}

	switch report.Decision.Verdict {
	case stats.VerdictRollback:
		e.rollback(exp, now, report, sum)
	case stats.VerdictHold:
		return // insufficient sample / experiment min-runtime: normal waiting
	case stats.VerdictShip, stats.VerdictContinue:
		if !step.AutoAdvance {
			return // engine holds at the step boundary; operators advance manually
		}
		if plan.CurrentStep == len(plan.Steps)-1 {
			if report.Decision.Verdict == stats.VerdictShip {
				e.complete(exp, now, report, sum)
			}
			// continue on the final step: keep running to the planned
			// sample; concluding is a human decision (§10.3).
			return
		}
		e.advance(exp, now, report, sum)
	}
}

func (e *Engine) advance(exp *platform.Experiment, now time.Time, report *experiment.Report, sum *TickSummary) {
	next := exp.ReleasePlan.CurrentStep + 1
	nextStep := exp.ReleasePlan.Steps[next]
	// Record-then-publish (idempotent convergence on crash).
	err := e.mutate(exp.ID, func(p *platform.ReleasePlan) error {
		p.CurrentStep = next
		started := now
		p.StepStartedAt = &started
		p.AppendHistory(platform.PlanEvent{At: now, Actor: EngineActor.Name,
			Action: "step.advance", Step: next,
			Detail: fmt.Sprintf("gate verdict %s → step %q at %.4g%% (%s)",
				report.Decision.Verdict, nextStep.Name, nextStep.Ratio, firstReason(report))})
		return nil
	})
	if err != nil {
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	if _, err := e.Admin.ApplyExperimentEligibility(EngineActor, exp.ID, nextStep.Ratio,
		fmt.Sprintf("release plan advance: step %q → %.4g%%", nextStep.Name, nextStep.Ratio)); err != nil {
		e.Logger.Printf("rollout: %s advance publish failed (next tick repairs): %v", exp.ID, err)
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	sum.Advanced = append(sum.Advanced, fmt.Sprintf("%s→%s", exp.ID, nextStep.Name))
}

func (e *Engine) complete(exp *platform.Experiment, now time.Time, report *experiment.Report, sum *TickSummary) {
	err := e.mutate(exp.ID, func(p *platform.ReleasePlan) error {
		p.State = platform.ReleasePlanCompleted
		p.AppendHistory(platform.PlanEvent{At: now, Actor: EngineActor.Name,
			Action: "plan.complete", Step: p.CurrentStep,
			Detail: fmt.Sprintf("ship verdict on final step %q (%s); flag stays at %.4g%% — full rollout is a human publish decision",
				p.Steps[p.CurrentStep].Name, firstReason(report), p.Steps[p.CurrentStep].Ratio)})
		return nil
	})
	if err != nil {
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	e.audit(exp, "release_plan.complete", map[string]any{"verdict": report.Decision.Verdict})
	sum.Completed = append(sum.Completed, exp.ID)
}

// rollback stops enrollment: the experiment transitions to Stopped, which
// republishes the bound flag through the normal convergence path so both
// local-evaluation SDKs and remote eval see enrollment end (§5.2/§5.3).
// The plan is marked rolled_back ONLY after the transition (including the
// republish) succeeded; any error retries the whole action next tick — the
// verdict is stable, and admin.TransitionExperiment is itself idempotent
// (state no-op + republish until the binding agrees).
func (e *Engine) rollback(exp *platform.Experiment, now time.Time, report *experiment.Report, sum *TickSummary) {
	reason := "release plan rollback: " + firstReason(report)
	if _, err := e.Admin.TransitionExperiment(EngineActor, exp.ID, model.ExperimentStopped, reason); err != nil {
		e.Logger.Printf("rollout: %s rollback transition failed (next tick retries): %v", exp.ID, err)
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	err := e.mutate(exp.ID, func(p *platform.ReleasePlan) error {
		p.State = platform.ReleasePlanRolledBack
		p.PausedReason = reason
		p.AppendHistory(platform.PlanEvent{At: now, Actor: EngineActor.Name,
			Action: "plan.rollback", Step: p.CurrentStep, Detail: reason})
		return nil
	})
	if err != nil {
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	e.audit(exp, "release_plan.rollback", map[string]any{
		"verdict": report.Decision.Verdict, "reasons": report.Decision.Reasons,
		"guardrailBreaches": report.Decision.GuardrailBreaches})
	sum.RolledBack = append(sum.RolledBack, exp.ID)
}

func (e *Engine) pause(exp *platform.Experiment, now time.Time, reason string, sum *TickSummary) {
	if exp.ReleasePlan.State == platform.ReleasePlanPaused && exp.ReleasePlan.PausedReason == reason {
		return // already paused for exactly this reason — idempotent
	}
	if err := e.mutate(exp.ID, func(p *platform.ReleasePlan) error {
		p.State = platform.ReleasePlanPaused
		p.PausedReason = reason
		p.AppendHistory(platform.PlanEvent{At: now, Actor: EngineActor.Name,
			Action: "plan.pause", Step: p.CurrentStep, Detail: reason})
		return nil
	}); err != nil {
		sum.Errors = append(sum.Errors, exp.ID+": "+err.Error())
		return
	}
	e.audit(exp, "release_plan.auto_pause", map[string]any{"reason": reason})
	sum.Paused = append(sum.Paused, exp.ID)
}

// ---------------------------------------------------------------------------
// Manual operations (admin API surface; actor-attributed and audited)
// ---------------------------------------------------------------------------

// Pause holds the plan for an operator. Allowed from armed/running.
func (e *Engine) Pause(actor admin.Actor, experimentID, reason string) (*platform.Experiment, error) {
	release, err := e.lockPlan(experimentID)
	if err != nil {
		return nil, err
	}
	defer release()
	exp, err := e.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermFlagPublish); err != nil {
		return nil, err
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: pause requires a reason", platform.ErrValidation)
	}
	now := e.now()
	out, err := e.Store.UpdateReleasePlan(experimentID, func(p *platform.ReleasePlan) error {
		if p.State != platform.ReleasePlanArmed && p.State != platform.ReleasePlanRunning {
			return fmt.Errorf("%w: plan is %s, want armed/running", platform.ErrInvalidState, p.State)
		}
		p.State = platform.ReleasePlanPaused
		p.PausedReason = reason
		p.AppendHistory(platform.PlanEvent{At: now, Actor: actor.Name,
			Action: "manual.pause", Step: p.CurrentStep, Detail: reason})
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.audit(exp, "release_plan.pause", map[string]any{"reason": reason, "by": actor.Name})
	return out, nil
}

// Resume returns a paused plan to engine control. The step clock RESTARTS:
// after an intervention the full pre-registered observation window must
// elapse on clean data before the gate may fire again (§10.3: peeking on
// contaminated data is not a valid analysis point).
func (e *Engine) Resume(actor admin.Actor, experimentID string) (*platform.Experiment, error) {
	release, err := e.lockPlan(experimentID)
	if err != nil {
		return nil, err
	}
	defer release()
	exp, err := e.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermFlagPublish); err != nil {
		return nil, err
	}
	now := e.now()
	out, err := e.Store.UpdateReleasePlan(experimentID, func(p *platform.ReleasePlan) error {
		if p.State != platform.ReleasePlanPaused {
			return fmt.Errorf("%w: plan is %s, want paused", platform.ErrInvalidState, p.State)
		}
		if p.StepStartedAt == nil {
			p.State = platform.ReleasePlanArmed // never started: back to waiting-for-running
		} else {
			p.State = platform.ReleasePlanRunning
			started := now
			p.StepStartedAt = &started // fresh observation window
		}
		p.PausedReason = ""
		p.AppendHistory(platform.PlanEvent{At: now, Actor: actor.Name,
			Action: "manual.resume", Step: p.CurrentStep, Detail: "step clock restarted"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.audit(exp, "release_plan.resume", map[string]any{"by": actor.Name})
	return out, nil
}

// SetManualOverride flips the 人工接管开关. While enabled the engine takes no
// automatic action on this plan; while disabled automatic operation resumes
// from the recorded state.
func (e *Engine) SetManualOverride(actor admin.Actor, experimentID string, enabled bool, reason string) (*platform.Experiment, error) {
	release, err := e.lockPlan(experimentID)
	if err != nil {
		return nil, err
	}
	defer release()
	exp, err := e.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermFlagPublish); err != nil {
		return nil, err
	}
	now := e.now()
	out, err := e.Store.UpdateReleasePlan(experimentID, func(p *platform.ReleasePlan) error {
		if p.ManualOverride == enabled {
			return nil // idempotent
		}
		p.ManualOverride = enabled
		action := "manual.takeover_off"
		if enabled {
			action = "manual.takeover_on"
		}
		p.AppendHistory(platform.PlanEvent{At: now, Actor: actor.Name,
			Action: action, Step: p.CurrentStep, Detail: reason})
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.audit(exp, "release_plan.takeover", map[string]any{"enabled": enabled, "reason": reason, "by": actor.Name})
	return out, nil
}

// AdvanceNow moves to the next step (or completes the plan on the final
// step) on an operator's explicit decision. Manual advance is a human
// publish decision: it is NOT gated by the report verdict, but the current
// verdict is recorded in the history entry for traceability (§9: 任何信号
// 不得绕过发布权限 — the caller needs publish permission).
func (e *Engine) AdvanceNow(actor admin.Actor, experimentID, reason string) (*platform.Experiment, error) {
	release, err := e.lockPlan(experimentID)
	if err != nil {
		return nil, err
	}
	defer release()
	exp, err := e.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermFlagPublish); err != nil {
		return nil, err
	}
	if exp.State != model.ExperimentRunning {
		return nil, fmt.Errorf("%w: experiment %s is %s, want running", platform.ErrInvalidState, experimentID, exp.State)
	}
	plan := exp.ReleasePlan
	if plan == nil || plan.State != platform.ReleasePlanRunning {
		return nil, fmt.Errorf("%w: plan must be running to advance", platform.ErrInvalidState)
	}
	verdictNote := ""
	if report, rerr := experiment.BuildReport(e.Store, experimentID, e.now()); rerr == nil {
		verdictNote = fmt.Sprintf("; gate verdict at advance: %s", report.Decision.Verdict)
	}
	now := e.now()
	final := plan.CurrentStep == len(plan.Steps)-1
	if final {
		out, err := e.Store.UpdateReleasePlan(experimentID, func(p *platform.ReleasePlan) error {
			p.State = platform.ReleasePlanCompleted
			p.AppendHistory(platform.PlanEvent{At: now, Actor: actor.Name,
				Action: "manual.complete", Step: p.CurrentStep, Detail: reason + verdictNote})
			return nil
		})
		if err != nil {
			return nil, err
		}
		e.audit(exp, "release_plan.manual_complete", map[string]any{"reason": reason, "by": actor.Name})
		return out, nil
	}
	next := plan.CurrentStep + 1
	out, err := e.Store.UpdateReleasePlan(experimentID, func(p *platform.ReleasePlan) error {
		p.CurrentStep = next
		started := now
		p.StepStartedAt = &started
		p.AppendHistory(platform.PlanEvent{At: now, Actor: actor.Name,
			Action: "manual.advance", Step: next,
			Detail: fmt.Sprintf("%s → step %q at %.4g%%%s", reason, p.Steps[next].Name, p.Steps[next].Ratio, verdictNote)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Record-then-publish: on failure the next engine tick repairs the
	// published ratio even for manually-driven plans (unless overridden).
	if _, err := e.Admin.ApplyExperimentEligibility(actor, experimentID, plan.Steps[next].Ratio,
		fmt.Sprintf("manual advance: step %q", plan.Steps[next].Name)); err != nil {
		return nil, fmt.Errorf("step recorded but republish failed (engine/operator retry converges): %w", err)
	}
	e.audit(exp, "release_plan.manual_advance", map[string]any{
		"step": next, "ratio": plan.Steps[next].Ratio, "reason": reason, "by": actor.Name})
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (e *Engine) mutate(experimentID string, fn func(*platform.ReleasePlan) error) error {
	_, err := e.Store.UpdateReleasePlan(experimentID, fn)
	return err
}

func (e *Engine) record(experimentID string, now time.Time, action string, step int, detail string) error {
	return e.mutate(experimentID, func(p *platform.ReleasePlan) error {
		p.AppendHistory(platform.PlanEvent{At: now, Actor: EngineActor.Name,
			Action: action, Step: step, Detail: detail})
		return nil
	})
}

func (e *Engine) audit(exp *platform.Experiment, action string, detail map[string]any) {
	d := ""
	if b, err := json.Marshal(detail); err == nil {
		d = string(b)
	}
	_ = e.Store.Audit(EngineActor.Name+" ("+EngineActor.TokenID+")", EngineActor.Role,
		action, exp.WorkspaceID, exp.EnvironmentID, exp.ID, d)
}

func firstReason(report *experiment.Report) string {
	if len(report.Decision.Reasons) == 0 {
		return "no reasons recorded"
	}
	return report.Decision.Reasons[0]
}
