package growth

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatermarkGenerationBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delta  time.Duration
		offset int
		valid  bool
	}{
		{"future second", time.Second, 0, false},
		{"future nanosecond", time.Nanosecond, 0, false},
		{"report only future", time.Nanosecond, 0, false},
		{"diagnostics only future", time.Nanosecond, 0, false},
		{"earlier second", -time.Second, 0, true},
		{"earlier nanosecond", -time.Nanosecond, 0, true},
		{"equal UTC", 0, 0, true},
		{"equal positive offset", 0, 8 * 3600, true},
		{"equal negative offset", 0, -7 * 3600, true},
		{"future offset", time.Nanosecond, -7 * 3600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, original := proposalFixture()
			approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			report := original.Evidence[0].Report
			report.GeneratedAt = report.Window.Watermark.Add(-tc.delta)
			report.Diagnostics.GeneratedAt = report.GeneratedAt
			report.Window.Watermark = report.Window.Watermark.In(time.FixedZone("fixture", tc.offset))
			report.Diagnostics.Window = report.Window
			if tc.name == "report only future" {
				report.Diagnostics.Window.Watermark = report.GeneratedAt
			}
			if tc.name == "diagnostics only future" {
				report.Window.Watermark = report.GeneratedAt
			}

			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					writes.Add(1)
					w.WriteHeader(405)
					return
				}
				for _, metric := range original.Metrics {
					if req.URL.RequestURI() == metric.SourcePath {
						json.MarshalWrite(w, metric)
						return
					}
				}
				if req.URL.RequestURI() != original.Evidence[0].SourcePath {
					t.Error("unexpected report path")
					w.WriteHeader(404)
					return
				}
				json.MarshalWrite(w, map[string]any{"workspaceId": p.WorkspaceID, "environmentId": p.EnvironmentID, "report": report})
			}))
			defer srv.Close()
			reader := readerFor(t, srv.URL, nil)
			e, err := reader.Read(t.Context(), Query{Metrics: []MetricRef{p.PrimaryMetric.Ref, p.Guardrails[0].Ref}, Experiments: []ExperimentRef{{ID: report.ExperimentID, Epoch: report.Epoch}}})
			if err != nil {
				t.Fatal(err)
			}
			want, state := Hold, ProposalNeedsData
			if tc.valid {
				want, state = Ready, ProposalReadyForReview
			}
			if e.Status != want || e.Evidence[0].Status != want {
				t.Fatalf("evidence: %+v", e)
			}
			review := ReviewProposal(p, e)
			if review.State != state {
				t.Fatal(review)
			}
			got, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
			if (err != nil) != !tc.valid || (got == nil) != !tc.valid {
				t.Fatalf("approval: %+v %v", got, err)
			}
			for _, decision := range []ReviewDecision{ReviewReject, ReviewRequestChanges} {
				record, err := RecordReview(p, e, reviewInputs(t, p, decision))
				if err != nil || record == nil || !reflect.DeepEqual(record.Readiness, review) {
					t.Fatalf("negative review: %+v %v", record, err)
				}
			}
			plain, err := CompileCreateRequest(p, e, preparationFixture(p))
			if (err != nil) != !tc.valid || (plain == nil) != !tc.valid {
				t.Fatalf("plain preparation: %+v %v", plain, err)
			}
			linked, err := CompileReviewedCreateRequest(p, *approval, p.WorkspaceID, p.EnvironmentID, e, preparationFixture(p))
			if (err != nil) != !tc.valid || (linked == nil) != !tc.valid {
				t.Fatalf("reviewed preparation: %+v %v", linked, err)
			}
			m := MonitorManifest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []MetricRef{p.Guardrails[0].Ref}}
			summary, err := reader.Monitor(t.Context(), m)
			blocker := MonitorBlocker{"window_metadata_invalid", "review_window"}
			if err != nil || summary.Status != want || summary.PublicationAuthorized || slices.Contains(summary.Blockers, blocker) == tc.valid {
				t.Fatalf("monitor: %+v %v", summary, err)
			}
			if !tc.valid {
				expected := []MonitorBlocker{blocker}
				if tc.name == "report only future" || tc.name == "diagnostics only future" {
					expected = append(expected, MonitorBlocker{"diagnostics_metadata_invalid", "refresh_report"})
				}
				if !reflect.DeepEqual(summary.Blockers, expected) {
					t.Fatal(summary.Blockers)
				}
			}

			fp, err := ProposalFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: p.AnalysisPlan.Provenance.SuppliedAt, Followups: []string{"review diagnostic times"}})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(learning)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeLearningRecord(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			verification, err := VerifyLearning(p, *decoded, p.WorkspaceID, p.EnvironmentID)
			if err != nil || verification.ObservationStatus != want || verification.PublicationAuthorized || verification.BusinessWinEstablished {
				t.Fatalf("verify: %+v %v", verification, err)
			}
			result, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: "review diagnostic"})
			if err != nil || len(result.Matches) != 1 || !reflect.DeepEqual(result.Matches[0].Observation, *summary) {
				t.Fatalf("search: %+v %v", result, err)
			}
			if writes.Load() != 0 {
				t.Fatal("non-GET requests", writes.Load())
			}
		})
	}
}

func TestLearningWatermarkConsistency(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delta   time.Duration
		offset  int
		status  Status
		blocker bool
		valid   bool
	}{
		{"future READY", time.Nanosecond, 0, Ready, false, false},
		{"future unrelated HOLD", time.Nanosecond, 0, Hold, false, false},
		{"future blocked HOLD", time.Nanosecond, 0, Hold, true, true},
		{"equal", 0, 0, Ready, false, true},
		{"equal offset", 0, 8 * 3600, Ready, false, true},
		{"earlier", -time.Nanosecond, 0, Ready, false, true},
		{"future offset READY", time.Nanosecond, -7 * 3600, Ready, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := proposalFixture()
			r := searchFixture(t)
			r.Observation.GeneratedAt = r.Observation.GeneratedAt.Add(123456789 * time.Nanosecond)
			r.Observation.Window.Watermark = r.Observation.GeneratedAt.Add(tc.delta).In(time.FixedZone("fixture", tc.offset))
			r.Observation.Status = tc.status
			if tc.status == Hold {
				r.Observation.Blockers = []MonitorBlocker{{"health_review_required", "review_diagnostics"}}
			}
			if tc.blocker {
				r.Observation.Blockers = []MonitorBlocker{{"window_metadata_invalid", "review_window"}}
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeLearningRecord(bytes.NewReader(data))
			if (err == nil) != tc.valid {
				t.Fatalf("decode: %+v %v", decoded, err)
			}
			verified, err := VerifyLearning(p, r, p.WorkspaceID, p.EnvironmentID)
			if (err == nil) != tc.valid || (!tc.valid && verified != nil) {
				t.Fatalf("verify: %+v %v", verified, err)
			}
			result, err := SearchLearning([]LearningRecord{r}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: r.OriginalHypothesis})
			if (err == nil) != tc.valid || (!tc.valid && result != nil) {
				t.Fatalf("search: %+v %v", result, err)
			}
			if tc.valid {
				if len(result.Matches) != 1 {
					t.Fatal("lost observation")
				}
				copied, err := json.Marshal(result.Matches[0])
				if err != nil || !bytes.Equal(data, copied) {
					t.Fatal("lost observation")
				}
			}
		})
	}
}
