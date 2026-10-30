package growth

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

func TestRecordReviewConsumerBlockerBounds(t *testing.T) {
	for _, decision := range []ReviewDecision{ReviewReject, ReviewRequestChanges} {
		for _, name := range []string{"count exact", "count excessive", "field exact", "field excessive", "field control"} {
			t.Run(string(decision)+"/"+name, func(t *testing.T) {
				p, e := proposalFixture()
				valid := name == "count exact" || name == "field exact"
				switch name {
				case "count exact", "count excessive":
					count := MaxReviewBlockers - 1 // Plus the count_out_of_bounds blocker.
					if !valid {
						count++
					}
					p.RollbackConditions = make([]string, count)
				default:
					key := strings.Repeat("v", MaxReviewBlockerTextBytes-len("implementation.variant."))
					if name == "field excessive" {
						key += "x"
					}
					if name == "field control" {
						key = "private\x00variant"
					}
					p.Variants[0].Key = key
				}
				before, _ := json.Marshal(p)
				readiness := ReviewProposal(p, e)
				record, err := RecordReview(p, e, reviewInputs(t, p, decision))
				if !valid {
					requireReviewError(t, record, err, "record", "too_large_or_invalid")
				} else {
					if err != nil || !reflect.DeepEqual(record.Readiness, readiness) {
						t.Fatalf("lost blockers: %v", err)
					}
					assertReviewExportRoundTrip(t, p, record)
				}
				after, _ := json.Marshal(p)
				if !bytes.Equal(before, after) {
					t.Fatal("mutated proposal")
				}
				approved, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
				requireReviewError(t, approved, err, "proposal", "not_ready_for_review")
			})
		}
	}
}

func assertReviewExportRoundTrip(t *testing.T, p Proposal, r *ReviewRecord) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReviewRecord(bytes.NewReader(data))
	if err != nil || !reflect.DeepEqual(r, decoded) {
		t.Fatalf("decode: %v", err)
	}
	verified, err := VerifyReview(p, *decoded, p.WorkspaceID, p.EnvironmentID)
	if err != nil || verified.Decision != r.Decision || verified.RecordedReadiness != r.Readiness.State || verified.PublicationAuthorized {
		t.Fatalf("verify: %+v %v", verified, err)
	}
}

func TestReviewRecordSerializedBound(t *testing.T) {
	p, e := proposalFixture()
	in := reviewInputs(t, p, ReviewReject)
	in.Rationale = "r" + strings.Repeat("\t", MaxReviewRationaleBytes-1)
	r, err := RecordReview(p, e, in)
	if err != nil {
		t.Fatal(err)
	}
	assertReviewExportRoundTrip(t, p, r)
	// Exercise the shared consumer validator's exact serialized boundary. Escaped
	// text is budgeted as JSON bytes rather than as decoded string bytes.
	r.EvidencePins[0].Window.Issues = []string{""}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	r.EvidencePins[0].Window.Issues[0] = strings.Repeat("a", MaxReviewRecordBytes-len(data)-2) + "\t"
	data, err = json.Marshal(r)
	if err != nil || len(data) != MaxReviewRecordBytes || validateReviewRecord(*r) != nil {
		t.Fatalf("exact boundary: %d %v", len(data), err)
	}
	if _, err := DecodeReviewRecord(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	r.EvidencePins[0].Window.Issues[0] += "x"
	if validateReviewRecord(*r) == nil {
		t.Fatal("oversized record accepted")
	}
}
