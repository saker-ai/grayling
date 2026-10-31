package growth

import "context"

const GoalCheckSchemaVersion = "growth.goal-check.v1"

// GoalCheck links caller-attested goal content to an existing monitor snapshot.
// READY means only inspectable matching experiment evidence, never a product
// baseline, diagnosis, approval, business win or permission to publish.
type GoalCheck struct {
	SchemaVersion         string           `json:"schemaVersion"`
	WorkspaceID           string           `json:"workspaceId"`
	EnvironmentID         string           `json:"environmentId"`
	GoalID                string           `json:"goalId"`
	GoalVersion           int64            `json:"goalVersion"`
	GoalFingerprint       string           `json:"goalFingerprint"`
	Observation           *MonitorSummary  `json:"observation"`
	Status                Status           `json:"status"`
	Blockers              []MonitorBlocker `json:"blockers"`
	PublicationAuthorized bool             `json:"publicationAuthorized"`
}

// CheckGoal validates local artifacts against the Reader's operator scope before
// HTTP. It reuses Monitor without reinterpreting health or statistical verdicts.
// Invalid local inputs return ErrGoal, ErrConfig or ErrGoalVerification;
// evidence execution errors retain Monitor's errors.
func (r *Reader) CheckGoal(ctx context.Context, g GrowthGoal, m MonitorManifest) (*GoalCheck, error) {
	fp, err := GoalFingerprint(g)
	if err != nil {
		return nil, err
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	guards := make([]MetricRef, len(g.Guardrails))
	for i, guard := range g.Guardrails {
		guards[i] = guard.Ref
	}
	if g.WorkspaceID != r.workspace || m.WorkspaceID != r.workspace || g.EnvironmentID != r.environment || m.EnvironmentID != r.environment || g.PrimaryMetric.Ref != m.PrimaryMetric || !exactMonitorGuards(m.Guardrails, guards) {
		return nil, ErrGoalVerification
	}
	observation, err := r.Monitor(ctx, m)
	if err != nil {
		return nil, err
	}
	s := &GoalCheck{SchemaVersion: GoalCheckSchemaVersion, WorkspaceID: g.WorkspaceID, EnvironmentID: g.EnvironmentID, GoalID: g.ID, GoalVersion: g.Version, GoalFingerprint: fp, Observation: observation, Status: observation.Status, Blockers: append([]MonitorBlocker{}, observation.Blockers...)}
	// No report means no observed interval. Missing metrics may coexist with an
	// observed report: retain NEEDS_DATA even when that interval also mismatches.
	if observation.EvidenceVersion != "" && (!g.BaselineWindow.Start.Equal(observation.Window.Start) || !g.BaselineWindow.End.Equal(observation.Window.End)) {
		s.Blockers = append(s.Blockers, MonitorBlocker{Code: "baseline_window_mismatch", Remediation: "review_baseline_window"})
		if s.Status != NeedsData {
			s.Status = Hold
		}
	}
	return s, nil
}
