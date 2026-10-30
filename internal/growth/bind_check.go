package growth

import (
	"context"
	"time"
)

// BindingPrerequisite contains only a bounded code and implementation-owned advice.
type BindingPrerequisite struct {
	Code        string `json:"code"`
	Ready       bool   `json:"ready"`
	Remediation string `json:"remediation"`
}

// BindingObservation is a non-atomic observation, never binding or publication authority.
type BindingObservation struct {
	RolloutManifest
	SourcePath            string                `json:"sourcePath"`
	ExperimentSourcePath  string                `json:"experimentSourcePath"`
	ExperimentUpdatedAt   *time.Time            `json:"experimentUpdatedAt,omitempty"`
	RegisteredRevision    int64                 `json:"registeredRevision"`
	CurrentRevision       int64                 `json:"currentRevision"`
	Ready                 bool                  `json:"ready"`
	Checks                []BindingPrerequisite `json:"checks"`
	Blockers              []BindingPrerequisite `json:"blockers"`
	NonAtomic             bool                  `json:"nonAtomic"`
	PublicationAuthorized bool                  `json:"publicationAuthorized"`
}

var bindingPrerequisites = []BindingPrerequisite{
	{Code: "state", Remediation: "Have an authorized operator review or schedule the experiment."},
	{Code: "published_flag", Remediation: "Have an authorized operator publish the required flag."},
	{Code: "draft_clear", Remediation: "Finish or discard the changed flag draft through the existing workflow."},
	{Code: "registered_revision", Remediation: "Review the changed flag and register the experiment against the intended revision."},
	{Code: "fallback", Remediation: "Configure a deterministic default variation through the existing flag workflow."},
	{Code: "unit", Remediation: "Align flag bucketing with the registered randomization unit."},
	{Code: "split", Remediation: "Configure a percentage rule matching the registered proportions."},
	{Code: "bandit_rule", Remediation: "For bandit allocation, configure one percentage rule matching its fallback."},
	{Code: "existing_binding", Remediation: "Resolve the existing experiment binding through the authorized workflow."},
}

type bindingSnapshot struct {
	ID                  string    `json:"id"`
	WorkspaceID         string    `json:"workspaceId"`
	EnvironmentID       string    `json:"environmentId"`
	Epoch               int64     `json:"epoch"`
	RegistrationVersion string    `json:"registrationVersion"`
	FlagRevision        *int64    `json:"flagRevision"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

func (s *bindingSnapshot) matches(m RolloutManifest) bool {
	return s.ID == m.ExperimentID && s.WorkspaceID == m.WorkspaceID && s.EnvironmentID == m.EnvironmentID && s.Epoch == m.Epoch && s.RegistrationVersion == m.RegistrationVersion
}

// BindCheck uses GET only. Surrounding experiment reads detect observed changes,
// but cannot provide an atomic snapshot or authorize a later bind/publication.
func (r *Reader) BindCheck(ctx context.Context, m RolloutManifest) (*BindingObservation, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	if m.WorkspaceID != r.workspace || m.EnvironmentID != r.environment {
		return nil, ErrMismatch
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := "/api/v1/admin/experiments/" + m.ExperimentID
	o := &BindingObservation{RolloutManifest: m, SourcePath: path + "/bind-check", ExperimentSourcePath: path, NonAtomic: true, Checks: []BindingPrerequisite{}, Blockers: []BindingPrerequisite{}}
	missingObservation := func(code string) (*BindingObservation, error) {
		o.Ready = false
		o.Blockers = []BindingPrerequisite{{Code: code, Remediation: "Restore access to the pinned evidence and repeat this read-only observation."}}
		return o, nil
	}
	readSnapshot := func() (*bindingSnapshot, bool, error) {
		var s *bindingSnapshot
		missing, err := r.get(ctx, path, &s)
		if err != nil || missing {
			return nil, missing, err
		}
		if s == nil {
			return nil, false, ErrResponse
		}
		if !s.matches(m) {
			return nil, false, ErrMismatch
		}
		if s.FlagRevision == nil || *s.FlagRevision < 0 || !rolloutTime(s.UpdatedAt) {
			return nil, false, ErrResponse
		}
		return s, false, nil
	}
	before, missing, err := readSnapshot()
	if err != nil {
		return nil, err
	}
	if missing {
		return missingObservation("experiment_missing")
	}
	var wire *struct {
		ExperimentID       string `json:"experimentId"`
		RegisteredRevision *int64 `json:"registeredRevision"`
		CurrentRevision    *int64 `json:"currentRevision"`
		Ready              *bool  `json:"ready"`
		Checks             []struct {
			Code  string `json:"code"`
			Ready *bool  `json:"ready"`
		} `json:"checks"`
	}
	missing, err = r.get(ctx, o.SourcePath, &wire)
	if err != nil {
		return nil, err
	}
	checkMissing := missing
	after, missing, err := readSnapshot()
	if err != nil {
		return nil, err
	}
	if missing {
		return missingObservation("experiment_missing")
	}
	if *before.FlagRevision != *after.FlagRevision || !before.UpdatedAt.Equal(after.UpdatedAt) {
		return nil, ErrMismatch
	}
	o.ExperimentUpdatedAt = &before.UpdatedAt
	o.RegisteredRevision = *before.FlagRevision
	if checkMissing {
		return missingObservation("bind_check_missing")
	}
	if wire == nil || wire.RegisteredRevision == nil || wire.CurrentRevision == nil || wire.Ready == nil || *wire.RegisteredRevision < 0 || *wire.CurrentRevision < 0 || len(wire.Checks) != len(bindingPrerequisites) {
		return nil, ErrResponse
	}
	if wire.ExperimentID != m.ExperimentID || *wire.RegisteredRevision != *before.FlagRevision {
		return nil, ErrMismatch
	}
	checks := make(map[string]bool, len(wire.Checks))
	for _, c := range wire.Checks {
		if c.Ready == nil {
			return nil, ErrResponse
		}
		if _, exists := checks[c.Code]; exists {
			return nil, ErrResponse
		}
		checks[c.Code] = *c.Ready
	}
	ready := true
	for _, p := range bindingPrerequisites {
		value, exists := checks[p.Code]
		if !exists {
			return nil, ErrResponse
		}
		p.Ready = value
		ready = ready && value
		o.Checks = append(o.Checks, p)
		if !value {
			o.Blockers = append(o.Blockers, p)
		}
	}
	if ready != *wire.Ready || checks["registered_revision"] != (*wire.CurrentRevision > 0 && *wire.CurrentRevision == *wire.RegisteredRevision) {
		return nil, ErrResponse
	}
	o.Ready = ready
	o.CurrentRevision = *wire.CurrentRevision
	return o, nil
}
