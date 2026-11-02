package growthservice

import (
	"encoding/json/v2"
	"testing"
)

func TestStudyBundleAndUnverifiedObservations(t *testing.T) {
	f := newFixture(t, nil)
	task := f.create(t)
	f.service.process(t.Context())
	task = f.load(t, task.ID)
	var err error
	task, err = f.store.change(t.Context(), task.ID, task.Revision, "synthetic", "synthetic_suggestions", func(t *Task) error { t.Suggestions = suggestions(hb); return nil })
	if err != nil {
		t.Fatal(err)
	}
	status, raw := f.call(t, "GET", "/v1/tasks/"+task.ID+"/study", viewSecret, nil, "")
	if status != 200 {
		t.Fatal(status)
	}
	var bundle StudyExport
	if json.Unmarshal(raw, &bundle) != nil || len(bundle.Recommendations) != 2 || bundle.IndependentHumanVerified {
		t.Fatal("invalid blind preparation")
	}
	entry := StudyEntry{Kind: "recommendation_review", RecommendationID: bundle.Recommendations[0].ID, QualityScore: 4, Note: "Synthetic harness example only", Synthetic: true}
	task = f.act(t, task, "study", Action{Study: &entry}, 200)
	if len(task.Study) != 1 || task.Study[0].Actor != "reviewer" || task.Study[0].At.IsZero() {
		t.Fatal("missing durable provenance")
	}
	entry.RecommendationID = "invented"
	f.act(t, task, "study", Action{Study: &entry}, 422)
	entry = StudyEntry{Kind: "manual_before", DurationSeconds: 300, Note: "Synthetic time example", Synthetic: true}
	task = f.load(t, task.ID)
	task = f.act(t, task, "study", Action{Study: &entry}, 200)
	bundlePtr, e := f.service.study(t.Context(), task)
	if e != nil || len(bundlePtr.Entries) != 2 || bundlePtr.IndependentHumanVerified {
		t.Fatal("fabricated independent evidence")
	}
	status, summary := f.call(t, "GET", "/v1/study", viewSecret, nil, "")
	var counts map[string]any
	if status != 200 || json.Unmarshal(summary, &counts) != nil || counts["taskDenominator"] != float64(1) || counts["dataDefectDiscoveryRate"] != nil {
		t.Fatal("unproven study rate")
	}
	status, archive := f.call(t, "GET", "/v1/archive", viewSecret, nil, "")
	if status != 200 || len(archive) == 0 {
		t.Fatal("archive unavailable")
	}
	if e = f.store.Verify(t.Context()); e != nil {
		t.Fatal(e)
	}
}
