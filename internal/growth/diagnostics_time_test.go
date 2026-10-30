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

func TestDiagnosticsGenerationTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delta  time.Duration
		offset int
		valid  bool
	}{
		{"earlier second", -time.Second, 0, false},
		{"later second", time.Second, 0, false},
		{"earlier nanosecond", -time.Nanosecond, 0, false},
		{"later nanosecond", time.Nanosecond, 0, false},
		{"missing diagnostics time", 0, 0, false},
		{"missing report time", 0, 0, false},
		{"both times missing", 0, 0, false},
		{"equal UTC", 0, 0, true},
		{"equal positive offset", 0, 8 * 3600, true},
		{"equal negative offset", 0, -7 * 3600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, original := proposalFixture()
			approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			report := original.Evidence[0].Report
			// Retain subsecond precision through the HTTP JSON round trip.
			report.GeneratedAt = report.GeneratedAt.Add(123456789 * time.Nanosecond)
			report.Diagnostics.GeneratedAt = report.GeneratedAt.Add(tc.delta).In(time.FixedZone("fixture", tc.offset))
			switch tc.name {
			case "missing diagnostics time":
				report.Diagnostics.GeneratedAt = time.Time{}
			case "missing report time":
				report.GeneratedAt = time.Time{}
			case "both times missing":
				report.GeneratedAt, report.Diagnostics.GeneratedAt = time.Time{}, time.Time{}
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
			blocker := MonitorBlocker{"diagnostics_metadata_invalid", "refresh_report"}
			if err != nil || summary.Status != want || summary.PublicationAuthorized || slices.Contains(summary.Blockers, blocker) == tc.valid {
				t.Fatalf("monitor: %+v %v", summary, err)
			}
			if tc.name == "missing report time" || tc.name == "both times missing" {
				if !slices.Contains(summary.Blockers, MonitorBlocker{"generation_time_missing", "refresh_report"}) {
					t.Fatal(summary.Blockers)
				}
			} else if !tc.valid && !reflect.DeepEqual(summary.Blockers, []MonitorBlocker{blocker}) {
				t.Fatal(summary.Blockers)
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
