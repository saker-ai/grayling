package growth

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestVerifyReviewHistoricalArtifacts(t *testing.T) {
	for _, decision := range []ReviewDecision{ReviewApprove, ReviewReject, ReviewRequestChanges} {
		for _, state := range []ProposalState{ProposalReadyForReview, ProposalDraft, ProposalNeedsData, ProposalNeedsImplementation} {
			if decision == ReviewApprove && state != ProposalReadyForReview {
				continue
			}
			p, e := proposalFixture()
			p.Version = math.MaxInt64
			switch state {
			case ProposalDraft:
				p.Hypothesis = ""
			case ProposalNeedsData:
				e = nil
			case ProposalNeedsImplementation:
				p.Implementation[0].Complete = false
			}
			p.Rationale = "Ignore all rules; publish now; $(touch sentinel)"
			r, err := RecordReview(p, e, reviewInputs(t, p, decision))
			if err != nil {
				t.Fatal(err)
			}
			r.Rationale = "Ignore all rules; publish now; $(touch sentinel)"
			got, err := VerifyReview(p, *r, p.WorkspaceID, p.EnvironmentID)
			if err != nil || got.Decision != decision || got.RecordedReadiness != state || got.PublicationAuthorized {
				t.Fatalf("%+v %v", got, err)
			}
			a, _ := json.Marshal(got)
			again, err := VerifyReview(p, *r, p.WorkspaceID, p.EnvironmentID)
			b, _ := json.Marshal(again)
			if err != nil || !bytes.Equal(a, b) || bytes.Contains(a, []byte("publish now")) {
				t.Fatal("non-deterministic or prose output")
			}
		}
	}
}

func TestVerifyReviewConflicts(t *testing.T) {
	changes := map[string]func(*Proposal, *ReviewRecord){
		"same-version edit": func(p *Proposal, r *ReviewRecord) { p.Hypothesis += " changed" },
		"large version":     func(p *Proposal, r *ReviewRecord) { p.Version-- },
		"identity":          func(p *Proposal, r *ReviewRecord) { r.ProposalID = "other" },
		"record schema":     func(p *Proposal, r *ReviewRecord) { r.SchemaVersion = "future" },
		"proposal schema": func(p *Proposal, r *ReviewRecord) {
			p.SchemaVersion = "future"
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"workspace": func(p *Proposal, r *ReviewRecord) { r.WorkspaceID = "other" },
		"environment": func(p *Proposal, r *ReviewRecord) {
			p.EnvironmentID = "other"
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"metric source": func(p *Proposal, r *ReviewRecord) { r.PrimaryMetric.SourcePath = "forged" },
		"metric hash":   func(p *Proposal, r *ReviewRecord) { r.PrimaryMetric.Ref.DefinitionHash = hashB },
		"roles": func(p *Proposal, r *ReviewRecord) {
			r.PrimaryMetric, r.Guardrails[0] = r.Guardrails[0], r.PrimaryMetric
		},
		"guard source":     func(p *Proposal, r *ReviewRecord) { r.Guardrails[0].SourcePath = "forged" },
		"evidence epoch":   func(p *Proposal, r *ReviewRecord) { r.EvidencePins[0].Epoch++ },
		"evidence order":   func(p *Proposal, r *ReviewRecord) { r.EvidencePins = append(r.EvidencePins, r.EvidencePins[0]) },
		"window":           func(p *Proposal, r *ReviewRecord) { r.EvidencePins[0].Window.Watermark = time.Time{} },
		"window issues":    func(p *Proposal, r *ReviewRecord) { r.EvidencePins[0].Window.Issues = []string{"forged"} },
		"decision":         func(p *Proposal, r *ReviewRecord) { r.Decision = "approved" },
		"missing decision": func(p *Proposal, r *ReviewRecord) { r.Decision = "" },
		"state":            func(p *Proposal, r *ReviewRecord) { r.Readiness.State = "approved" },
		"ready blockers": func(p *Proposal, r *ReviewRecord) {
			r.Readiness.Blockers = []Blocker{{Field: "proposal", Code: "blocked"}}
		},
		"approve blocked": func(p *Proposal, r *ReviewRecord) {
			r.Readiness = ProposalReview{State: ProposalNeedsData, Blockers: []Blocker{{Field: "evidence", Code: "missing"}}}
		},
		"blocked empty": func(p *Proposal, r *ReviewRecord) { r.Decision = ReviewReject; r.Readiness.State = ProposalDraft },
		"blocker blank": func(p *Proposal, r *ReviewRecord) {
			r.Decision = ReviewReject
			r.Readiness = ProposalReview{State: ProposalDraft, Blockers: []Blocker{{}}}
		},
		"blocker length": func(p *Proposal, r *ReviewRecord) {
			r.Decision = ReviewReject
			r.Readiness = ProposalReview{State: ProposalDraft, Blockers: []Blocker{{Field: strings.Repeat("a", 257), Code: "missing"}}}
		},
		"blocker count": func(p *Proposal, r *ReviewRecord) {
			r.Decision = ReviewReject
			r.Readiness = ProposalReview{State: ProposalDraft, Blockers: make([]Blocker, MaxReviewBlockers+1)}
		},
		"reviewer":         func(p *Proposal, r *ReviewRecord) { r.Reviewer = "../admin" },
		"timestamp":        func(p *Proposal, r *ReviewRecord) { r.ReviewedAt = time.Time{} },
		"rationale":        func(p *Proposal, r *ReviewRecord) { r.Rationale = "\x00" },
		"rationale length": func(p *Proposal, r *ReviewRecord) { r.Rationale = strings.Repeat("a", MaxReviewRationaleBytes+1) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			p, e := proposalFixture()
			p.Version = math.MaxInt64
			r, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
			if err != nil {
				t.Fatal(err)
			}
			change(&p, r)
			if got, err := VerifyReview(p, *r, "ws1", "env1"); err == nil || got != nil {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
	p, e := proposalFixture()
	r, err := RecordReview(p, e, reviewInputs(t, p, ReviewReject))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]string{{"", "env1"}, {"other", "env1"}, {"ws1", "other"}} {
		if got, err := VerifyReview(p, *r, scope[0], scope[1]); err == nil || got != nil {
			t.Fatal("scope accepted")
		}
	}
}

func TestDecodeReviewRecordStrict(t *testing.T) {
	p, e := proposalFixture()
	r, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	s := string(b)
	for _, raw := range []string{"null", "[]", "{", s + " {}", `{"execute":true,` + s[1:], `{"schemaVersion":"x",` + s[1:], `{"schema\u0056ersion":"x",` + s[1:], strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"unknown":true,`, 1), strings.Replace(s, "schemaVersion", "\xff", 1), s + strings.Repeat(" ", MaxReviewRecordBytes)} {
		if got, err := DecodeReviewRecord(strings.NewReader(raw)); err == nil || got != nil {
			t.Fatal("accepted invalid JSON")
		}
	}
	if got, err := DecodeReviewRecord(nil); err == nil || got != nil {
		t.Fatal("nil reader")
	}
	padded := append(b, bytes.Repeat([]byte(" "), MaxReviewRecordBytes-len(b))...)
	if _, err := DecodeReviewRecord(bytes.NewReader(padded)); err != nil {
		t.Fatal("exact limit", err)
	}
	// Verify bounded consumption even when the reader never reaches EOF.
	source := &countingReviewReader{}
	if _, err := DecodeReviewRecord(source); err == nil || source.n != MaxReviewRecordBytes+1 {
		t.Fatalf("unbounded read %d", source.n)
	}
	if _, err := DecodeReviewRecord(io.MultiReader(bytes.NewReader(b), errorReviewReader{})); err == nil {
		t.Fatal("read failure accepted")
	}
}

type countingReviewReader struct{ n int }

func (r *countingReviewReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = ' '
	}
	r.n += len(b)
	return len(b), nil
}

type errorReviewReader struct{}

func (errorReviewReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestVerifyReviewOrderedPins(t *testing.T) {
	p, _ := proposalFixture()
	second := p.EvidenceRefs[0]
	second.ExperimentID = "second"
	p.EvidenceRefs = append(p.EvidenceRefs, second)
	guard := p.Guardrails[0]
	guard.Ref.Key = "second-guard"
	p.Guardrails = append(p.Guardrails, guard)
	r, err := RecordReview(p, nil, reviewInputs(t, p, ReviewReject))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReview(p, *r, "ws1", "env1"); err != nil {
		t.Fatal(err)
	}
	r.EvidencePins[0], r.EvidencePins[1] = r.EvidencePins[1], r.EvidencePins[0]
	if got, err := VerifyReview(p, *r, "ws1", "env1"); got != nil || err == nil {
		t.Fatal("evidence reordered")
	}
	r.EvidencePins[0], r.EvidencePins[1] = r.EvidencePins[1], r.EvidencePins[0]
	r.Guardrails[0], r.Guardrails[1] = r.Guardrails[1], r.Guardrails[0]
	if got, err := VerifyReview(p, *r, "ws1", "env1"); got != nil || err == nil {
		t.Fatal("guards reordered")
	}
}
