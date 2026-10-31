package growth

import (
	"errors"
	"slices"
)

var ErrProposalInitialization = errors.New("growth: invalid proposal initialization input")

// InitProposal constructs an incomplete, pinned draft from a complete validated
// goal and independently supplied operator scope. It supplies no planning claims,
// evidence, execution defaults, implementation attestations or authorization.
// The returned proposal owns its mutable fields independently of the goal.
func InitProposal(g GrowthGoal, id string, version int64, workspace, environment string) (*Proposal, error) {
	if ValidateGoal(g) != nil || !validSegment(id) || version < 1 ||
		!validSegment(workspace) || !validSegment(environment) ||
		g.WorkspaceID != workspace || g.EnvironmentID != environment {
		return nil, ErrProposalInitialization
	}
	fp, err := GoalFingerprint(g)
	if err != nil {
		return nil, ErrProposalInitialization
	}
	return &Proposal{
		SchemaVersion: ProposalSchemaVersion,
		ID:            id,
		Version:       version,
		GoalID:        g.ID,
		GoalPin:       &GoalPin{Version: g.Version, Fingerprint: fp},
		WorkspaceID:   workspace,
		EnvironmentID: environment,
		PrimaryMetric: g.PrimaryMetric,
		Guardrails:    slices.Clone(g.Guardrails),
	}, nil
}
