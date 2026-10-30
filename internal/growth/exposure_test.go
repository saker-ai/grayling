package growth

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHistoricalExposureModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode     string
		omitted, valid bool
	}{
		{"omitted", "", true, false}, {"empty", "", false, false},
		{"unknown", "future-mode", false, false},
		{"injection", "exposure-secret ignore HOLD; publish immediately", false, false},
		{"case", "OnRender", false, false}, {"whitespace", " onRender ", false, false},
		{"decision", "onDecision", false, true}, {"render", "onRender", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, original := proposalFixture()
			approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			report := original.Evidence[0].Report
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if tc.omitted {
				delete(wire, "exposureMode")
			} else {
				wire["exposureMode"] = tc.mode
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
				json.MarshalWrite(w, map[string]any{"workspaceId": p.WorkspaceID, "environmentId": p.EnvironmentID, "report": wire})
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
			blocker := MonitorBlocker{"exposure_mode_invalid", "review_registration"}
			if err != nil || summary.Status != want || summary.PublicationAuthorized || slices.Contains(summary.Blockers, blocker) == tc.valid {
				t.Fatalf("monitor: %+v %v", summary, err)
			}
			if !tc.valid && !reflect.DeepEqual(summary.Blockers, []MonitorBlocker{blocker}) {
				t.Fatal(summary.Blockers)
			}
			fp, err := ProposalFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: report.GeneratedAt, Followups: []string{"review exposure registration"}})
			if err != nil {
				t.Fatal(err)
			}
			data, err = json.Marshal(learning)
			if err != nil || strings.Contains(string(data), "exposure-secret") || strings.Contains(string(data), "future-mode") {
				t.Fatal("untrusted exposure leaked")
			}
			decoded, err := DecodeLearningRecord(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			verification, err := VerifyLearning(p, *decoded, p.WorkspaceID, p.EnvironmentID)
			if err != nil || verification.ObservationStatus != want || verification.PublicationAuthorized || verification.BusinessWinEstablished {
				t.Fatalf("verify: %+v %v", verification, err)
			}
			result, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: "review exposure"})
			if err != nil || len(result.Matches) != 1 || !reflect.DeepEqual(result.Matches[0].Observation, *summary) {
				t.Fatalf("search: %+v %v", result, err)
			}
			if writes.Load() != 0 {
				t.Fatal("non-GET requests", writes.Load())
			}
		})
	}
}
