package platform

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/saker-ai/grayling/pkg/model"
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
	if exp.AnalysisMethod == "" && (exp.PrimaryMetric.Type == "funnel" || exp.PrimaryMetric.Type == "retention") {
		exp.AnalysisMethod = "two-proportion-z"
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
	version, err := ExperimentRegistrationVersion(exp)
	if err != nil {
		return nil, err
	}
	exp.RegistrationVersion = version
	stored := exp
	if exp.HolmFamily != nil || exp.PrimaryMetric.Funnel != nil || exp.PrimaryMetric.Retention != nil || hasJourneyGuardrail(exp.Guardrails) {
		stored = cloneRegisteredExperiment(exp)
	}
	s.data.Experiments[exp.ID] = stored
	s.changes.markDirty(collExperiments, exp.ID)
	return exp, s.commitLocked()
}

// cloneRegisteredExperiment keeps the pre-registered decision contract out of
// reach of callers holding an Experiment pointer. State transitions mutate
// the store's copy; reads return another copy for Holm experiments.
func cloneRegisteredExperiment(exp *Experiment) *Experiment {
	clone := *exp
	clone.Variants = slices.Clone(exp.Variants)
	clone.Proportions = maps.Clone(exp.Proportions)
	if exp.LayerSlot != nil {
		clone.LayerSlot = new(*exp.LayerSlot)
	}
	clone.PrimaryMetric = cloneRegisteredMetric(exp.PrimaryMetric)
	clone.Guardrails = slices.Clone(exp.Guardrails)
	for i := range clone.Guardrails {
		clone.Guardrails[i].Funnel = cloneFunnel(exp.Guardrails[i].Funnel)
		clone.Guardrails[i].Retention = cloneRetention(exp.Guardrails[i].Retention)
	}
	if exp.HolmFamily != nil {
		family := *exp.HolmFamily
		family.Hypotheses = slices.Clone(family.Hypotheses)
		for i := range family.Hypotheses {
			family.Hypotheses[i].Metric = cloneRegisteredMetric(family.Hypotheses[i].Metric)
		}
		clone.HolmFamily = &family
	}
	return &clone
}

func cloneRegisteredMetric(metric MetricSpec) MetricSpec {
	metric.Funnel = cloneFunnel(metric.Funnel)
	metric.Retention = cloneRetention(metric.Retention)
	if metric.Floor != nil {
		metric.Floor = new(*metric.Floor)
	}
	if metric.Ceil != nil {
		metric.Ceil = new(*metric.Ceil)
	}
	return metric
}

func cloneFunnel(spec *FunnelSpec) *FunnelSpec {
	if spec == nil {
		return nil
	}
	clone := *spec
	clone.Steps = slices.Clone(spec.Steps)
	return &clone
}

func cloneRetention(spec *RetentionSpec) *RetentionSpec {
	if spec == nil {
		return nil
	}
	return new(*spec)
}

func hasJourneyGuardrail(guards []GuardrailSpec) bool {
	for _, g := range guards {
		if g.Funnel != nil || g.Retention != nil {
			return true
		}
	}
	return false
}

func validateJourney(typ string, funnel *FunnelSpec, retention *RetentionSpec) error {
	if typ == "funnel" {
		if funnel == nil || retention != nil || len(funnel.Steps) < 2 || len(funnel.Steps) > 32 || funnel.StepTimeout <= 0 ||
			(funnel.DedupUnit != "user" && funnel.DedupUnit != "session") {
			return fmt.Errorf("%w: funnel requires ordered steps, user|session dedup and positive stepTimeout", ErrValidation)
		}
		seen := map[string]bool{}
		for _, step := range funnel.Steps {
			if step == "" || seen[step] {
				return fmt.Errorf("%w: funnel steps must be nonempty and unique", ErrValidation)
			}
			seen[step] = true
		}
		return nil
	}
	if typ == "retention" {
		if retention == nil || funnel != nil || retention.ReturnKey == "" ||
			(retention.Day != 1 && retention.Day != 7) || retention.Timezone == "" {
			return fmt.Errorf("%w: retention requires returnKey, D1|D7 and timezone", ErrValidation)
		}
		if _, err := time.LoadLocation(retention.Timezone); err != nil {
			return fmt.Errorf("%w: invalid retention timezone: %v", ErrValidation, err)
		}
		return nil
	}
	if funnel != nil || retention != nil {
		return fmt.Errorf("%w: journey spec requires funnel or retention metric", ErrValidation)
	}
	return nil
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
	case "proportion", "mean", "funnel", "retention":
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
	if err := validateJourney(exp.PrimaryMetric.Type, exp.PrimaryMetric.Funnel, exp.PrimaryMetric.Retention); err != nil {
		return err
	}
	if exp.PrimaryMetric.Type == "funnel" || exp.PrimaryMetric.Type == "retention" {
		if exp.PrimaryMetric.NumeratorKey != "" || exp.PrimaryMetric.DenominatorKey != "" || exp.PrimaryMetric.QuantileLevel != 0 || exp.PrimaryMetric.Floor != nil || exp.PrimaryMetric.Ceil != nil {
			return fmt.Errorf("%w: journey metrics cannot use ratio, quantile or clamp parameters", ErrValidation)
		}
		if exp.AnalysisMethod != "" && exp.AnalysisMethod != "two-proportion-z" {
			return fmt.Errorf("%w: journey primary requires two-proportion-z analysis", ErrValidation)
		}
	}
	for _, g := range exp.Guardrails {
		if g.Type != "mean" && g.Type != "proportion" && g.Type != "funnel" && g.Type != "retention" {
			return fmt.Errorf("%w: unsupported guardrail type %q", ErrValidation, g.Type)
		}
		if err := validateJourney(g.Type, g.Funnel, g.Retention); err != nil {
			return err
		}
		if (g.Type == "funnel" || g.Type == "retention") && (g.MetricKey == "" || g.MinSample <= 0 || g.MaxRelativeRegression < 0) {
			return fmt.Errorf("%w: journey guardrail needs metricKey, minSample and nonnegative threshold", ErrValidation)
		}
		if (g.Type == "funnel" || g.Type == "retention") && g.Direction != "increase" && g.Direction != "decrease" {
			return fmt.Errorf("%w: journey guardrail direction must be increase|decrease", ErrValidation)
		}
	}
	if (exp.PrimaryMetric.Type == "funnel" || exp.PrimaryMetric.Type == "retention" || hasJourneyGuardrail(exp.Guardrails)) && exp.AllocationMode == "bandit" {
		return fmt.Errorf("%w: journey metrics require fixed-split allocation", ErrValidation)
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
	if math.IsNaN(exp.Alpha) || exp.Alpha <= 0 || exp.Alpha >= 1 {
		return fmt.Errorf("%w: alpha %v out of (0,1)", ErrValidation, exp.Alpha)
	}
	if exp.Layer != "" && exp.LayerSlot == nil {
		return fmt.Errorf("%w: layer %q requires a slot assignment", ErrValidation, exp.Layer)
	}
	if err := validateHolmFamily(exp); err != nil {
		return err
	}
	return nil
}

func validateHolmFamily(exp *Experiment) error {
	family := exp.HolmFamily
	if family == nil {
		return nil
	}
	if exp.AllocationMode == "bandit" || family.ID == "" || len(family.Hypotheses) < 2 {
		return fmt.Errorf("%w: Holm requires a named family with at least two fixed-sample hypotheses", ErrValidation)
	}
	if len(family.Hypotheses) > 100 {
		return fmt.Errorf("%w: Holm family exceeds 100 hypotheses", ErrValidation)
	}
	if (exp.PrimaryMetric.Type == "proportion" && exp.AnalysisMethod != "" && exp.AnalysisMethod != "two-proportion-z") ||
		(exp.PrimaryMetric.Type == "mean" && exp.AnalysisMethod != "" && exp.AnalysisMethod != "welch") {
		return fmt.Errorf("%w: Holm analysis method must match the primary mean/proportion test", ErrValidation)
	}
	seen := map[string]bool{}
	for i, h := range family.Hypotheses {
		if h.ID == "" || seen[h.ID] || h.Control == h.Treatment ||
			!slices.Contains(exp.Variants, h.Control) || !slices.Contains(exp.Variants, h.Treatment) {
			return fmt.Errorf("%w: Holm hypothesis %d has duplicate/empty ID or invalid arm pair", ErrValidation, i)
		}
		seen[h.ID] = true
		if h.Metric.Key == "" || (h.Metric.Type != "mean" && h.Metric.Type != "proportion") ||
			(h.Metric.Direction != "increase" && h.Metric.Direction != "decrease") ||
			h.Metric.CovariateKey != "" || h.Metric.NumeratorKey != "" || h.Metric.DenominatorKey != "" || h.Metric.QuantileLevel != 0 {
			return fmt.Errorf("%w: Holm hypothesis %q needs a mean or proportion metric without advanced-method parameters", ErrValidation, h.ID)
		}
		if h.Metric.Floor != nil || h.Metric.Ceil != nil {
			return fmt.Errorf("%w: Holm hypothesis %q cannot use unimplemented clamps", ErrValidation, h.ID)
		}
		for j := 0; j < i; j++ {
			prior := family.Hypotheses[j]
			if prior.Metric.Key == h.Metric.Key && prior.Metric.Type == h.Metric.Type &&
				prior.Control == h.Control && prior.Treatment == h.Treatment {
				return fmt.Errorf("%w: duplicate Holm comparison %q", ErrValidation, h.ID)
			}
		}
	}
	first := family.Hypotheses[0]
	if first.Control != exp.Variants[0] || first.Treatment != exp.Variants[1] || !reflect.DeepEqual(first.Metric, exp.PrimaryMetric) {
		return fmt.Errorf("%w: first Holm hypothesis must exactly match primaryMetric and first arm pair", ErrValidation)
	}
	if len(exp.Variants) > 2 {
		for _, variant := range exp.Variants[1:] {
			found := false
			for _, h := range family.Hypotheses {
				found = found || h.Treatment == variant
			}
			if !found {
				return fmt.Errorf("%w: variant %q has no Holm comparison", ErrValidation, variant)
			}
		}
	}
	return nil
}

// ExperimentRegistrationVersion is the SHA-256 of the decision contract. It
// is persisted in the experiment, audit create event, and each report. Any
// accidental in-memory modification of the registered family fails closed.
func ExperimentRegistrationVersion(exp *Experiment) (string, error) {
	contract := struct {
		FlagKey         string
		FlagRevision    int64
		Epoch           int64
		Unit            string
		Layer           string
		LayerSlot       *model.BucketRange
		Eligibility     float64
		Variants        []string
		Proportions     map[string]float64
		PrimaryMetric   MetricSpec
		Guardrails      []GuardrailSpec
		HolmFamily      *HolmFamily
		Alpha           float64
		Power           float64
		MDE             float64
		MinSamplePerArm int64
		MinRuntime      time.Duration
		AnalysisWindow  time.Duration
		Hypothesis      string
		AnalysisMethod  string
	}{
		FlagKey: exp.FlagKey, FlagRevision: exp.FlagRevision, Epoch: exp.Epoch,
		Unit: exp.Unit, Layer: exp.Layer, LayerSlot: exp.LayerSlot, Eligibility: exp.Eligibility,
		Variants: exp.Variants, Proportions: exp.Proportions, PrimaryMetric: exp.PrimaryMetric,
		Guardrails: exp.Guardrails, HolmFamily: exp.HolmFamily, Alpha: exp.Alpha,
		Power: exp.Power, MDE: exp.MDE, MinSamplePerArm: exp.MinSamplePerArm,
		MinRuntime: exp.MinRuntime, AnalysisWindow: exp.AnalysisWindow,
		Hypothesis: exp.Hypothesis, AnalysisMethod: exp.AnalysisMethod,
	}
	b, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("%w: serialize registration: %v", ErrValidation, err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b)), nil
}

// GetExperiment returns one experiment.
func (s *Store) GetExperiment(id string) (*Experiment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data.Experiments[id]
	if !ok {
		return nil, fmt.Errorf("%w: experiment %q", ErrNotFound, id)
	}
	if e.HolmFamily != nil || e.PrimaryMetric.Funnel != nil || e.PrimaryMetric.Retention != nil || hasJourneyGuardrail(e.Guardrails) {
		return cloneRegisteredExperiment(e), nil
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
			if e.HolmFamily != nil || e.PrimaryMetric.Funnel != nil || e.PrimaryMetric.Retention != nil || hasJourneyGuardrail(e.Guardrails) {
				out = append(out, cloneRegisteredExperiment(e))
			} else {
				out = append(out, e)
			}
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
		if e.HolmFamily != nil || e.PrimaryMetric.Funnel != nil || e.PrimaryMetric.Retention != nil || hasJourneyGuardrail(e.Guardrails) {
			return cloneRegisteredExperiment(e), nil
		}
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
	err := s.commitLocked()
	if e.HolmFamily != nil || e.PrimaryMetric.Funnel != nil || e.PrimaryMetric.Retention != nil || hasJourneyGuardrail(e.Guardrails) {
		return cloneRegisteredExperiment(e), err
	}
	return e, err
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
