package growthservice

import (
	"context"
	"sort"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

// StudyEntry is an operator-supplied observation. Authentication records who
// supplied it; it cannot establish independent human review or business benefit.
type StudyEntry struct {
	Kind             string    `json:"kind"` // manual_before, assisted, or recommendation_review
	DurationSeconds  int64     `json:"durationSeconds,omitempty"`
	RecommendationID string    `json:"recommendationId,omitempty"`
	QualityScore     int       `json:"qualityScore,omitempty"`
	Note             string    `json:"note"`
	Synthetic        bool      `json:"synthetic"`
	Actor            string    `json:"actor,omitempty"`
	At               time.Time `json:"at,omitempty"`
}
type BlindRecommendation struct {
	ID              string `json:"id"`
	Hypothesis      string `json:"hypothesis"`
	Mechanism       string `json:"mechanism"`
	Falsification   string `json:"falsification"`
	Risk            string `json:"risk"`
	Effort          string `json:"effort"`
	EvidenceVersion string `json:"evidenceVersion"`
}
type StudyExport struct {
	Schema                   string                    `json:"schema"`
	TaskID                   string                    `json:"taskId"`
	ProposalFingerprint      string                    `json:"proposalFingerprint"`
	Baseline                 *growth.AggregateBaseline `json:"baseline,omitempty"`
	Evidence                 *growth.Envelope          `json:"evidence,omitempty"`
	Recommendations          []BlindRecommendation     `json:"recommendations"`
	Rubric                   []string                  `json:"rubric"`
	Entries                  []StudyEntry              `json:"operatorSuppliedEntries"`
	IndependentHumanVerified bool                      `json:"independentHumanVerified"`
	Measurements             map[string]any            `json:"workflowMeasurements"`
	Limitations              []string                  `json:"limitations"`
}

func blindRecommendations(t *Task) []BlindRecommendation {
	out := []BlindRecommendation{}
	for _, s := range t.Suggestions {
		id := digest(struct{ Task, Hypothesis, Pin string }{t.ID, s.Hypothesis, s.EvidenceVersion})
		out = append(out, BlindRecommendation{ID: id, Hypothesis: s.Hypothesis, Mechanism: s.Mechanism, Falsification: s.Falsification, Risk: s.Risk, Effort: s.Effort, EvidenceVersion: s.EvidenceVersion})
	}
	// Opaque hash ordering hides original provider order. Model names and origin
	// never enter the review bundle. It is not an independent human evaluation.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (s *Service) recordStudy(ctx context.Context, t *Task, actor Principal, in StudyEntry) (*Task, error) {
	if in.Actor != "" || !in.At.IsZero() || len(t.Study) >= 64 || !boundedStudyText(in.Note) {
		return nil, ErrInput
	}
	switch in.Kind {
	case "manual_before", "assisted":
		if in.DurationSeconds < 1 || in.DurationSeconds > 7*24*3600 || in.RecommendationID != "" || in.QualityScore != 0 {
			return nil, ErrInput
		}
	case "recommendation_review":
		found := false
		for _, item := range blindRecommendations(t) {
			if item.ID == in.RecommendationID {
				found = true
			}
		}
		if !found || in.QualityScore < 1 || in.QualityScore > 5 || in.DurationSeconds != 0 {
			return nil, ErrInput
		}
	default:
		return nil, ErrInput
	}
	raw := []byte(in.Note)
	if s.containsSecret(raw) {
		return nil, ErrInput
	}
	in.Actor = actor.Name
	in.At = time.Now().UTC()
	return s.store.change(ctx, t.ID, t.Revision, actor.Name, "study_observation_recorded", func(t *Task) error { t.Study = append(t.Study, in); return nil })
}
func boundedStudyText(s string) bool { return len(s) > 0 && len(s) <= 1000 }
func (s *Service) study(ctx context.Context, t *Task) (*StudyExport, error) {
	history, err := s.store.history(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	fp, err := growth.ProposalFingerprint(t.Proposal)
	if err != nil {
		return nil, err
	}
	defects := 0
	approved, prepared := false, false
	var created, draft time.Time
	for _, a := range history {
		if created.IsZero() {
			created = a.At
		}
		if a.Action == "prepare" && a.State == "PREPARED" {
			prepared = true
			if draft.IsZero() {
				draft = a.At
			}
		}
		if a.Action == "review" && a.State == "APPROVED" {
			approved = true
		}
		if a.State == "HOLD" || a.State == "NEEDS_DATA" {
			defects++
		}
	}
	ready := growth.ReviewProposal(t.Proposal, t.Evidence)
	measurements := map[string]any{"taskCreatedAt": created, "firstPreparedAt": draft, "preregistrationReady": ready.State == growth.ProposalReadyForReview, "approved": approved, "prepared": prepared, "blockedCheckpoints": defects, "linkedExperiment": t.Manifest != nil, "learningRecorded": t.Learning != nil, "currentState": t.State, "validRun": t.Observation != nil && t.Observation.Status == growth.Ready}
	if !draft.IsZero() {
		measurements["workflowElapsedSeconds"] = draft.Sub(created).Seconds()
	}
	return &StudyExport{Schema: "growth.study.v1", TaskID: t.ID, ProposalFingerprint: fp, Baseline: t.Baseline, Evidence: t.Evidence, Recommendations: blindRecommendations(t), Rubric: []string{"Score usefulness and feasibility from one to five", "Verify each factual reference against the pinned evidence", "Record failure reasons; do not use model self-rating"}, Entries: append([]StudyEntry{}, t.Study...), IndependentHumanVerified: false, Measurements: measurements, Limitations: []string{"Operator-supplied entries are unverified observations, never signatures or consent", "Workflow elapsed time includes service waits and is not measured human effort", "blockedCheckpoints counts checkpoints, not distinct discovered defects", "validRun reflects current data-health readiness only; it does not establish superiority", "Synthetic samples and virtual time establish no actual user benefit or elapsed pilot"}}, nil
}

// studySummary provides explicit denominators and periods. It never turns
// service checkpoints into a claimed human defect-discovery or productivity rate.
func (s *Service) studySummary(ctx context.Context) (map[string]any, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	tasks, err := s.store.list(ctx)
	if err != nil {
		return nil, err
	}
	ready, prepared, linked, healthy, withBlocks, blocks := 0, 0, 0, 0, 0, 0
	for _, task := range tasks {
		bundle, e := s.study(ctx, task)
		if e != nil {
			return nil, e
		}
		if bundle.Measurements["preregistrationReady"] == true {
			ready++
		}
		if bundle.Measurements["prepared"] == true {
			prepared++
		}
		if task.Manifest != nil {
			linked++
			if task.Observation != nil && task.Observation.Status == growth.Ready {
				healthy++
			}
		}
		count := bundle.Measurements["blockedCheckpoints"].(int)
		blocks += count
		if count > 0 {
			withBlocks++
		}
	}
	records, err := s.store.journal(ctx)
	if err != nil {
		return nil, err
	}
	monthly := map[string]int{}
	for _, record := range records {
		monthly[record.RecordedAt.UTC().Format("2006-01")]++
	}
	return map[string]any{"schema": "growth.study-summary.v1", "workspaceId": s.cfg.Evidence.WorkspaceID, "environmentId": s.cfg.Evidence.EnvironmentID, "generatedAt": time.Now().UTC(), "taskDenominator": len(tasks), "currentlyReadyPreregistrations": ready, "everPreparedTasks": prepared, "linkedRunDenominator": linked, "currentlyHealthyLinkedRuns": healthy, "tasksWithBlockedCheckpoints": withBlocks, "blockedCheckpoints": blocks, "monthlyLearningSnapshotsUTC": monthly, "dataDefectDiscoveryRate": nil, "independentHumanVerified": false, "limitations": []string{"Scope is the current retained database, not an entire organization", "Readiness and healthy-run counts are current snapshots, not business success", "Learning counts are immutable snapshots, not distinct experiments", "Blocked checkpoints may repeat; distinct defect ground truth is unavailable", "Human timing/recommendation observations remain supplied and unverified"}}, nil
}
