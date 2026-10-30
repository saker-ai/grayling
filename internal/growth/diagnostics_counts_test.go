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

func TestDiagnosticsCountsEvidenceLifecycle(t *testing.T) {
	for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			for _, tc := range []struct {
				name, token string
				valid       bool
			}{
				{"missing-cross", `{"assignmentMismatch":0}`, false},
				{"missing-assignment", `{"crossVariantUnits":0}`, false},
				{"null-cross", `{"crossVariantUnits":null,"assignmentMismatch":0}`, false},
				{"null-assignment", `{"crossVariantUnits":0,"assignmentMismatch":null}`, false},
				{"positive-cross", `{"crossVariantUnits":1,"assignmentMismatch":0}`, false},
				{"positive-assignment", `{"crossVariantUnits":0,"assignmentMismatch":1}`, false},
				{"negative-cross", `{"crossVariantUnits":-1,"assignmentMismatch":0}`, false},
				{"negative-assignment", `{"crossVariantUnits":0,"assignmentMismatch":-1}`, false},
				{"max-cross", `{"crossVariantUnits":9223372036854775807,"assignmentMismatch":0}`, false},
				{"min-cross", `{"crossVariantUnits":-9223372036854775808,"assignmentMismatch":0}`, false},
				{"max-assignment", `{"crossVariantUnits":0,"assignmentMismatch":9223372036854775807}`, false},
				{"min-assignment", `{"crossVariantUnits":0,"assignmentMismatch":-9223372036854775808}`, false},
				{"zero", `{"crossVariantUnits":0,"assignmentMismatch":0,"unitIds":["secret"]}`, true},
			} {
				t.Run(method+"/"+verdict+"/"+tc.name, func(t *testing.T) {
					p, original := proposalFixture()
					approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
					if err != nil {
						t.Fatal(err)
					}
					report := original.Evidence[0].Report
					report.AnalysisMethod = method
					report.Decision.Verdict = verdict
					data, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					var wire map[string]jsontext.Value
					if err := json.Unmarshal(data, &wire); err != nil {
						t.Fatal(err)
					}
					var counts map[string]jsontext.Value
					if err := json.Unmarshal([]byte(tc.token), &counts); err != nil {
						t.Fatal(err)
					}
					var diagnostics map[string]jsontext.Value
					if err := json.Unmarshal(wire["diagnostics"], &diagnostics); err != nil {
						t.Fatal(err)
					}
					delete(diagnostics, "crossVariantUnits")
					delete(diagnostics, "assignmentMismatch")
					for k, v := range counts {
						diagnostics[k] = v
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
						json.MarshalWrite(w, map[string]any{"workspaceId": p.WorkspaceID, "environmentId": p.EnvironmentID, "report": wire}, jsontext.AllowDuplicateNames(true))
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
					for _, key := range []string{"crossVariantUnits", "assignmentMismatch"} {
						want := string(counts[key])
						if want == "" {
							want = "null"
						}
						if string(projectedDiagnostics[key]) != want {
							t.Fatalf("lost exact count %s: %s", key, projected)
						}
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
					blocker := MonitorBlocker{"health_review_required", "review_diagnostics"}
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
					learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: report.GeneratedAt, Followups: []string{"review counts registration"}})
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
					result, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: "review counts"})
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

func TestDiagnosticsCountsMalformedWire(t *testing.T) {
	for _, method := range []string{"two-proportion-z", "bandit-beta-bernoulli"} {
		for _, field := range []string{"crossVariantUnits", "assignmentMismatch"} {
			for _, value := range []string{`true`, `false`, `"secret"`, `[]`, `{}`, `0.5`, `9223372036854775808`, `-9223372036854775809`, `0,"` + field + `":1`} {
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
					delete(diagnostics, "crossVariantUnits")
					delete(diagnostics, "assignmentMismatch")
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
						json.MarshalWrite(w, map[string]any{"workspaceId": "ws1", "environmentId": "env1", "report": wire}, jsontext.AllowDuplicateNames(true))
					}))
					defer srv.Close()
					reader := readerFor(t, srv.URL, nil)
					e, err := reader.Read(t.Context(), query)
					m := MonitorManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, PrimaryMetric: *report.PrimaryMetricRef}
					summary, monitorErr := reader.Monitor(t.Context(), m)
					if e != nil || summary != nil || !errors.Is(err, ErrResponse) || !errors.Is(monitorErr, ErrResponse) {
						t.Fatalf("invalid counts accepted: %+v %+v %v %v", e, summary, err, monitorErr)
					}
					if writes.Load() != 0 {
						t.Fatal("non-GET requests", writes.Load())
					}
				})
			}
		}
	}
}
