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

func TestRejectedReasonsEvidenceLifecycle(t *testing.T) {
	for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			for _, tc := range []struct {
				name, token string
				valid       bool
			}{
				{"omitted", `{}`, true},
				{"null", `{"rejectedByReason":null}`, true},
				{"empty", `{"rejectedByReason":{}}`, true},
				{"duplicates-only", `{"duplicateEvents":999}`, true},
				{"unknown", `{"rejectedByReason":{"secret":0}}`, false},
				{"duplicate-reason", `{"rejectedByReason":{"duplicate_event":0}}`, false},
				{"mixed", `{"rejectedByReason":{"late_event":0,"future_event":1}}`, false},
				{"invalid_event/0", `{"rejectedByReason":{"invalid_event":0}}`, true},
				{"invalid_event/null", `{"rejectedByReason":{"invalid_event":null}}`, false},
				{"invalid_event/1", `{"rejectedByReason":{"invalid_event":1}}`, false},
				{"invalid_event/-1", `{"rejectedByReason":{"invalid_event":-1}}`, false},
				{"invalid_event/9223372036854775807", `{"rejectedByReason":{"invalid_event":9223372036854775807}}`, false},
				{"invalid_event/-9223372036854775808", `{"rejectedByReason":{"invalid_event":-9223372036854775808}}`, false},
				{"unknown_experiment/0", `{"rejectedByReason":{"unknown_experiment":0}}`, true},
				{"unknown_experiment/null", `{"rejectedByReason":{"unknown_experiment":null}}`, false},
				{"unknown_experiment/1", `{"rejectedByReason":{"unknown_experiment":1}}`, false},
				{"unknown_experiment/-1", `{"rejectedByReason":{"unknown_experiment":-1}}`, false},
				{"unknown_experiment/9223372036854775807", `{"rejectedByReason":{"unknown_experiment":9223372036854775807}}`, false},
				{"unknown_experiment/-9223372036854775808", `{"rejectedByReason":{"unknown_experiment":-9223372036854775808}}`, false},
				{"stale_epoch/0", `{"rejectedByReason":{"stale_epoch":0}}`, true},
				{"stale_epoch/null", `{"rejectedByReason":{"stale_epoch":null}}`, false},
				{"stale_epoch/1", `{"rejectedByReason":{"stale_epoch":1}}`, false},
				{"stale_epoch/-1", `{"rejectedByReason":{"stale_epoch":-1}}`, false},
				{"stale_epoch/9223372036854775807", `{"rejectedByReason":{"stale_epoch":9223372036854775807}}`, false},
				{"stale_epoch/-9223372036854775808", `{"rejectedByReason":{"stale_epoch":-9223372036854775808}}`, false},
				{"assignment_mismatch/0", `{"rejectedByReason":{"assignment_mismatch":0}}`, true},
				{"assignment_mismatch/null", `{"rejectedByReason":{"assignment_mismatch":null}}`, false},
				{"assignment_mismatch/1", `{"rejectedByReason":{"assignment_mismatch":1}}`, false},
				{"assignment_mismatch/-1", `{"rejectedByReason":{"assignment_mismatch":-1}}`, false},
				{"assignment_mismatch/9223372036854775807", `{"rejectedByReason":{"assignment_mismatch":9223372036854775807}}`, false},
				{"assignment_mismatch/-9223372036854775808", `{"rejectedByReason":{"assignment_mismatch":-9223372036854775808}}`, false},
				{"missing_exposure/0", `{"rejectedByReason":{"missing_exposure":0}}`, true},
				{"missing_exposure/null", `{"rejectedByReason":{"missing_exposure":null}}`, false},
				{"missing_exposure/1", `{"rejectedByReason":{"missing_exposure":1}}`, false},
				{"missing_exposure/-1", `{"rejectedByReason":{"missing_exposure":-1}}`, false},
				{"missing_exposure/9223372036854775807", `{"rejectedByReason":{"missing_exposure":9223372036854775807}}`, false},
				{"missing_exposure/-9223372036854775808", `{"rejectedByReason":{"missing_exposure":-9223372036854775808}}`, false},
				{"missing_unit/0", `{"rejectedByReason":{"missing_unit":0}}`, true},
				{"missing_unit/null", `{"rejectedByReason":{"missing_unit":null}}`, false},
				{"missing_unit/1", `{"rejectedByReason":{"missing_unit":1}}`, false},
				{"missing_unit/-1", `{"rejectedByReason":{"missing_unit":-1}}`, false},
				{"missing_unit/9223372036854775807", `{"rejectedByReason":{"missing_unit":9223372036854775807}}`, false},
				{"missing_unit/-9223372036854775808", `{"rejectedByReason":{"missing_unit":-9223372036854775808}}`, false},
				{"invalid_exposure_token/0", `{"rejectedByReason":{"invalid_exposure_token":0}}`, true},
				{"invalid_exposure_token/null", `{"rejectedByReason":{"invalid_exposure_token":null}}`, false},
				{"invalid_exposure_token/1", `{"rejectedByReason":{"invalid_exposure_token":1}}`, false},
				{"invalid_exposure_token/-1", `{"rejectedByReason":{"invalid_exposure_token":-1}}`, false},
				{"invalid_exposure_token/9223372036854775807", `{"rejectedByReason":{"invalid_exposure_token":9223372036854775807}}`, false},
				{"invalid_exposure_token/-9223372036854775808", `{"rejectedByReason":{"invalid_exposure_token":-9223372036854775808}}`, false},
				{"expired_exposure_token/0", `{"rejectedByReason":{"expired_exposure_token":0}}`, true},
				{"expired_exposure_token/null", `{"rejectedByReason":{"expired_exposure_token":null}}`, false},
				{"expired_exposure_token/1", `{"rejectedByReason":{"expired_exposure_token":1}}`, false},
				{"expired_exposure_token/-1", `{"rejectedByReason":{"expired_exposure_token":-1}}`, false},
				{"expired_exposure_token/9223372036854775807", `{"rejectedByReason":{"expired_exposure_token":9223372036854775807}}`, false},
				{"expired_exposure_token/-9223372036854775808", `{"rejectedByReason":{"expired_exposure_token":-9223372036854775808}}`, false},
				{"late_event/0", `{"rejectedByReason":{"late_event":0}}`, true},
				{"late_event/null", `{"rejectedByReason":{"late_event":null}}`, false},
				{"late_event/1", `{"rejectedByReason":{"late_event":1}}`, false},
				{"late_event/-1", `{"rejectedByReason":{"late_event":-1}}`, false},
				{"late_event/9223372036854775807", `{"rejectedByReason":{"late_event":9223372036854775807}}`, false},
				{"late_event/-9223372036854775808", `{"rejectedByReason":{"late_event":-9223372036854775808}}`, false},
				{"future_event/0", `{"rejectedByReason":{"future_event":0}}`, true},
				{"future_event/null", `{"rejectedByReason":{"future_event":null}}`, false},
				{"future_event/1", `{"rejectedByReason":{"future_event":1}}`, false},
				{"future_event/-1", `{"rejectedByReason":{"future_event":-1}}`, false},
				{"future_event/9223372036854775807", `{"rejectedByReason":{"future_event":9223372036854775807}}`, false},
				{"future_event/-9223372036854775808", `{"rejectedByReason":{"future_event":-9223372036854775808}}`, false},
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
					delete(diagnostics, "rejectedByReason")
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
					var projectedCounts map[string]*int64
					if json.Unmarshal(projectedDiagnostics["rejectedByReason"], &projectedCounts) != nil && len(projectedDiagnostics["rejectedByReason"]) != 0 {
						t.Fatal("invalid projected counts")
					}
					unknown := tc.name == "unknown" || tc.name == "duplicate-reason"
					if e.Evidence[0].Report.Diagnostics.UnknownRejectionReasons != unknown {
						t.Fatal("lost unknown reason indicator")
					}
					if !unknown && len(counts["rejectedByReason"]) > 0 && string(counts["rejectedByReason"]) != "null" {
						var supplied map[string]*int64
						if json.Unmarshal(counts["rejectedByReason"], &supplied) != nil || !reflect.DeepEqual(projectedCounts, supplied) && len(supplied) != 0 {
							t.Fatalf("lost exact nullable count: %s", projected)
						}
					}
					if strings.Contains(string(projected), "duplicate_event") {
						t.Fatal("unknown reason exported")
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

func TestRejectedReasonsMalformedWire(t *testing.T) {
	for _, method := range []string{"two-proportion-z", "bandit-beta-bernoulli"} {
		for _, field := range []string{"late_event", "secret"} {
			for _, value := range []string{`true`, `false`, `"secret"`, `[]`, `{}`, `0.5`, `1e0`, `9223372036854775808`, `-9223372036854775809`, `0,"` + field + `":1`} {
				token := `{"rejectedByReason":{"` + field + `":` + value + `}}`
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
					delete(diagnostics, "rejectedByReason")
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
