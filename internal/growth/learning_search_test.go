package growth

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

func searchFixture(t *testing.T) LearningRecord {
	t.Helper()
	p, e := proposalFixture()
	fp, err := ProposalFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	report := e.Evidence[0].Report
	return LearningRecord{
		SchemaVersion: LearningRecordSchemaVersion, ProposalID: p.ID, ProposalVersion: p.Version, ProposalFingerprint: fp,
		WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, OriginalHypothesis: p.Hypothesis, ProposalBaselineReferences: p.EvidenceRefs,
		LinkedExperiment: MonitorManifest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, EvidenceVersion: report.EvidenceVersion, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []MetricRef{p.Guardrails[0].Ref}},
		Observation:      MonitorSummary{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: report.ExperimentID, Epoch: report.Epoch, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []MetricRef{p.Guardrails[0].Ref}, Status: Ready, ServerVerdict: report.Decision.Verdict, EvidenceVersion: report.EvidenceVersion, SourcePath: reportSource(report.ExperimentID), GeneratedAt: report.GeneratedAt, RegistrationVersion: report.RegistrationVersion, AnalysisMethod: report.AnalysisMethod, Window: report.Window, Blockers: []MonitorBlocker{}},
		Recorder:         "owner", RecordedAt: report.GeneratedAt, Followups: []string{"Inspect [literal].* $(touch sentinel) after HOLD"},
	}
}

func TestSearchLearningContract(t *testing.T) {
	q := LearningSearchQuery{"ws1", "env1", "introduction"}
	r := searchFixture(t)
	data, _ := json.Marshal(r)
	if _, err := DecodeLearningRecord(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text  string
		count int
	}{{"introduction", 1}, {"[literal].* $(touch sentinel)", 1}, {"INTRODUCTION", 0}, {".*$", 0}, {r.Recorder, 0}} {
		q.Text = tc.text
		got, err := SearchLearning([]LearningRecord{r}, q)
		if err != nil || len(got.Matches) != tc.count {
			t.Fatalf("%q: %+v %v", tc.text, got, err)
		}
		if tc.count == 1 && !reflect.DeepEqual(got.Matches[0], r) {
			t.Fatal("lost snapshot")
		}
	}
	q.Text = "introduction"
	other := searchFixture(t)
	other.WorkspaceID = "other"
	other.LinkedExperiment.WorkspaceID = "other"
	other.Observation.WorkspaceID = "other"
	other.OriginalHypothesis = "PRIVATE introduction"
	env := searchFixture(t)
	env.EnvironmentID = "other"
	env.LinkedExperiment.EnvironmentID = "other"
	env.Observation.EnvironmentID = "other"
	env.OriginalHypothesis = "PRIVATE introduction"
	second := searchFixture(t)
	second.ProposalID = "a"
	a, err := SearchLearning([]LearningRecord{r, other, second, env, r}, q)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SearchLearning([]LearningRecord{env, second, r, other}, q)
	if err != nil || !reflect.DeepEqual(a, b) || len(a.Matches) != 2 || a.Matches[0].ProposalID != "a" {
		t.Fatalf("ordering/isolation: %+v %v", a, err)
	}
	a.Matches[1].Followups[0] = "changed"
	if r.Followups[0] == "changed" {
		t.Fatal("aliased input")
	}
	for _, status := range []Status{Hold, NeedsData} {
		held := searchFixture(t)
		held.Observation.Status = status
		held.Observation.Blockers = []MonitorBlocker{{"health_review_required", "review_diagnostics"}}
		if status == NeedsData {
			held.Observation.Blockers = []MonitorBlocker{{"evidence_missing", "restore_evidence_access"}}
		}
		got, err := SearchLearning([]LearningRecord{held}, q)
		if err != nil || got.Matches[0].Observation.Status != status {
			t.Fatal("lost HOLD/NEEDS_DATA", err)
		}
	}
	missing := searchFixture(t)
	missing.Observation = MonitorSummary{WorkspaceID: r.WorkspaceID, EnvironmentID: r.EnvironmentID, ExperimentID: r.LinkedExperiment.ExperimentID, Epoch: r.LinkedExperiment.Epoch, PrimaryMetric: r.LinkedExperiment.PrimaryMetric, Guardrails: r.LinkedExperiment.Guardrails, Status: NeedsData, Blockers: []MonitorBlocker{{"evidence_missing", "restore_evidence_access"}}}
	if got, err := SearchLearning([]LearningRecord{missing}, q); err != nil || got.Matches[0].Observation.EvidenceVersion != "" {
		t.Fatal("missing report", err)
	}
}

func TestSearchLearningRejectsConflicts(t *testing.T) {
	mutations := map[string]func(*LearningRecord){
		"schema":                  func(r *LearningRecord) { r.SchemaVersion = "future" },
		"fingerprint":             func(r *LearningRecord) { r.ProposalFingerprint = "latest" },
		"proposal":                func(r *LearningRecord) { r.ProposalVersion = 0 },
		"scope":                   func(r *LearningRecord) { r.Observation.WorkspaceID = "other" },
		"environment":             func(r *LearningRecord) { r.LinkedExperiment.EnvironmentID = "other" },
		"experiment":              func(r *LearningRecord) { r.Observation.ExperimentID = "other" },
		"epoch":                   func(r *LearningRecord) { r.Observation.Epoch++ },
		"registration":            func(r *LearningRecord) { r.Observation.RegistrationVersion = hashB },
		"evidence":                func(r *LearningRecord) { r.Observation.EvidenceVersion = hashA },
		"source":                  func(r *LearningRecord) { r.Observation.SourcePath = "https://attacker" },
		"primary":                 func(r *LearningRecord) { r.Observation.PrimaryMetric.Version++ },
		"guards":                  func(r *LearningRecord) { r.Observation.Guardrails = nil },
		"baseline source":         func(r *LearningRecord) { r.ProposalBaselineReferences[0].SourcePath = "https://attacker" },
		"baseline version":        func(r *LearningRecord) { r.ProposalBaselineReferences[0].EvidenceVersion = "" },
		"business win":            func(r *LearningRecord) { r.BusinessWinEstablished = true },
		"publication":             func(r *LearningRecord) { r.PublicationAuthorized = true },
		"observation publication": func(r *LearningRecord) { r.Observation.PublicationAuthorized = true },
		"ready missing evidence":  func(r *LearningRecord) { r.Observation.EvidenceVersion = "" },
		"ready window":            func(r *LearningRecord) { r.Observation.Window.Complete = false },
		"ready window metadata":   func(r *LearningRecord) { r.Observation.Window.Version = "" },
		"status":                  func(r *LearningRecord) { r.Observation.Status = "SHIP" },
		"blocker": func(r *LearningRecord) {
			r.Observation.Status = Hold
			r.Observation.Blockers = []MonitorBlocker{{"execute", "publish"}}
		},
		"followups": func(r *LearningRecord) { r.Followups = nil },
		"text":      func(r *LearningRecord) { r.OriginalHypothesis = "\x00" },
		"recorder":  func(r *LearningRecord) { r.Recorder = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := searchFixture(t)
			mutate(&r)
			// Even an excluded invalid record aborts the entire search.
			got, err := SearchLearning([]LearningRecord{searchFixture(t), r}, LearningSearchQuery{"other", "env1", "absent"})
			if err == nil || got != nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	for _, q := range []LearningSearchQuery{{"", "env1", "x"}, {"ws1", "", "x"}, {"ws1", "env1", ""}, {"ws1", "env1", strings.Repeat("x", 4097)}} {
		if got, err := SearchLearning([]LearningRecord{searchFixture(t)}, q); err == nil || got != nil {
			t.Fatal("invalid query")
		}
	}
	if got, err := SearchLearning(make([]LearningRecord, MaxLearningSearchFiles+1), LearningSearchQuery{"ws1", "env1", "x"}); err == nil || got != nil {
		t.Fatal("file count")
	}
}

func TestDecodeLearningRecordStrict(t *testing.T) {
	data, _ := json.Marshal(searchFixture(t))
	s := string(data)
	for _, raw := range []string{"", "null", "[]", "{", s + " {}", `{"unknown":true,` + s[1:], `{"schemaVersion":"growth.learning.v1",` + s[1:], `{"schema\u0056ersion":"growth.learning.v1",` + s[1:], strings.Replace(s, "Inspect", "\xff", 1), strings.Repeat(" ", MaxLearningRecordBytes+1)} {
		if r, err := DecodeLearningRecord(strings.NewReader(raw)); err == nil || r != nil {
			t.Fatal("accepted malformed JSON")
		}
	}
}
