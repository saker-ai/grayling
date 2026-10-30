package growth

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"testing"
)

func TestReviewedPreparation(t *testing.T) {
	p, e := proposalFixture()
	r, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
	if err != nil {
		t.Fatal(err)
	}
	in := preparationFixture(p)
	plain, err := CompileCreateRequest(p, e, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CompileReviewedCreateRequest(p, *r, "ws1", "env1", e, in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PreparedExperiment, *plain) || got.ReviewProvenance.Verification.PublicationAuthorized || got.ReviewProvenance.Verification.ProposalFingerprint != r.ProposalFingerprint {
		t.Fatalf("unexpected output: %+v", got)
	}
	again, err := CompileReviewedCreateRequest(p, *r, "ws1", "env1", e, in)
	a, _ := json.Marshal(got, json.Deterministic(true))
	b, _ := json.Marshal(again, json.Deterministic(true))
	if err != nil || string(a) != string(b) {
		t.Fatal("nondeterministic output", err)
	}
	// A proposal review does not pin separate settings: valid changes still
	// compile, while invalid changes retain the original compiler error.
	in.Alpha = 0.04
	if _, err := CompileReviewedCreateRequest(p, *r, "ws1", "env1", e, in); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stale", "pins", "roles", "scope", "reject", "request_changes", "hold", "missing", "settings"} {
		t.Run(name, func(t *testing.T) {
			p, e := proposalFixture()
			r, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			in := preparationFixture(p)
			ws := "ws1"
			switch name {
			case "stale":
				p.Hypothesis += " same version edit"
			case "pins":
				r.EvidencePins[0].Epoch++
			case "roles":
				r.PrimaryMetric, r.Guardrails[0] = r.Guardrails[0], r.PrimaryMetric
			case "scope":
				ws = "other"
			case "reject":
				r.Decision = ReviewReject
			case "request_changes":
				r.Decision = ReviewRequestChanges
			case "hold":
				e.Status = Hold
			case "missing":
				e = nil
			case "settings":
				in.Alpha = 0
			}
			got, err := CompileReviewedCreateRequest(p, *r, ws, "env1", e, in)
			if got != nil || err == nil {
				t.Fatalf("partial or accepted: %+v %v", got, err)
			}
			if name == "hold" || name == "missing" || name == "settings" {
				_, original := CompileCreateRequest(p, e, in)
				if err.Error() != original.Error() {
					t.Fatalf("compiler error changed: %v / %v", err, original)
				}
			} else if !errors.Is(err, ErrReviewVerification) {
				t.Fatal(err)
			}
		})
	}
}
