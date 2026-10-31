package growth

const GoalLearningSearchSchemaVersion = "growth.goal-learning-search.v1"

// GoalLearningSearchResult reports metric compatibility with a caller-attested
// goal. It does not establish historical goal linkage, transferability,
// freshness, approval or causal benefit. Goal prose is deliberately omitted.
type GoalLearningSearchResult struct {
	SchemaVersion   string           `json:"schemaVersion"`
	WorkspaceID     string           `json:"workspaceId"`
	EnvironmentID   string           `json:"environmentId"`
	GoalID          string           `json:"goalId"`
	GoalVersion     int64            `json:"goalVersion"`
	GoalFingerprint string           `json:"goalFingerprint"`
	Matches         []LearningRecord `json:"matches"`
}

// SearchGoalLearning is pure. The query supplies independent operator scope.
// SearchLearning validates and bounds every record, including excluded records,
// before exact linked-experiment metric-role filtering. Baseline windows are not
// a search filter; complete READY/HOLD/NEEDS_DATA snapshots remain unchanged.
func SearchGoalLearning(g GrowthGoal, records []LearningRecord, q LearningSearchQuery) (*GoalLearningSearchResult, error) {
	fp, err := GoalFingerprint(g)
	if err != nil {
		return nil, err
	}
	if q.Validate() != nil || g.WorkspaceID != q.WorkspaceID || g.EnvironmentID != q.EnvironmentID {
		return nil, ErrGoalVerification
	}
	search, err := SearchLearning(records, q)
	if err != nil {
		return nil, err
	}
	guards := make([]MetricRef, len(g.Guardrails))
	for i, guard := range g.Guardrails {
		guards[i] = guard.Ref
	}
	result := &GoalLearningSearchResult{SchemaVersion: GoalLearningSearchSchemaVersion, WorkspaceID: q.WorkspaceID, EnvironmentID: q.EnvironmentID, GoalID: g.ID, GoalVersion: g.Version, GoalFingerprint: fp, Matches: []LearningRecord{}}
	for _, record := range search.Matches {
		m := record.LinkedExperiment
		if m.PrimaryMetric == g.PrimaryMetric.Ref && exactMonitorGuards(m.Guardrails, guards) {
			result.Matches = append(result.Matches, record)
		}
	}
	return result, nil
}
