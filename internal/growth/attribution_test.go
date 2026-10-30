package growth

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProposalAttribution(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		hours, primary, guard int64
		state                 ProposalState
		blocker               Blocker
	}{
		{"match", 24, int64(24 * time.Hour), int64(24 * time.Hour), ProposalReadyForReview, Blocker{}},
		{"missing claim", 0, int64(24 * time.Hour), int64(time.Hour), ProposalDraft, Blocker{"analysisPlan.attributionWindowHours", "positive_required"}},
		{"overflow claim", math.MaxInt64, int64(24 * time.Hour), int64(time.Hour), ProposalDraft, Blocker{"analysisPlan.attributionWindowHours", "invalid_duration"}},
		// This value wraps back to 24 hours under unchecked multiplication.
		{"wrapping claim", 24 + (1 << 53), int64(24 * time.Hour), int64(time.Hour), ProposalDraft, Blocker{"analysisPlan.attributionWindowHours", "invalid_duration"}},
		{"missing catalog", 24, 0, int64(time.Hour), ProposalNeedsData, Blocker{"primaryMetric.attributionWindow", "invalid_duration"}},
		{"invalid catalog", 24, -1, int64(time.Hour), ProposalNeedsData, Blocker{"primaryMetric.attributionWindow", "invalid_duration"}},
		{"mismatch", 24, int64(48 * time.Hour), int64(time.Hour), ProposalNeedsData, Blocker{"analysisPlan.attributionWindowHours", "metric_attribution_mismatch"}},
		{"sub hour", 1, int64(30 * time.Minute), int64(time.Hour), ProposalNeedsData, Blocker{"analysisPlan.attributionWindowHours", "metric_attribution_mismatch"}},
		{"fractional hour", 24, int64(24*time.Hour + time.Nanosecond), int64(time.Hour), ProposalNeedsData, Blocker{"analysisPlan.attributionWindowHours", "metric_attribution_mismatch"}},
		{"missing guardrail", 24, int64(24 * time.Hour), 0, ProposalNeedsData, Blocker{"guardrails[0].attributionWindow", "invalid_duration"}},
		{"long guardrail", 24, int64(24 * time.Hour), int64(72 * time.Hour), ProposalReadyForReview, Blocker{}},
		{"sub hour guardrail", 24, int64(24 * time.Hour), int64(time.Minute), ProposalReadyForReview, Blocker{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, e := proposalFixture()
			p.AnalysisPlan.AttributionWindowHours = tc.hours
			e.Metrics[0].AttributionWindow = tc.primary
			e.Metrics[1].AttributionWindow = tc.guard
			r := ReviewProposal(p, e)
			if r.State != tc.state || (tc.blocker.Code != "" && !slices.Contains(r.Blockers, tc.blocker)) {
				t.Fatal(r)
			}
			record, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
			if (err == nil) != (tc.state == ProposalReadyForReview) {
				t.Fatalf("approval: %+v %v", record, err)
			}
			in := preparationFixture(p)
			got, err := CompileCreateRequest(p, e, in)
			blocked := tc.state != ProposalReadyForReview || tc.guard > int64(48*time.Hour)
			if (err != nil) != blocked || (got == nil) != blocked {
				t.Fatalf("prepare: %+v %v", got, err)
			}
			if tc.name == "long guardrail" {
				in.AnalysisWindowHours = 72
				if _, err := CompileCreateRequest(p, e, in); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReaderAttributionWire(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		duration    int64
		invalid     bool
	}{
		{"missing", "", 0, false},
		{"zero", "0", 0, false},
		{"negative", "-1", -1, false},
		{"sub-hour", "1800000000000", int64(30 * time.Minute), false},
		{"max", "9223372036854775807", math.MaxInt64, false},
		{"overflow", "9223372036854775808", 0, true},
		{"string", `"24h"`, 0, true},
		{"fraction", "0.5", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, q := fixture()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					t.Error("write attempted")
				}
				if strings.Contains(req.URL.Path, "/metrics/") {
					wire := fmt.Sprintf(`{"workspaceId":%q,"key":%q,"version":%d,"hash":%q,"status":"published"`, m.WorkspaceID, m.Key, m.Version, m.Hash)
					if tc.token != "" {
						wire += `,"attributionWindow":` + tc.token
					}
					io.WriteString(w, wire+`}`)
					return
				}
				json.MarshalWrite(w, struct {
					WorkspaceID   string `json:"workspaceId"`
					EnvironmentID string `json:"environmentId"`
					Report        Report `json:"report"`
				}{"ws1", "env1", r})
			}))
			defer srv.Close()
			reader := readerFor(t, srv.URL, nil)
			e, err := reader.Read(t.Context(), q)
			if tc.invalid {
				if !errors.Is(err, ErrResponse) || e != nil {
					t.Fatalf("%+v %v", e, err)
				}
				return
			}
			if err != nil || e.Status != Ready || e.Metrics[0].AttributionWindow != tc.duration {
				t.Fatalf("historical evidence changed: %+v %v", e, err)
			}
		})
	}
}
