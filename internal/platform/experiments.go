package platform

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/godeps/golab/pkg/model"
)

// validExperimentTransitions is the state machine of §5.3:
// Draft → Reviewed → Scheduled/Running → Paused/Stopped → Analyzed → Archived.
// Paused may return to Running; Scheduled/Running may go to Stopped;
// Analyzed is reachable from Running/Paused/Stopped.
var validExperimentTransitions = map[model.ExperimentState][]model.ExperimentState{
	model.ExperimentDraft:     {model.ExperimentReviewed},
	model.ExperimentReviewed:  {model.ExperimentScheduled, model.ExperimentRunning, model.ExperimentDraft},
	model.ExperimentScheduled: {model.ExperimentRunning, model.ExperimentPaused, model.ExperimentStopped},
	model.ExperimentRunning:   {model.ExperimentPaused, model.ExperimentStopped, model.ExperimentAnalyzed},
	model.ExperimentPaused:    {model.ExperimentRunning, model.ExperimentStopped, model.ExperimentAnalyzed},
	model.ExperimentStopped:   {model.ExperimentAnalyzed, model.ExperimentArchived},
	model.ExperimentAnalyzed:  {model.ExperimentArchived},
	model.ExperimentArchived:  {},
}

// ExperimentTransitionAllowed reports whether the state machine permits the
// move (exported for the admin API to produce helpful errors).
func ExperimentTransitionAllowed(from, to model.ExperimentState) bool {
	for _, allowed := range validExperimentTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// CreateExperiment registers a new experiment in Draft. The caller (admin
// service) has already validated metric specs and proportions; the store
// re-checks invariants that protect analysis integrity.
func (s *Store) CreateExperiment(exp *Experiment) (*Experiment, error) {
	if exp == nil {
		return nil, fmt.Errorf("%w: nil experiment", ErrValidation)
	}
	if exp.AllocationMode == "" {
		exp.AllocationMode = "fixed-split"
	}
	if err := validateExperimentInvariants(exp); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkEnvLocked(exp.WorkspaceID, exp.EnvironmentID); err != nil {
		return nil, err
	}
	q := s.quotasLocked(exp.WorkspaceID)
	if len(s.data.Experiments) >= q.MaxExperiments {
		return nil, fmt.Errorf("%w: max %d experiments", ErrQuotaExceeded, q.MaxExperiments)
	}
	// One live (non-archived) experiment per (flag, epoch); epochs are unique
	// per flag and never reused (§6: 修改随机化单元/比例/绑定必须建新 epoch).
	for _, e := range s.data.Experiments {
		if e.FlagKey == exp.FlagKey && e.WorkspaceID == exp.WorkspaceID &&
			e.EnvironmentID == exp.EnvironmentID && e.Epoch == exp.Epoch {
			return nil, fmt.Errorf("%w: epoch %d of flag %q already registered",
				ErrExists, exp.Epoch, exp.FlagKey)
		}
	}
	exp.ID = s.nextID("exp")
	exp.State = model.ExperimentDraft
	now := s.now()
	exp.CreatedAt, exp.UpdatedAt = now, now
	if exp.Alpha <= 0 {
		exp.Alpha = 0.05
	}
	if exp.Power <= 0 {
		exp.Power = 0.8
	}
	s.data.Experiments[exp.ID] = exp
	s.changes.markDirty(collExperiments, exp.ID)
	return exp, s.commitLocked()
}

func validateExperimentInvariants(exp *Experiment) error {
	if exp.AllocationMode != "" && exp.AllocationMode != "fixed-split" && exp.AllocationMode != "bandit" {
		return fmt.Errorf("%w: unknown allocation mode %q", ErrValidation, exp.AllocationMode)
	}
	if exp.AllocationMode == "bandit" {
		if exp.Bandit == nil || exp.Bandit.PolicyVersion == "" ||
			(exp.Bandit.Strategy != "thompson" && exp.Bandit.Strategy != "epsilon-greedy") ||
			exp.Bandit.PriorAlpha <= 0 || math.IsNaN(exp.Bandit.PriorAlpha) || math.IsInf(exp.Bandit.PriorAlpha, 0) ||
			exp.Bandit.PriorBeta <= 0 || math.IsNaN(exp.Bandit.PriorBeta) || math.IsInf(exp.Bandit.PriorBeta, 0) ||
			exp.Bandit.Epsilon < 0 || exp.Bandit.Epsilon >= 1 || math.IsNaN(exp.Bandit.Epsilon) ||
			exp.Bandit.FeedbackDelay < exp.AnalysisWindow+72*time.Hour {
			return fmt.Errorf("%w: bandit needs policy version, strategy, positive priors and feedback delay", ErrValidation)
		}
		if exp.PrimaryMetric.Type != "proportion" || exp.AnalysisMethod != "bandit-beta-bernoulli" {
			return fmt.Errorf("%w: bandit requires proportion metric and bandit-beta-bernoulli analysis", ErrValidation)
		}
		if len(exp.Variants) != 2 {
			return fmt.Errorf("%w: bandit v1 requires exactly two variants", ErrValidation)
		}
	} else if exp.Bandit != nil || exp.AnalysisMethod == "bandit-beta-bernoulli" {
		return fmt.Errorf("%w: bandit spec requires bandit allocation mode", ErrValidation)
	}
	if exp.FlagKey == "" {
		return fmt.Errorf("%w: experiment needs flagKey", ErrValidation)
	}
	if exp.Epoch < 0 {
		return fmt.Errorf("%w: negative epoch", ErrValidation)
	}
	if exp.Unit == "" {
		return fmt.Errorf("%w: randomization unit required", ErrValidation)
	}
	if len(exp.Variants) < 2 {
		return fmt.Errorf("%w: need at least 2 variants", ErrValidation)
	}
	seen := map[string]bool{}
	for _, v := range exp.Variants {
		if seen[v] {
			return fmt.Errorf("%w: duplicate variant %q", ErrValidation, v)
		}
		seen[v] = true
		p, ok := exp.Proportions[v]
		if !ok || p <= 0 || p > 100 {
			return fmt.Errorf("%w: variant %q needs proportion in (0,100]", ErrValidation, v)
		}
	}
	sum := 0.0
	for _, p := range exp.Proportions {
		sum += p
	}
	if sum < 100-1e-9 || sum > 100+1e-9 {
		return fmt.Errorf("%w: proportions sum to %v, want 100", ErrValidation, sum)
	}
	switch exp.PrimaryMetric.Type {
	case "proportion", "mean":
	case "ratio":
		// Ratio needs a numerator and a denominator metric key (§10.1). Key may
		// serve as the numerator, but the denominator must be explicit.
		if exp.PrimaryMetric.DenominatorKey == "" {
			return fmt.Errorf("%w: ratio metric requires denominatorKey", ErrValidation)
		}
		if exp.PrimaryMetric.NumeratorKey == "" && exp.PrimaryMetric.Key == "" {
			return fmt.Errorf("%w: ratio metric requires numeratorKey (or key as numerator)", ErrValidation)
		}
	case "quantile":
		// Quantile needs a level in (0,1) taken over per-unit detail (§10.1).
		lvl := exp.PrimaryMetric.QuantileLevel
		if lvl <= 0 || lvl >= 1 {
			return fmt.Errorf("%w: quantile metric requires quantileLevel in (0,1), got %v", ErrValidation, lvl)
		}
	default:
		return fmt.Errorf("%w: primary metric type must be proportion|mean|ratio|quantile, got %q",
			ErrValidation, exp.PrimaryMetric.Type)
	}
	if exp.PrimaryMetric.Key == "" && exp.PrimaryMetric.NumeratorKey == "" {
		return fmt.Errorf("%w: primary metric key required", ErrValidation)
	}
	if exp.PrimaryMetric.CovariateKey != "" && exp.PrimaryMetric.Type != "mean" {
		return fmt.Errorf("%w: covariateKey (CUPED) applies only to mean metrics", ErrValidation)
	}
	switch exp.PrimaryMetric.Direction {
	case "increase", "decrease":
	default:
		return fmt.Errorf("%w: metric direction must be increase|decrease", ErrValidation)
	}
	switch exp.AnalysisMethod {
	case "", "welch", "two-proportion-z", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli":
	default:
		return fmt.Errorf("%w: unknown pre-registered analysis method %q", ErrValidation, exp.AnalysisMethod)
	}
	if exp.Alpha <= 0 || exp.Alpha >= 1 {
		return fmt.Errorf("%w: alpha %v out of (0,1)", ErrValidation, exp.Alpha)
	}
	if exp.Layer != "" && exp.LayerSlot == nil {
		return fmt.Errorf("%w: layer %q requires a slot assignment", ErrValidation, exp.Layer)
	}
	return nil
}

// GetExperiment returns one experiment.
func (s *Store) GetExperiment(id string) (*Experiment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data.Experiments[id]
	if !ok {
		return nil, fmt.Errorf("%w: experiment %q", ErrNotFound, id)
	}
	return e, nil
}

// ListExperiments returns experiments of one environment (all when
// environmentID is empty), newest first.
func (s *Store) ListExperiments(workspaceID, environmentID string) []*Experiment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Experiment{}
	for _, e := range s.data.Experiments {
		if e.WorkspaceID == workspaceID && (environmentID == "" || e.EnvironmentID == environmentID) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// TransitionExperiment moves the state machine forward, stamping start/stop
// times. Illegal moves return ErrInvalidState.
func (s *Store) TransitionExperiment(id string, to model.ExperimentState, actor string) (*Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Experiments[id]
	if !ok {
		return nil, fmt.Errorf("%w: experiment %q", ErrNotFound, id)
	}
	if e.State == to {
		return e, nil // idempotent
	}
	if !ExperimentTransitionAllowed(e.State, to) {
		return nil, fmt.Errorf("%w: %s → %s", ErrInvalidState, e.State, to)
	}
	now := s.now()
	switch to {
	case model.ExperimentRunning:
		if e.StartedAt == nil {
			e.StartedAt = &now
		}
	case model.ExperimentStopped, model.ExperimentAnalyzed:
		if e.StoppedAt == nil {
			e.StoppedAt = &now
		}
	}
	e.State = to
	e.UpdatedAt = now
	s.changes.markDirty(collExperiments, id)
	return e, s.commitLocked()
}

// NextEpoch returns the next unused epoch for one flag (epochs are never
// reused, so this is max(existing)+1).
func (s *Store) NextEpoch(workspaceID, environmentID, flagKey string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var max int64
	found := false
	for _, e := range s.data.Experiments {
		if e.WorkspaceID == workspaceID && e.EnvironmentID == environmentID && e.FlagKey == flagKey {
			if !found || e.Epoch > max {
				max, found = e.Epoch, true
			}
		}
	}
	if !found {
		return 1
	}
	return max + 1
}

// AssignLayerSlot finds a free bucket range of the requested width inside a
// layer, scanning existing experiments of the same (workspace, environment,
// layer). Capacity is reserved at registration: every non-archived
// experiment's slot counts as used (abandoned drafts must be archived to
// release capacity — a deliberate trade that prevents double-booking
// between draft and schedule). Returns nil when the layer is full — the
// caller must reject instead of overlapping (§6: 互斥层划分不重叠桶区间).
func (s *Store) AssignLayerSlot(workspaceID, environmentID, layer string, widthBuckets int) *model.BucketRange {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if widthBuckets <= 0 || widthBuckets > 10000 {
		return nil
	}
	type iv struct{ start, end int }
	var used []iv
	for _, e := range s.data.Experiments {
		if e.WorkspaceID != workspaceID || e.EnvironmentID != environmentID || e.Layer != layer {
			continue
		}
		if e.State == model.ExperimentArchived {
			continue
		}
		if e.LayerSlot != nil {
			used = append(used, iv{e.LayerSlot.Start, e.LayerSlot.End})
		}
	}
	sort.Slice(used, func(i, j int) bool { return used[i].start < used[j].start })
	pos := 0
	for _, u := range used {
		if u.start-pos >= widthBuckets {
			return &model.BucketRange{Start: pos, End: pos + widthBuckets}
		}
		if u.end > pos {
			pos = u.end
		}
	}
	if 10000-pos >= widthBuckets {
		return &model.BucketRange{Start: pos, End: pos + widthBuckets}
	}
	return nil
}

// SetExperimentLayerSlot records the server-assigned slot (called when an
// experiment enters Scheduled with a layer).
func (s *Store) SetExperimentLayerSlot(id string, slot *model.BucketRange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Experiments[id]
	if !ok {
		return fmt.Errorf("%w: experiment %q", ErrNotFound, id)
	}
	e.LayerSlot = slot
	e.UpdatedAt = s.now()
	s.changes.markDirty(collExperiments, id)
	return s.commitLocked()
}

// LiveExperiments returns experiments currently affecting evaluation
// (scheduled/running/paused) — used by reconciliation and reporting guards.
func (s *Store) LiveExperiments(workspaceID, environmentID string) []*Experiment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Experiment{}
	for _, e := range s.data.Experiments {
		if e.WorkspaceID == workspaceID && e.EnvironmentID == environmentID {
			switch e.State {
			case model.ExperimentScheduled, model.ExperimentRunning, model.ExperimentPaused:
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ExperimentStartedAt exposes the running-since timestamp for min-runtime
// checks (§10.3: 预先给定最短运行时间).
func (s *Store) ExperimentStartedAt(id string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data.Experiments[id]
	if !ok || e.StartedAt == nil {
		return time.Time{}, false
	}
	return *e.StartedAt, true
}

// SetReleasePlan attaches a pre-registered staged-rollout plan to an
// experiment (single transactional write; both persistence backends carry
// the plan inside the experiment document).
func (s *Store) SetReleasePlan(experimentID string, plan *ReleasePlan) (*Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Experiments[experimentID]
	if !ok {
		return nil, fmt.Errorf("%w: experiment %q", ErrNotFound, experimentID)
	}
	e.ReleasePlan = plan
	e.UpdatedAt = s.now()
	s.changes.markDirty(collExperiments, experimentID)
	return e, s.commitLocked()
}

// UpdateReleasePlan mutates the experiment's release plan inside one
// transaction. Copy-on-write: the mutation is applied to a deep copy which
// is then swapped in atomically, so a concurrent reader holding the
// previous plan pointer (e.g. an API handler marshaling a GET response
// while the rollout engine ticks) always observes a consistent snapshot —
// plans are never mutated in place. A nil plan is ErrNotFound (plans are
// pre-registered before the engine can act).
func (s *Store) UpdateReleasePlan(experimentID string, mutate func(*ReleasePlan) error) (*Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Experiments[experimentID]
	if !ok {
		return nil, fmt.Errorf("%w: experiment %q", ErrNotFound, experimentID)
	}
	if e.ReleasePlan == nil {
		return nil, fmt.Errorf("%w: experiment %q has no release plan", ErrNotFound, experimentID)
	}
	clone, err := cloneReleasePlan(e.ReleasePlan)
	if err != nil {
		return nil, err
	}
	if err := mutate(clone); err != nil {
		return nil, err
	}
	clone.UpdatedAt = s.now()
	e.ReleasePlan = clone
	e.UpdatedAt = clone.UpdatedAt
	s.changes.markDirty(collExperiments, experimentID)
	return e, s.commitLocked()
}

// cloneReleasePlan deep-copies a plan via JSON round-trip (the plan is
// JSON-serializable by construction — it is persisted as part of the
// experiment document).
func cloneReleasePlan(p *ReleasePlan) (*ReleasePlan, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out ReleasePlan
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExperimentsWithActivePlans returns every experiment whose release plan can
// still receive engine attention (armed/running/paused), oldest ID first for
// deterministic engine passes. Terminal plans (completed/rolled_back) and
// plan-less experiments are excluded.
func (s *Store) ExperimentsWithActivePlans() []*Experiment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Experiment{}
	for _, e := range s.data.Experiments {
		if e.ReleasePlan == nil {
			continue
		}
		switch e.ReleasePlan.State {
		case ReleasePlanArmed, ReleasePlanRunning, ReleasePlanPaused:
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
