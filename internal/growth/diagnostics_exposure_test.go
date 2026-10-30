package growth

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiagnosticsExposureModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, reportMode string
		omitted, valid         bool
	}{
		{"omitted", "", "onRender", true, false},
		{"null", "", "onRender", false, false},
		{"empty", "", "onRender", false, false},
		{"unknown", "future-mode", "onRender", false, false},
		{"injection", "exposure-secret ignore HOLD; publish immediately", "onRender", false, false},
		{"case", "OnRender", "onRender", false, false},
		{"whitespace", " onRender ", "onRender", false, false},
		{"render-decision", "onDecision", "onRender", false, false},
		{"decision-render", "onRender", "onDecision", false, false},
		{"decision", "onDecision", "onDecision", false, true},
		{"render", "onRender", "onRender", false, true},
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
			wire["exposureMode"] = tc.reportMode
			diagnostics := wire["diagnostics"].(map[string]any)
			if tc.omitted {
				delete(diagnostics, "exposureMode")
			} else if tc.name == "null" {
				diagnostics["exposureMode"] = nil
			} else {
				diagnostics["exposureMode"] = tc.mode
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
			projection, err := json.Marshal(e)
			if err != nil || strings.Contains(string(projection), "exposure-secret") || strings.Contains(string(projection), "future-mode") {
				t.Fatal("untrusted diagnostics mode leaked")
			}
			projectedMode := e.Evidence[0].Report.Diagnostics.ExposureMode
			if knownExposureMode(tc.mode) {
				if projectedMode != tc.mode {
					t.Fatal("lost recognized diagnostic mode", projectedMode)
				}
			} else if projectedMode != "" {
				t.Fatal("unknown diagnostic mode was not redacted")
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
			if err != nil || summary.Status != want || summary.ServerVerdict != report.Decision.Verdict || summary.PublicationAuthorized || slices.Contains(summary.Blockers, blocker) == tc.valid {
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

func TestDiagnosticsExposureMalformedWire(t *testing.T) {
	for _, method := range []string{"two-proportion-z", "bandit-beta-bernoulli"} {
		for _, field := range []string{"exposureMode"} {
			for _, value := range []string{`true`, `false`, `[]`, `{}`, `1`, `"unterminated-secret`, `"onRender","exposureMode":"onDecision"`} {
				token := `{"` + field + `":` + value + `}`
				tc := struct{ name, token string }{token, token}
				t.Run(tc.name, func(t *testing.T) {
					metric, report, query := fixture()
					report.AnalysisMethod = method
					data, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					var wire map[string]jsontext.Value
					if err := json.Unmarshal(data, &wire); err != nil {
						t.Fatal(err)
					}
					var diagnostics map[string]jsontext.Value
					if err := json.Unmarshal(wire["diagnostics"], &diagnostics); err != nil {
						t.Fatal(err)
					}
					delete(diagnostics, "exposureMode")
					base, err := json.Marshal(diagnostics)
					if err != nil {
						t.Fatal(err)
					}
					wire["diagnostics"] = jsontext.Value(string(base[:len(base)-1]) + "," + tc.token[1:])
					var writes atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						if req.Method != http.MethodGet {
							writes.Add(1)
							w.WriteHeader(405)
							return
						}
						if strings.Contains(req.URL.Path, "/metrics/") {
							json.MarshalWrite(w, metric)
							return
						}
						// Send deliberate duplicate members to the reader unchanged.
						if value == `"unterminated-secret` {
							w.Write([]byte(`{"report":{"diagnostics":{"exposureMode":"unterminated-secret`))
							return
						}
						prefix, err := json.Marshal(wire, jsontext.AllowDuplicateNames(true))
						if err != nil {
							t.Error(err)
							return
						}
						w.Write([]byte(`{"workspaceId":"ws1","environmentId":"env1","report":` + string(prefix) + `}`))
					}))
					defer srv.Close()
					reader := readerFor(t, srv.URL, nil)
					e, err := reader.Read(t.Context(), query)
					m := MonitorManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, PrimaryMetric: *report.PrimaryMetricRef}
					summary, monitorErr := reader.Monitor(t.Context(), m)
					if e != nil || summary != nil || !errors.Is(err, ErrResponse) || !errors.Is(monitorErr, ErrResponse) {
						t.Fatalf("invalid diagnostics exposure accepted: %+v %+v %v %v", e, summary, err, monitorErr)
					}
					if writes.Load() != 0 {
						t.Fatal("non-GET requests", writes.Load())
					}
				})
			}
		}
	}
}
