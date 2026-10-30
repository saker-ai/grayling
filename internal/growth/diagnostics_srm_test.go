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

func TestDiagnosticsSRMEvidenceLifecycle(t *testing.T) {
	for _, field := range []string{"assignmentSrm", "exposureSrm", "both", "mixed"} {
		for _, mode := range []string{"onRender", "onDecision"} {
			t.Run(field+"/"+mode, func(t *testing.T) { testDiagnosticsSRMLifecycle(t, field, mode) })
		}
	}
}

func testDiagnosticsSRMLifecycle(t *testing.T, field, mode string) {
	for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			for _, tc := range []struct {
				name, token string
				valid       bool
			}{
				{"omitted", "", true},
				{"null", "null", false}, {"missing healthy", "{}", false},
				{"null healthy", `{"healthy":null}`, false},
				{"false", `{"healthy":false}`, false},
				{"true", `{"healthy":true,"pValue":"secret","observed":["secret"]}`, true},
			} {
				t.Run(method+"/"+verdict+"/"+tc.name, func(t *testing.T) {
					p, original := proposalFixture()
					approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
					if err != nil {
						t.Fatal(err)
					}
					report := original.Evidence[0].Report
					report.AnalysisMethod = method
					report.ExposureMode, report.Diagnostics.ExposureMode = mode, mode
					report.Decision.Verdict = verdict
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
					fields := []string{field}
					if field == "both" || field == "mixed" {
						fields = []string{"assignmentSrm", "exposureSrm"}
					}
					for i, key := range fields {
						token := tc.token
						if field == "mixed" && i == 1 {
							token = `{"healthy":true}`
						}
						if token != "" {
							diagnostics[key] = jsontext.Value(token)
						}
					}
					wire["diagnostics"], err = json.Marshal(diagnostics)
					if err != nil {
						t.Fatal(err)
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
					projected, err := json.Marshal(e.Evidence[0].Report)
					if err != nil || strings.Contains(string(projected), "secret") {
						t.Fatalf("projection: %s %v", projected, err)
					}
					var projectedWire map[string]jsontext.Value
					if err := json.Unmarshal(projected, &projectedWire); err != nil {
						t.Fatal(err)
					}
					var projectedDiagnostics map[string]jsontext.Value
					if err := json.Unmarshal(projectedWire["diagnostics"], &projectedDiagnostics); err != nil {
						t.Fatal(err)
					}
					key := field
					if field == "both" || field == "mixed" {
						key = "assignmentSrm"
					}
					expected := tc.token
					if tc.name == "true" {
						expected = `{"healthy":true}`
					}
					if string(projectedDiagnostics[key]) != expected {
						t.Fatalf("lost diagnostic SRM presence: %s", projected)
					}
					if field == "both" && string(projectedDiagnostics["exposureSrm"]) != expected {
						t.Fatalf("lost exposure SRM presence: %s", projected)
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
					blocker := MonitorBlocker{"srm_review_required", "review_diagnostics"}
					if err != nil || summary.Status != want || summary.ServerVerdict != verdict || summary.PublicationAuthorized || slices.Contains(summary.Blockers, blocker) == tc.valid {
						t.Fatalf("monitor: %+v %v", summary, err)
					}
					if !tc.valid && !reflect.DeepEqual(summary.Blockers, []MonitorBlocker{blocker}) {
						t.Fatal(summary.Blockers)
					}
					fp, err := ProposalFingerprint(p)
					if err != nil {
						t.Fatal(err)
					}
					learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: report.GeneratedAt, Followups: []string{"review srm registration"}})
					if err != nil {
						t.Fatal(err)
					}
					data, err = json.Marshal(learning)
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
					result, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: "review srm"})
					if err != nil || len(result.Matches) != 1 || !reflect.DeepEqual(result.Matches[0].Observation, *summary) {
						t.Fatalf("search: %+v %v", result, err)
					}
					if writes.Load() != 0 {
						t.Fatal("non-GET requests", writes.Load())
					}
				})
			}
		}
	}
}

func TestDiagnosticsSRMMalformedWire(t *testing.T) {
	for _, field := range []string{"assignmentSrm", "exposureSrm", "both"} {
		t.Run(field, func(t *testing.T) { testDiagnosticsSRMMalformedWire(t, field) })
	}
}

func testDiagnosticsSRMMalformedWire(t *testing.T, field string) {
	for _, method := range []string{"two-proportion-z", "bandit-beta-bernoulli"} {
		for _, token := range []string{`true`, `false`, `1`, `"secret"`, `[]`, `{"healthy":"secret"}`, `{"healthy":1}`, `{"healthy":{}}`, `{"healthy":[]}`, `{"healthy":true,"healthy":false}`, `duplicate-member`, `malformed`} {
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
				fields := []string{field}
				if field == "both" {
					fields = []string{"assignmentSrm", "exposureSrm"}
				}
				for _, key := range fields {
					token := tc.token
					if token == "duplicate-member" || token == "malformed" {
						token = `{"healthy":true}`
					}
					diagnostics[key] = jsontext.Value(token)
				}
				wire["diagnostics"], err = json.Marshal(diagnostics, jsontext.AllowDuplicateNames(true))
				if err != nil {
					t.Fatal(err)
				}
				if tc.token == "duplicate-member" {
					base := wire["diagnostics"]
					wire["diagnostics"] = jsontext.Value(string(base[:len(base)-1]) + `,"` + fields[0] + `":{"healthy":false}}`)
				}
				payload, err := json.Marshal(map[string]any{"workspaceId": "ws1", "environmentId": "env1", "report": wire}, jsontext.AllowDuplicateNames(true))
				if err != nil {
					t.Fatal(err)
				}
				if tc.token == "malformed" {
					payload = payload[:len(payload)-3]
				}
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
					if _, err := w.Write(payload); err != nil {
						t.Error(err)
					}
				}))
				defer srv.Close()
				reader := readerFor(t, srv.URL, nil)
				e, err := reader.Read(t.Context(), query)
				m := MonitorManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, PrimaryMetric: *report.PrimaryMetricRef}
				summary, monitorErr := reader.Monitor(t.Context(), m)
				if e != nil || summary != nil || !errors.Is(err, ErrResponse) || !errors.Is(monitorErr, ErrResponse) {
					t.Fatalf("invalid SRM accepted: %+v %+v %v %v", e, summary, err, monitorErr)
				}
				if writes.Load() != 0 {
					t.Fatal("non-GET requests", writes.Load())
				}
			})
		}
	}
}
