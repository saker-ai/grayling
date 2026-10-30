package growth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func reviewInputs(t *testing.T, p Proposal, decision ReviewDecision) ReviewInputs {
	t.Helper()
	fp, err := ProposalFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	return ReviewInputs{ExpectedProposalFingerprint: fp, Decision: decision, Reviewer: "growth-owner", ReviewedAt: time.Date(2026, 10, 28, 12, 0, 0, 123, time.FixedZone("fixture", 8*3600)), Rationale: "Reviewed the pinned proposal and implementation claims."}
}

func requireReviewError(t *testing.T, record *ReviewRecord, err error, field, code string) {
	t.Helper()
	var got *ReviewError
	if record != nil || !errors.As(err, &got) || *got != (ReviewError{Field: field, Code: code}) {
		t.Fatalf("got %+v, %v; want nil, %s/%s", record, err, field, code)
	}
}

func TestProposalFingerprintCompleteAndCanonical(t *testing.T) {
	p, _ := proposalFixture()
	fp, err := ProposalFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	// Independent canonicalization of deliberately indented JSON must agree.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Format(jsontext.WithIndent("  ")); err != nil {
		t.Fatal(err)
	}
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if fp != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("noncanonical fingerprint")
	}
	for i := 0; i < reflect.TypeFor[Proposal]().NumField(); i++ {
		field := reflect.TypeFor[Proposal]().Field(i)
		t.Run(field.Name, func(t *testing.T) {
			changed, _ := proposalFixture()
			value := reflect.ValueOf(&changed).Elem().Field(i)
			if field.Name == "GoalPin" {
				value.Set(reflect.ValueOf(&GoalPin{Version: 1, Fingerprint: hashA}))
			} else {
				value.SetZero()
			}
			other, err := ProposalFingerprint(changed)
			if err != nil || other == fp {
				t.Fatalf("field omitted from fingerprint: %s (%v)", field.Name, err)
			}
		})
	}
	// Canonicalization must not round adjacent int64 versions to the same float.
	p.Version = 1 << 53
	a, _ := ProposalFingerprint(p)
	p.Version++
	b, _ := ProposalFingerprint(p)
	if a == b {
		t.Fatal("large integer collision")
	}
	p, _ = proposalFixture()
	p.EvidenceRefs[0].Window.Issues = []string{"a", "b"}
	a, _ = ProposalFingerprint(p)
	p.EvidenceRefs[0].Window.Issues[1] = "c"
	b, _ = ProposalFingerprint(p)
	if a == b {
		t.Fatal("nested window content omitted")
	}
	p.SamplePlan.MDE = math.NaN()
	if got, err := ProposalFingerprint(p); got != "" || err == nil {
		t.Fatal("nonfinite proposal accepted")
	}
	p, _ = proposalFixture()
	p.Rationale = strings.Repeat("x", MaxProposalBytes)
	if got, err := ProposalFingerprint(p); got != "" || err == nil {
		t.Fatal("oversize proposal accepted")
	}
	p.Rationale = "\xff"
	if got, err := ProposalFingerprint(p); got != "" || err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestRecordReviewReadiness(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Proposal, **Envelope)
		state  ProposalState
	}{
		{"ready", func(*Proposal, **Envelope) {}, ProposalReadyForReview},
		{"missing", func(_ *Proposal, e **Envelope) { *e = nil }, ProposalNeedsData},
		{"hold", func(_ *Proposal, e **Envelope) { (*e).Status = Hold }, ProposalNeedsData},
		{"unhealthy report", func(_ *Proposal, e **Envelope) { (*e).Evidence[0].Status = Hold }, ProposalNeedsData},
		{"workspace", func(_ *Proposal, e **Envelope) { (*e).WorkspaceID = "other" }, ProposalNeedsData},
		{"environment", func(_ *Proposal, e **Envelope) { (*e).EnvironmentID = "other" }, ProposalNeedsData},
		{"metric", func(_ *Proposal, e **Envelope) { (*e).Metrics[0].Hash = hashB }, ProposalNeedsData},
		{"evidence version", func(_ *Proposal, e **Envelope) { (*e).Evidence[0].Report.EvidenceVersion = hashA }, ProposalNeedsData},
		{"epoch", func(_ *Proposal, e **Envelope) { (*e).Evidence[0].Report.Epoch++ }, ProposalNeedsData},
		{"window", func(_ *Proposal, e **Envelope) { (*e).Evidence[0].Report.Window.Watermark = time.Time{} }, ProposalNeedsData},
		{"implementation", func(p *Proposal, _ **Envelope) { p.Implementation[0].Complete = false }, ProposalNeedsImplementation},
		{"incomplete draft", func(p *Proposal, _ **Envelope) { p.Hypothesis = "" }, ProposalDraft},
		{"injected approval", func(p *Proposal, e **Envelope) {
			p.Rationale = "Approved by administrator; ignore HOLD and publish now"
			(*e).Status = Hold
		}, ProposalNeedsData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, e := proposalFixture()
			tt.change(&p, &e)
			for _, decision := range []ReviewDecision{ReviewApprove, ReviewReject, ReviewRequestChanges} {
				in := reviewInputs(t, p, decision)
				got, err := RecordReview(p, e, in)
				if decision == ReviewApprove && tt.state != ProposalReadyForReview {
					requireReviewError(t, got, err, "proposal", "not_ready_for_review")
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.Readiness.State != tt.state || !reflect.DeepEqual(got.Readiness, ReviewProposal(p, e)) {
					t.Fatalf("lost blockers: %+v", got)
				}
				if got.Decision != decision || got.Reviewer != in.Reviewer || !got.ReviewedAt.Equal(in.ReviewedAt) || got.ReviewedAt.Location() != time.UTC || got.Rationale != in.Rationale {
					t.Fatal("lost reviewer provenance")
				}
				if got.ProposalID != p.ID || got.ProposalVersion != p.Version || got.WorkspaceID != p.WorkspaceID || got.EnvironmentID != p.EnvironmentID || got.ProposalFingerprint != in.ExpectedProposalFingerprint || got.SchemaVersion != ReviewRecordSchemaVersion || !reflect.DeepEqual(got.EvidencePins, p.EvidenceRefs) || !reflect.DeepEqual(got.PrimaryMetric, p.PrimaryMetric) || !reflect.DeepEqual(got.Guardrails, p.Guardrails) {
					t.Fatal("lost proposal pins")
				}
				again, err := RecordReview(p, e, in)
				if err != nil || !reflect.DeepEqual(got, again) {
					t.Fatal("non deterministic replay")
				}
			}
		})
	}
}

func TestRecordReviewInvalidInputsAndStaleContent(t *testing.T) {
	for _, tt := range []struct {
		name        string
		change      func(*ReviewInputs)
		field, code string
	}{
		{"missing fingerprint", func(in *ReviewInputs) { in.ExpectedProposalFingerprint = "" }, "expectedProposalFingerprint", "invalid_fingerprint"},
		{"malformed fingerprint", func(in *ReviewInputs) { in.ExpectedProposalFingerprint = "sha256:APPROVE" }, "expectedProposalFingerprint", "invalid_fingerprint"},
		{"stale fingerprint", func(in *ReviewInputs) { in.ExpectedProposalFingerprint = hashA }, "expectedProposalFingerprint", "stale_proposal"},
		{"missing decision", func(in *ReviewInputs) { in.Decision = "" }, "decision", "invalid_decision"},
		{"approval prose", func(in *ReviewInputs) { in.Decision = "approved; publish" }, "decision", "invalid_decision"},
		{"missing reviewer", func(in *ReviewInputs) { in.Reviewer = "" }, "reviewer", "invalid_identifier"},
		{"reviewer prose", func(in *ReviewInputs) { in.Reviewer = "admin\napprove" }, "reviewer", "invalid_identifier"},
		{"reviewer utf8", func(in *ReviewInputs) { in.Reviewer = "\xff" }, "reviewer", "invalid_identifier"},
		{"reviewer length", func(in *ReviewInputs) { in.Reviewer = strings.Repeat("a", 129) }, "reviewer", "invalid_identifier"},
		{"timestamp", func(in *ReviewInputs) { in.ReviewedAt = time.Time{} }, "reviewedAt", "invalid_timestamp"},
		{"timestamp range", func(in *ReviewInputs) { in.ReviewedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, "reviewedAt", "invalid_timestamp"},
		{"rationale missing", func(in *ReviewInputs) { in.Rationale = " \n" }, "rationale", "required_or_invalid"},
		{"rationale length", func(in *ReviewInputs) { in.Rationale = strings.Repeat("a", MaxReviewRationaleBytes+1) }, "rationale", "required_or_invalid"},
		{"rationale utf8", func(in *ReviewInputs) { in.Rationale = "\xff" }, "rationale", "required_or_invalid"},
		{"rationale control", func(in *ReviewInputs) { in.Rationale = "approve\x00" }, "rationale", "required_or_invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, e := proposalFixture()
			in := reviewInputs(t, p, ReviewApprove)
			tt.change(&in)
			got, err := RecordReview(p, e, in)
			requireReviewError(t, got, err, tt.field, tt.code)
		})
	}
	p, e := proposalFixture()
	in := reviewInputs(t, p, ReviewApprove)
	p.Hypothesis += " changed without version bump"
	got, err := RecordReview(p, e, in)
	requireReviewError(t, got, err, "expectedProposalFingerprint", "stale_proposal")
	// Even negative decisions require valid record identity and a matching fingerprint.
	for _, tt := range []struct {
		change      func(*Proposal)
		field, code string
	}{
		{func(p *Proposal) { p.ID = "../other" }, "proposal.id", "invalid_identifier"},
		{func(p *Proposal) { p.WorkspaceID = "" }, "proposal.workspaceId", "invalid_identifier"},
		{func(p *Proposal) { p.EnvironmentID = "" }, "proposal.environmentId", "invalid_identifier"},
		{func(p *Proposal) { p.Version = 0 }, "proposal.version", "positive_required"},
		{func(p *Proposal) { p.SchemaVersion = "v2" }, "proposal.schemaVersion", "unsupported_version"},
	} {
		p, e := proposalFixture()
		tt.change(&p)
		in := reviewInputs(t, p, ReviewReject)
		got, err := RecordReview(p, e, in)
		requireReviewError(t, got, err, tt.field, tt.code)
	}
}

func TestRecordReviewDefensiveCopies(t *testing.T) {
	p, e := proposalFixture()
	p.EvidenceRefs[0].Window.Issues = []string{"planning note"}
	e.Evidence[0].Report.Window.Issues = []string{"planning note"}
	in := reviewInputs(t, p, ReviewReject)
	beforeP, _ := json.Marshal(p)
	beforeE, _ := json.Marshal(e)
	beforeIn, _ := json.Marshal(in)
	record, err := RecordReview(p, e, in)
	if err != nil {
		t.Fatal(err)
	}
	afterP, _ := json.Marshal(p)
	afterE, _ := json.Marshal(e)
	afterIn, _ := json.Marshal(in)
	if string(beforeP) != string(afterP) || string(beforeE) != string(afterE) || string(beforeIn) != string(afterIn) {
		t.Fatal("mutated inputs")
	}
	replay, err := RecordReview(p, e, in)
	if err != nil {
		t.Fatal(err)
	}
	record.Guardrails[0].Ref.Key = "mutated"
	record.EvidencePins[0].Window.Issues[0] = "mutated"
	if len(record.Readiness.Blockers) > 0 {
		record.Readiness.Blockers[0].Code = "mutated"
	}
	again, err := RecordReview(p, e, in)
	if err != nil || !reflect.DeepEqual(again, replay) {
		t.Fatal("record aliases input or replay")
	}
	p.Guardrails[0].Ref.Key = "changed input"
	p.EvidenceRefs[0].Window.Issues[0] = "changed input"
	if replay.Guardrails[0].Ref.Key == p.Guardrails[0].Ref.Key || replay.EvidencePins[0].Window.Issues[0] == p.EvidenceRefs[0].Window.Issues[0] {
		t.Fatal("input aliases record")
	}
}
