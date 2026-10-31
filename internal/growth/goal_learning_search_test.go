package growth

import (
	"encoding/json/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSearchGoalLearning(t *testing.T) {
	g, _ := goalFixture()
	q := LearningSearchQuery{g.WorkspaceID, g.EnvironmentID, "introduction"}
	r := searchFixture(t)
	// Exact adjacent integers above 2^53 must not collapse through JSON.
	g.PrimaryMetric.Ref.Version = 1<<53 + 1
	g.PrimaryMetric.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + g.PrimaryMetric.Ref.Key + "/versions/9007199254740993"
	r.LinkedExperiment.PrimaryMetric = g.PrimaryMetric.Ref
	r.Observation.PrimaryMetric = g.PrimaryMetric.Ref
	g.Guardrails[0].Ref.Version = 1<<53 + 1
	g.Guardrails[0].SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + g.Guardrails[0].Ref.Key + "/versions/9007199254740993"
	r.LinkedExperiment.Guardrails[0] = g.Guardrails[0].Ref
	r.Observation.Guardrails = append([]MetricRef{}, r.LinkedExperiment.Guardrails...)
	extra := g.Guardrails[0]
	extra.Ref.Key = "extra"
	extra.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/extra/versions/" + strconv.FormatInt(extra.Ref.Version, 10)
	g.Guardrails = append(g.Guardrails, extra)
	r.LinkedExperiment.Guardrails = append(r.LinkedExperiment.Guardrails, extra.Ref)
	r.Observation.Guardrails = append([]MetricRef{}, r.LinkedExperiment.Guardrails...)
	fp, _ := GoalFingerprint(g)
	got, err := SearchGoalLearning(g, []LearningRecord{r, r}, q)
	if err != nil || got.SchemaVersion != GoalLearningSearchSchemaVersion || got.GoalVersion != g.Version || got.GoalID != g.ID || got.GoalFingerprint != fp || !reflect.DeepEqual(got.Matches, []LearningRecord{r}) {
		t.Fatalf("%+v %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), g.Objective) || strings.Contains(string(raw), `"owner"`+`:"owner"`) {
		t.Fatal("goal prose leaked")
	}
	g.Guardrails[0], g.Guardrails[1] = g.Guardrails[1], g.Guardrails[0]
	reordered, err := SearchGoalLearning(g, []LearningRecord{r}, q)
	if err != nil || !reflect.DeepEqual(got.Matches, reordered.Matches) {
		t.Fatal("guard order", err)
	}
	for name, change := range map[string]func(*LearningRecord){
		"primary version": func(r *LearningRecord) { r.LinkedExperiment.PrimaryMetric.Version-- },
		"primary hash":    func(r *LearningRecord) { r.LinkedExperiment.PrimaryMetric.DefinitionHash = hashB },
		"primary key":     func(r *LearningRecord) { r.LinkedExperiment.PrimaryMetric.Key = "another" },
		"guard version":   func(r *LearningRecord) { r.LinkedExperiment.Guardrails[0].Version-- },
		"guard hash":      func(r *LearningRecord) { r.LinkedExperiment.Guardrails[0].DefinitionHash = hashB },
		"guard key":       func(r *LearningRecord) { r.LinkedExperiment.Guardrails[0].Key = "another" },
		"roles": func(r *LearningRecord) {
			r.LinkedExperiment.PrimaryMetric, r.LinkedExperiment.Guardrails[0] = r.LinkedExperiment.Guardrails[0], r.LinkedExperiment.PrimaryMetric
		},
		"missing guard": func(r *LearningRecord) { r.LinkedExperiment.Guardrails = r.LinkedExperiment.Guardrails[:1] },
		"extra guard": func(r *LearningRecord) {
			r.LinkedExperiment.Guardrails = append(r.LinkedExperiment.Guardrails, MetricRef{Key: "third", Version: 1, DefinitionHash: hashA})
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(r)
			var changed LearningRecord
			_ = json.Unmarshal(raw, &changed)
			change(&changed)
			changed.Observation.PrimaryMetric = changed.LinkedExperiment.PrimaryMetric
			changed.Observation.Guardrails = changed.LinkedExperiment.Guardrails
			got, err := SearchGoalLearning(g, []LearningRecord{changed}, q)
			if err != nil || len(got.Matches) != 0 {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	for _, status := range []Status{Ready, Hold, NeedsData} {
		snapshot := r
		snapshot.Observation.Status = status
		if status == Hold {
			snapshot.Observation.Blockers = []MonitorBlocker{{"health_review_required", "review_diagnostics"}}
		}
		if status == NeedsData {
			snapshot.Observation = MonitorSummary{WorkspaceID: r.WorkspaceID, EnvironmentID: r.EnvironmentID, ExperimentID: r.LinkedExperiment.ExperimentID, Epoch: r.LinkedExperiment.Epoch, PrimaryMetric: r.LinkedExperiment.PrimaryMetric, Guardrails: r.LinkedExperiment.Guardrails, Status: NeedsData, Blockers: []MonitorBlocker{{"evidence_missing", "restore_evidence_access"}}}
		}
		found, err := SearchGoalLearning(g, []LearningRecord{snapshot}, q)
		if err != nil || !reflect.DeepEqual(found.Matches, []LearningRecord{snapshot}) {
			t.Fatal("snapshot", status, err)
		}
	}
	// A distinct baseline interval is intentionally irrelevant to search.
	g.BaselineWindow.Start = g.BaselineWindow.Start.AddDate(-1, 0, 0)
	if found, err := SearchGoalLearning(g, []LearningRecord{r}, q); err != nil || len(found.Matches) != 1 {
		t.Fatal("baseline filter", err)
	}
	for len(g.Guardrails) < 20 {
		m := extra
		m.Ref.Key = "g" + strconv.Itoa(len(g.Guardrails))
		m.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + m.Ref.Key + "/versions/" + strconv.FormatInt(m.Ref.Version, 10)
		g.Guardrails = append(g.Guardrails, m)
	}
	if found, err := SearchGoalLearning(g, []LearningRecord{r}, q); err != nil || len(found.Matches) != 0 || found.Matches == nil {
		t.Fatal("large goal", err)
	}
}

func TestSearchGoalLearningValidatesExcludedRecords(t *testing.T) {
	g, _ := goalFixture()
	q := LearningSearchQuery{g.WorkspaceID, g.EnvironmentID, "absent"}
	for name, change := range map[string]func(*LearningRecord){
		"invalid": func(r *LearningRecord) { r.ProposalVersion = 0 },
		"metric excluded invalid": func(r *LearningRecord) {
			r.LinkedExperiment.PrimaryMetric.Version++
			r.Observation.PrimaryMetric = r.LinkedExperiment.PrimaryMetric
			r.PublicationAuthorized = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := searchFixture(t)
			change(&r)
			if got, err := SearchGoalLearning(g, []LearningRecord{r}, q); err == nil || got != nil {
				t.Fatal("accepted excluded record")
			}
		})
	}
	for _, q := range []LearningSearchQuery{{"other", "env1", "x"}, {"ws1", "other", "x"}, {"ws1", "env1", ""}} {
		if got, err := SearchGoalLearning(g, []LearningRecord{searchFixture(t)}, q); err == nil || got != nil {
			t.Fatal("scope/query")
		}
	}
	g.Version = 0
	if got, err := SearchGoalLearning(g, []LearningRecord{searchFixture(t)}, q); err == nil || got != nil {
		t.Fatal("invalid goal")
	}
}

func TestSearchGoalLearningBoundsAndIsolation(t *testing.T) {
	g, _ := goalFixture()
	q := LearningSearchQuery{g.WorkspaceID, g.EnvironmentID, "introduction"}
	r := searchFixture(t)
	other := searchFixture(t)
	other.WorkspaceID = "other"
	other.LinkedExperiment.WorkspaceID = "other"
	other.Observation.WorkspaceID = "other"
	second := searchFixture(t)
	second.ProposalID = "a"
	a, err := SearchGoalLearning(g, []LearningRecord{r, other, second, r}, q)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SearchGoalLearning(g, []LearningRecord{second, r, other}, q)
	if err != nil || !reflect.DeepEqual(a, b) || len(a.Matches) != 2 || a.Matches[0].ProposalID != "a" {
		t.Fatal("ordering/isolation", err)
	}
	a.Matches[1].Followups[0] = "modified"
	if reflect.DeepEqual(a.Matches[1], r) {
		t.Fatal("snapshot alias")
	}
	// A locally valid but oversized excluded record must fail, rather than being
	// skipped by the metric, text or workspace filter.
	large := other
	ref := large.ProposalBaselineReferences[0]
	for {
		data, _ := json.Marshal(large)
		if len(data) > MaxLearningRecordBytes {
			break
		}
		large.ProposalBaselineReferences = append(large.ProposalBaselineReferences, ref)
	}
	if validateLearningRecord(large) != nil {
		t.Fatal("bound fixture structurally invalid")
	}
	if got, err := SearchGoalLearning(g, []LearningRecord{r, large}, q); err == nil || got != nil {
		t.Fatal("excluded byte bound")
	}
	// Total bytes count excluded records and duplicates, before deduplication.
	large = other
	for {
		data, _ := json.Marshal(large)
		if len(data) > MaxLearningRecordBytes-1024 {
			break
		}
		large.ProposalBaselineReferences = append(large.ProposalBaselineReferences, ref)
	}
	records := make([]LearningRecord, MaxLearningSearchFiles)
	for i := range records {
		records[i] = large
	}
	if got, err := SearchGoalLearning(g, records, q); err == nil || got != nil {
		t.Fatal("excluded total bound")
	}
	for _, records := range [][]LearningRecord{nil, make([]LearningRecord, MaxLearningSearchFiles+1)} {
		if got, err := SearchGoalLearning(g, records, q); err == nil || got != nil {
			t.Fatal("file count bound")
		}
	}
}
