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

func TestHistoricalPrimaryAttribution(t *testing.T) {
	for _, tc := range []struct {
		name            string
		report, catalog int64
		omitted, valid  bool
	}{
		{"omitted", 0, 86400000000000, true, false},
		{"null", 0, 86400000000000, false, false},
		{"zero", 0, 86400000000000, false, false},
		{"negative", -1, 86400000000000, false, false},
		{"minimum", -9223372036854775808, -9223372036854775808, false, false},
		{"one nanosecond below", 86399999999999, 86400000000000, false, false},
		{"one nanosecond above", 86400000000001, 86400000000000, false, false},
		{"catalog zero", 86400000000000, 0, false, false},
		{"catalog negative", 86400000000000, -1, false, false},
		{"catalog mismatch", 86400000000000, 86400000000001, false, false},
		{"match", 86400000000000, 86400000000000, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, original := proposalFixture()
			approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			report := original.Evidence[0].Report
			original.Metrics[0].AttributionWindow = tc.catalog
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if tc.omitted {
				delete(wire, "primaryAttributionWindow")
			} else if tc.name == "null" {
				wire["primaryAttributionWindow"] = nil
			} else {
				wire["primaryAttributionWindow"] = tc.report
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
			blocker := MonitorBlocker{"primary_attribution_invalid", "review_metric_definitions"}
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
			learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: report.GeneratedAt, Followups: []string{"review attribution registration"}})
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
			result, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: "review attribution"})
			if err != nil || len(result.Matches) != 1 || !reflect.DeepEqual(result.Matches[0].Observation, *summary) {
				t.Fatalf("search: %+v %v", result, err)
			}
			if writes.Load() != 0 {
				t.Fatal("non-GET requests", writes.Load())
			}
		})
	}
}

// Raw report numbers must never pass through float64: the duration bounds and
// one-nanosecond differences remain meaningful even above 2^53.
func TestPrimaryAttributionWireBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, token, lifecycle string
		catalog                int64
		status                 Status
		invalid                bool
	}{
		{"null", "null", "published", 86400000000000, Hold, false},
		{"subhour published", "1800000000001", "published", 1800000000001, Ready, false},
		{"subhour deprecated", "1800000000001", "deprecated", 1800000000001, Ready, false},
		{"max", "9223372036854775807", "published", 9223372036854775807, Ready, false},
		{"max minus nanosecond", "9223372036854775806", "deprecated", 9223372036854775807, Hold, false},
		{"minimum", "-9223372036854775808", "deprecated", -9223372036854775808, Hold, false},
		{"overflow", "9223372036854775808", "published", 1, "", true},
		{"underflow", "-9223372036854775809", "published", 1, "", true},
		{"fraction", "0.5", "published", 1, "", true},
		{"exponent", "1e3", "published", 1000, "", true},
		{"string", `"secret duration"`, "published", 1, "", true},
		{"boolean", "true", "published", 1, "", true},
		{"object", "{}", "published", 1, "", true},
		{"array", "[]", "published", 1, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metric, report, query := fixture()
			metric.AttributionWindow, metric.Status = tc.catalog, tc.lifecycle
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]jsontext.Value
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			wire["primaryAttributionWindow"] = jsontext.Value(tc.token)
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
				json.MarshalWrite(w, map[string]any{"workspaceId": "ws1", "environmentId": "env1", "report": wire})
			}))
			defer srv.Close()
			reader := readerFor(t, srv.URL, nil)
			e, err := reader.Read(t.Context(), query)
			m := MonitorManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, PrimaryMetric: *report.PrimaryMetricRef}
			summary, monitorErr := reader.Monitor(t.Context(), m)
			if tc.invalid {
				if e != nil || summary != nil || !errors.Is(err, ErrResponse) || !errors.Is(monitorErr, ErrResponse) {
					t.Fatalf("invalid number accepted: %+v %+v %v %v", e, summary, err, monitorErr)
				}
			} else {
				if err != nil || monitorErr != nil || e.Status != tc.status || e.Evidence[0].Status != tc.status || summary.Status != tc.status || summary.PublicationAuthorized {
					t.Fatalf("evidence: %+v monitor: %+v %v %v", e, summary, err, monitorErr)
				}
				blocker := MonitorBlocker{"primary_attribution_invalid", "review_metric_definitions"}
				if slices.Contains(summary.Blockers, blocker) != (tc.status == Hold) {
					t.Fatal(summary)
				}
				if tc.status == Ready && e.Evidence[0].Report.PrimaryAttributionWindow != tc.catalog {
					t.Fatal("duration rounded")
				}
			}
			if writes.Load() != 0 {
				t.Fatal("non-GET requests", writes.Load())
			}
		})
	}
}

func TestPrimaryAttributionUsesExactPinnedPrimary(t *testing.T) {
	metric, report, _ := fixture()
	other := metric
	other.Key = "guardrail"
	other.AttributionWindow++
	if !primaryAttributionValid(report, []Metric{other, metric}) {
		t.Fatal("compared guardrail")
	}
	for _, mutate := range []func(*Metric){
		func(m *Metric) { m.Key = "different" },
		func(m *Metric) { m.Version++ },
		func(m *Metric) { m.Hash = hashB },
	} {
		changed := metric
		mutate(&changed)
		if primaryAttributionValid(report, []Metric{changed}) {
			t.Fatal("accepted different pin")
		}
	}
}
