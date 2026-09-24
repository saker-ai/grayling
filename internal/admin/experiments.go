package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/bucketing"
	"github.com/godeps/golab/pkg/model"
)

// CreateExperiment registers the pre-registered analysis contract (§5.3).
// Epochs are allocated by the store and never reused; changing unit,
// proportions, metrics or bindings later REQUIRES a new experiment record
// with a new epoch (§6) — this API has no update path on purpose.
func (s *Service) CreateExperiment(actor Actor, workspaceID, environmentID string,
	exp *platform.Experiment) (*platform.Experiment, error) {

	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermExperimentWrite)
	if err != nil {
		return nil, err
	}
	exp.WorkspaceID, exp.EnvironmentID = env.WorkspaceID, env.ID

	// The flag must exist and be published: experiments bind to a concrete
	// flag revision, not to a draft.
	current := s.Store.CurrentFlagContents(env.WorkspaceID, env.ID)[exp.FlagKey]
	if current == nil {
		return nil, fmt.Errorf("%w: flag %q has no published revision", platform.ErrValidation, exp.FlagKey)
	}
	exp.FlagRevision = current.Revision

	// Variants must be flag variations; proportions must reference exactly
	// the registered variants.
	for _, v := range exp.Variants {
		if _, ok := current.Content.Variations[v]; !ok {
			return nil, fmt.Errorf("%w: variant %q is not a variation of flag %q",
				platform.ErrValidation, v, exp.FlagKey)
		}
	}
	if exp.Epoch == 0 {
		exp.Epoch = s.Store.NextEpoch(env.WorkspaceID, env.ID, exp.FlagKey)
	}
	if exp.AnalysisMethod == "" {
		switch exp.PrimaryMetric.Type {
		case "mean":
			exp.AnalysisMethod = "welch"
		case "proportion":
			exp.AnalysisMethod = "two-proportion-z"
		}
	}
	if exp.AnalysisWindow <= 0 {
		return nil, fmt.Errorf("%w: analysisWindow must be pre-registered (> 0)", platform.ErrValidation)
	}
	// Layer slot assignment happens at registration so the binding is fixed
	// before any unit is exposed. Slot width = eligibility share; inside a
	// layer the slot itself is the eligibility mechanism (§6).
	if exp.Layer != "" {
		elig := exp.Eligibility
		if elig <= 0 {
			elig = 100
		}
		width := int(math.Floor(elig*float64(bucketing.Buckets)/100 + 0.5))
		slot := s.Store.AssignLayerSlot(env.WorkspaceID, env.ID, exp.Layer, width)
		if slot == nil {
			return nil, fmt.Errorf("%w: layer %q has no free slot of width %d buckets",
				platform.ErrQuotaExceeded, exp.Layer, width)
		}
		exp.LayerSlot = slot
	}
	created, err := s.Store.CreateExperiment(exp)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "experiment.create", env.WorkspaceID, env.ID, created.ID, map[string]any{
		"flag": created.FlagKey, "epoch": created.Epoch, "layer": created.Layer,
	})
	return created, nil
}

// ReviewExperiment moves Draft → Reviewed (§5.3 state machine).
func (s *Service) ReviewExperiment(actor Actor, experimentID string, note string) (*platform.Experiment, error) {
	exp, err := s.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := Authorize(actor, exp.WorkspaceID, PermExperimentWrite); err != nil {
		return nil, err
	}
	out, err := s.Store.TransitionExperiment(experimentID, model.ExperimentReviewed, actor.Name)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "experiment.review", exp.WorkspaceID, exp.EnvironmentID, experimentID,
		map[string]any{"note": note})
	return out, nil
}

// BindExperimentFlag injects the experiment binding into the flag DSL as a
// new draft and fast-tracks it through submit+approve so the publisher can
// publish it. The flag must already contain a percentage rule matching the
// registered proportions and a deterministic defaultRule.variation (§6:
// non-enrolled fallback). GoLab does not silently rewrite human-authored
// rules; it validates the structure and only adds the binding block.
func (s *Service) BindExperimentFlag(actor Actor, experimentID string) (*platform.FlagDraft, error) {
	exp, err := s.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := Authorize(actor, exp.WorkspaceID, PermFlagWrite); err != nil {
		return nil, err
	}
	if exp.State != model.ExperimentReviewed && exp.State != model.ExperimentScheduled {
		return nil, fmt.Errorf("%w: experiment %s is %s, want reviewed/scheduled",
			platform.ErrInvalidState, experimentID, exp.State)
	}
	current := s.Store.CurrentFlagContents(exp.WorkspaceID, exp.EnvironmentID)[exp.FlagKey]
	if current == nil {
		return nil, fmt.Errorf("%w: flag %q not published", platform.ErrNotFound, exp.FlagKey)
	}

	content := *current.Content
	// Deep-copy rules so we never mutate the immutable revision in place.
	if err := copyFlagRules(&content, current.Content); err != nil {
		return nil, err
	}

	// Structural validation against the registered contract.
	if content.DefaultRule == nil || content.DefaultRule.Variation == "" {
		return nil, fmt.Errorf("%w: flag needs deterministic defaultRule.variation for experiment binding",
			platform.ErrValidation)
	}
	if content.Bucketing == nil || content.Bucketing.Unit != exp.Unit {
		return nil, fmt.Errorf("%w: flag bucketing unit %q != experiment unit %q",
			platform.ErrValidation, unitOf(content.Bucketing), exp.Unit)
	}
	if !hasMatchingSplit(&content, exp.Proportions) {
		return nil, fmt.Errorf("%w: flag has no percentage rule matching registered proportions %v",
			platform.ErrValidation, exp.Proportions)
	}

	binding := exp.ExperimentBinding()
	if exp.Layer != "" && exp.LayerSlot != nil {
		// Inside a layer the slot is the eligibility mechanism.
		binding.Eligibility = 100
	}
	// Transition to Scheduled BEFORE stamping the binding so the published
	// snapshot reflects the state the experiment is actually in.
	if _, err := s.Store.TransitionExperiment(experimentID, model.ExperimentScheduled, actor.Name); err != nil {
		return nil, err
	}
	binding.State = model.ExperimentScheduled
	content.Experiment = binding
	content.Version = "" // set at publish time

	draft, err := s.Store.SaveFlagDraft(exp.WorkspaceID, exp.EnvironmentID, &content,
		platform.RiskHigh, actor.Name, current.Revision)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SubmitFlagDraft(exp.WorkspaceID, exp.EnvironmentID, exp.FlagKey, actor.Name); err != nil {
		return nil, err
	}
	s.audit(actor, "experiment.bind", exp.WorkspaceID, exp.EnvironmentID, experimentID,
		map[string]any{"flag": exp.FlagKey, "epoch": exp.Epoch, "risk": platform.RiskHigh})
	return draft, nil
}

func unitOf(b *model.Bucketing) string {
	if b == nil {
		return ""
	}
	return b.Unit
}

// copyFlagRules deep-copies the mutable parts of a flag via JSON round-trip
// (the DSL is JSON-serializable by construction).
func copyFlagRules(dst *model.Flag, src *model.Flag) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

func hasMatchingSplit(f *model.Flag, proportions map[string]float64) bool {
	matches := func(pct map[string]float64) bool {
		if len(pct) != len(proportions) {
			return false
		}
		for k, v := range proportions {
			got, ok := pct[k]
			if !ok || math.Abs(got-v) > 1e-9 {
				return false
			}
		}
		return true
	}
	for i := range f.Targeting {
		if matches(f.Targeting[i].Percentage) {
			return true
		}
	}
	return matches(f.DefaultRule.Percentage)
}

// TransitionExperiment moves the experiment state machine and republishes
// the bound flag so locally-evaluating SDKs observe the new state through
// the normal sync path (§5.2: state lives in the published snapshot).
// Requires publish permission because it changes what users get.
func (s *Service) TransitionExperiment(actor Actor, experimentID string,
	to model.ExperimentState, reason string) (*platform.Experiment, error) {

	exp, err := s.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := Authorize(actor, exp.WorkspaceID, PermFlagPublish); err != nil {
		return nil, err
	}
	updated, err := s.Store.TransitionExperiment(experimentID, to, actor.Name)
	if err != nil {
		return nil, err
	}
	// Republish the bound flag whenever the published binding disagrees
	// with the target state. Comparing against the binding (not the
	// transition delta) makes retries converge: if a previous republish
	// failed, re-running with the same target state repairs the snapshot
	// (§3: 发布状态记录目标 revision 并靠重试/对账收敛).
	current := s.Store.CurrentFlagContents(exp.WorkspaceID, exp.EnvironmentID)[exp.FlagKey]
	if current != nil && current.Content.Experiment != nil &&
		current.Content.Experiment.ID == experimentID &&
		current.Content.Experiment.State != to {

		env, err := s.Store.GetEnvironment(exp.WorkspaceID, exp.EnvironmentID)
		if err != nil {
			return nil, err
		}
		content := model.Flag{}
		if err := copyFlagRules(&content, current.Content); err != nil {
			return nil, err
		}
		content.Experiment.State = to
		content.Version = ""
		if _, _, err := s.publishRevision(actor, env, &content,
			fmt.Sprintf("experiment %s → %s: %s", experimentID, to, reason)); err != nil {
			return nil, fmt.Errorf("state recorded but flag republish failed (retry converges): %w", err)
		}
	}
	s.audit(actor, "experiment.transition", exp.WorkspaceID, exp.EnvironmentID, experimentID,
		map[string]any{"to": to, "reason": reason})
	return updated, nil
}

// ListExperiments returns experiments visible to the actor.
func (s *Service) ListExperiments(actor Actor, workspaceID, environmentID string) ([]*platform.Experiment, error) {
	ws, err := s.Store.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if err := Authorize(actor, ws.ID, PermFlagRead); err != nil {
		return nil, err
	}
	envID := ""
	if environmentID != "" {
		env, err := s.Store.GetEnvironment(ws.ID, environmentID)
		if err != nil {
			return nil, err
		}
		envID = env.ID
	}
	return s.Store.ListExperiments(ws.ID, envID), nil
}

// GetExperiment returns one experiment after authorization.
func (s *Service) GetExperiment(actor Actor, experimentID string) (*platform.Experiment, error) {
	exp, err := s.Store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if err := Authorize(actor, exp.WorkspaceID, PermFlagRead); err != nil {
		return nil, err
	}
	return exp, nil
}

// SortedVariants returns the explicit stable variant order of an experiment
// (used by report rendering and SRM expectation building).
func SortedVariants(exp *platform.Experiment) []string {
	out := make([]string, len(exp.Variants))
	copy(out, exp.Variants)
	sort.Strings(out)
	return out
}
