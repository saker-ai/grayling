package growth

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"math"
	"time"
)

const MaxRolloutManifestBytes = 16 << 10

// These observation bounds mirror admin.MaxReleaseSteps and platform.PlanHistoryCap.
const MaxRolloutSteps = 32
const MaxRolloutHistory = 64

// RolloutManifest pins an operator-owned experiment identity. It carries no authority.
type RolloutManifest struct {
	WorkspaceID         string `json:"workspaceId"`
	EnvironmentID       string `json:"environmentId"`
	ExperimentID        string `json:"experimentId"`
	Epoch               int64  `json:"epoch"`
	RegistrationVersion string `json:"registrationVersion"`
}

func (m RolloutManifest) validate() error {
	if !validSegment(m.WorkspaceID) || !validSegment(m.EnvironmentID) || !validSegment(m.ExperimentID) || m.Epoch < 1 || !fingerprint.MatchString(m.RegistrationVersion) {
		return ErrConfig
	}
	return nil
}
func DecodeRolloutManifest(r io.Reader) (*RolloutManifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxRolloutManifestBytes+1))
	if err != nil || len(data) > MaxRolloutManifestBytes {
		return nil, ErrConfig
	}
	var m *RolloutManifest
	if json.Unmarshal(data, &m, json.RejectUnknownMembers(true)) != nil || m == nil {
		return nil, ErrConfig
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// RolloutStep omits untrusted names. Durations are integer nanoseconds, as in the API.
type RolloutStep struct {
	Ratio             float64 `json:"ratio"`
	MinRuntime        int64   `json:"minRuntime"`
	ObservationWindow int64   `json:"observationWindow"`
	AutoAdvance       bool    `json:"autoAdvance"`
}

// RolloutEvent is bounded recent server history, not an authenticated audit export.
type RolloutEvent struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Step   *int      `json:"step"`
}

// RolloutPlan is an explicit allowlist; completion makes no business-win claim.
type RolloutPlan struct {
	Steps          []RolloutStep  `json:"steps"`
	State          string         `json:"state"`
	CurrentStep    *int           `json:"currentStep"`
	StepStartedAt  *time.Time     `json:"stepStartedAt,omitempty"`
	ManualOverride bool           `json:"manualOverride"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	History        []RolloutEvent `json:"history"`
}

// RolloutObservation separates snapshot availability from the optional plan state.
// PublicationAuthorized is always false, including for completed plans.
type RolloutObservation struct {
	RolloutManifest
	Availability          string       `json:"availability"`
	SourcePath            string       `json:"sourcePath"`
	ExperimentUpdatedAt   *time.Time   `json:"experimentUpdatedAt,omitempty"`
	PlanPresent           bool         `json:"planPresent"`
	Plan                  *RolloutPlan `json:"plan,omitempty"`
	PublicationAuthorized bool         `json:"publicationAuthorized"`
}

// Rollout reads exactly one experiment snapshot using the Reader's GET-only
// transport. Every validation failure returns nil, never a partial observation.
func (r *Reader) Rollout(ctx context.Context, m RolloutManifest) (*RolloutObservation, error) {
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
	var exp *struct {
		ID                  string       `json:"id"`
		WorkspaceID         string       `json:"workspaceId"`
		EnvironmentID       string       `json:"environmentId"`
		Epoch               int64        `json:"epoch"`
		RegistrationVersion string       `json:"registrationVersion"`
		UpdatedAt           time.Time    `json:"updatedAt"`
		ReleasePlan         *RolloutPlan `json:"releasePlan"`
	}
	missing, err := r.get(ctx, path, &exp)
	if err != nil {
		return nil, err
	}
	o := &RolloutObservation{RolloutManifest: m, SourcePath: path, Availability: "experiment_missing"}
	if missing {
		return o, nil
	}
	if exp == nil {
		return nil, ErrResponse
	}
	if exp.ID != m.ExperimentID || exp.WorkspaceID != m.WorkspaceID || exp.EnvironmentID != m.EnvironmentID || exp.Epoch != m.Epoch || exp.RegistrationVersion != m.RegistrationVersion {
		return nil, ErrMismatch
	}
	if !rolloutTime(exp.UpdatedAt) {
		return nil, ErrResponse
	}
	if exp.ReleasePlan != nil && (!validRolloutPlan(exp.ReleasePlan) || exp.ReleasePlan.UpdatedAt.After(exp.UpdatedAt)) {
		return nil, ErrResponse
	}
	o.Availability = "available"
	o.ExperimentUpdatedAt = &exp.UpdatedAt
	o.PlanPresent = exp.ReleasePlan != nil
	o.Plan = exp.ReleasePlan
	return o, nil
}
func rolloutTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func validRolloutPlan(p *RolloutPlan) bool {
	if len(p.Steps) < 1 || len(p.Steps) > MaxRolloutSteps || len(p.History) > MaxRolloutHistory || p.CurrentStep == nil || *p.CurrentStep < 0 || *p.CurrentStep >= len(p.Steps) || !rolloutTime(p.CreatedAt) || !rolloutTime(p.UpdatedAt) || p.UpdatedAt.Before(p.CreatedAt) {
		return false
	}
	switch p.State {
	case "armed":
		if *p.CurrentStep != 0 || p.StepStartedAt != nil {
			return false
		}
	case "completed":
		if *p.CurrentStep != len(p.Steps)-1 {
			return false
		}
	case "running", "paused", "rolled_back":
	default:
		return false
	}
	if p.StepStartedAt != nil && (!rolloutTime(*p.StepStartedAt) || p.StepStartedAt.Before(p.CreatedAt) || p.StepStartedAt.After(p.UpdatedAt)) {
		return false
	}
	if (p.State == "running" || p.State == "completed") && p.StepStartedAt == nil {
		return false
	}
	prev := 0.0
	for _, s := range p.Steps {
		if math.IsNaN(s.Ratio) || math.IsInf(s.Ratio, 0) || s.Ratio <= prev || s.Ratio > 100 || s.MinRuntime <= 0 || s.ObservationWindow < 0 || s.MinRuntime > int64(math.MaxInt64)-s.ObservationWindow {
			return false
		}
		prev = s.Ratio
	}
	for _, e := range p.History {
		if !rolloutTime(e.At) || e.Step == nil || *e.Step < 0 || *e.Step >= len(p.Steps) {
			return false
		}
		switch e.Action {
		case "plan.create", "plan.start", "step.advance", "plan.complete", "plan.rollback", "plan.pause", "manual.pause", "manual.resume", "manual.takeover_on", "manual.takeover_off", "manual.complete", "manual.advance", "repair_publish":
		default:
			return false
		}
	}
	return true
}

// Require the fields emitted by the platform while preserving its optional
// zero observation window and omitted false manualOverride convention.
func (s *RolloutStep) UnmarshalJSON(data []byte) error {
	type plain RolloutStep
	var v plain
	if !rolloutMembers(data, "ratio", "minRuntime", "autoAdvance") || json.Unmarshal(data, &v) != nil {
		return ErrResponse
	}
	*s = RolloutStep(v)
	return nil
}
func (p *RolloutPlan) UnmarshalJSON(data []byte) error {
	type plain RolloutPlan
	var v plain
	if !rolloutMembers(data, "steps", "state", "currentStep", "createdAt", "updatedAt") || json.Unmarshal(data, &v) != nil {
		return ErrResponse
	}
	*p = RolloutPlan(v)
	return nil
}
func rolloutMembers(data []byte, required ...string) bool {
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return false
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	// Only selected scalar/configuration fields are interpreted. Prose and
	// future server members remain outside this contract.
	for _, key := range []string{"ratio", "minRuntime", "observationWindow", "autoAdvance", "steps", "state", "currentStep", "manualOverride", "createdAt", "updatedAt"} {
		if value, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}
