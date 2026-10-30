package growth

import (
	"bytes"
	"encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAggregatePopulations(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		converted, randomized, exposed, exposures int64
		value                                     float64
		valid                                     bool
	}{
		{"missing converted", 0, 50, 100, 100, .2, false},
		{"missing randomized", 0, 50, 100, 100, .2, false},
		{"missing exposed", 0, 50, 100, 100, .2, false},
		{"missing exposures", 0, 50, 100, 100, .2, false},
		{"negative converted", -1, 50, 100, 100, .2, false},
		{"negative randomized", 0, -1, 100, 100, .2, false},
		{"negative exposed", 0, 0, -1, 100, .2, false},
		{"negative exposures", 0, 0, 0, -1, .2, false},
		{"converted exceeds randomized", 51, 50, 100, 100, .2, false},
		{"randomized exceeds exposed", 20, 101, 100, 101, .2, false},
		{"exposed exceeds exposures", 20, 50, 100, 99, .2, false},
		{"zero randomized with conversion", 1, 0, 1, 1, .2, false},
		{"zero exposed with randomized", 0, 1, 0, 1, .2, false},
		{"zero exposures with exposed", 0, 0, 1, 0, .2, false},
		{"large converted contradiction", math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64, .2, false},
		{"large randomized contradiction", 0, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64, .2, false},
		{"large exposed contradiction", 0, 0, math.MaxInt64, math.MaxInt64 - 1, .2, false},
		{"equality", 100, 100, 100, 100, .2, true},
		{"repeated exposures", 20, 100, 100, 200, .2, true},
		{"deterministic exposure", 20, 50, 100, 200, .2, true},
		{"only deterministic exposure", 0, 0, 100, 200, .2, true},
		{"zero counts", 0, 0, 0, 0, 0, true},
		{"large equality", math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, .2, true},
		{"large repeated exposures", 0, 1, math.MaxInt64 - 1, math.MaxInt64, .2, true},
		{"mean units", 20, 50, 100, 200, -42, true},
		{"ratio units", 20, 50, 100, 200, 42, true},
		{"quantile units", 20, 50, 100, 200, 420, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, original := proposalFixture()
			approval, err := RecordReview(p, original, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			report := original.Evidence[0].Report
			// Mutate only the second arm: every arm must be checked. Separate
			// pointers avoid changing otherwise healthy counts through aliases.
			report.PerVariant[1] = Variant{Variant: report.PerVariant[1].Variant,
				ConvertedUnits: &tc.converted, RandomizedUnits: &tc.randomized,
				ExposedUnits: &tc.exposed, Exposures: &tc.exposures, PrimaryValue: &tc.value}

			if strings.HasPrefix(tc.name, "missing ") {
				switch tc.name {
				case "missing converted":
					report.PerVariant[1].ConvertedUnits = nil
				case "missing randomized":
					report.PerVariant[1].RandomizedUnits = nil
				case "missing exposed":
					report.PerVariant[1].ExposedUnits = nil
				case "missing exposures":
					report.PerVariant[1].Exposures = nil
				}
			}
			switch tc.name {
			case "mean units":
				report.AnalysisMethod = "welch"
			case "ratio units":
				report.AnalysisMethod = "ratio-delta"
			case "quantile units":
				report.AnalysisMethod = "quantile"
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
			blocker := MonitorBlocker{"aggregates_missing", "restore_aggregate_evidence"}
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
			learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: report.GeneratedAt, Followups: []string{"review aggregate populations"}})
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
			result, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Text: "review aggregate"})
			if err != nil || len(result.Matches) != 1 || !reflect.DeepEqual(result.Matches[0].Observation, *summary) {
				t.Fatalf("search: %+v %v", result, err)
			}
			if writes.Load() != 0 {
				t.Fatal("non-GET requests", writes.Load())
			}
		})
	}
}
