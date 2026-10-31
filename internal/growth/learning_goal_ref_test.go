package growth

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestLearningGoalRefValidation(t *testing.T) {
	r := searchFixture(t)
	raw, _ := json.Marshal(r)
	for _, ref := range []string{
		"null", "{}", `{"id":"goal"}`,
		`{"id":"../secret","version":1,"fingerprint":"` + hashA + `"}`,
		`{"id":"goal","version":0,"fingerprint":"` + hashA + `"}`,
		`{"id":"goal","version":-1,"fingerprint":"` + hashA + `"}`,
		`{"id":"goal","version":1.5,"fingerprint":"` + hashA + `"}`,
		`{"id":"goal","version":9223372036854775808,"fingerprint":"` + hashA + `"}`,
		`{"id":"goal","version":1,"fingerprint":"secret"}`,
		`{"id":"goal","version":1,"fingerprint":"` + hashA + `","extra":true}`,
		`{"id":"goal","id":"other","version":1,"fingerprint":"` + hashA + `"}`,
	} {
		t.Run(ref, func(t *testing.T) {
			input := `{"goalRef":` + ref + `,` + string(raw[1:])
			if got, err := DecodeLearningRecord(strings.NewReader(input)); err == nil || got != nil {
				t.Fatal("accepted malformed reference")
			}
		})
	}
	for _, ref := range []LearningGoalRef{{}, {"../bad", 1, hashA}, {"goal", 0, hashA}, {"goal", -1, hashA}, {"goal", 1, "bad"}} {
		r.GoalRef = &ref
		// Even a text-excluded record must be validated.
		for _, q := range []LearningSearchQuery{{"ws1", "env1", "no match"}, {"other", "env1", r.Followups[0]}} {
			if got, err := SearchLearning([]LearningRecord{r}, q); err == nil || got != nil {
				t.Fatal("excluded malformed reference")
			}
		}
	}
}

func TestVerifyLearningGoalRef(t *testing.T) {
	p, _ := proposalFixture()
	p.GoalPin = &GoalPin{Version: 1<<53 + 1, Fingerprint: hashA}
	r := searchFixture(t)
	r.ProposalFingerprint, _ = ProposalFingerprint(p)
	r.GoalRef = &LearningGoalRef{p.GoalID, p.GoalPin.Version, p.GoalPin.Fingerprint}
	v, err := VerifyLearning(p, r, "ws1", "env1")
	if err != nil || !reflect.DeepEqual(v.GoalRef, r.GoalRef) {
		t.Fatal("verification lost reference", err)
	}
	v.GoalRef.Version--
	if r.GoalRef.Version != p.GoalPin.Version {
		t.Fatal("verification aliases record")
	}
	for _, ref := range []LearningGoalRef{
		{"other", p.GoalPin.Version, hashA},
		{p.GoalID, p.GoalPin.Version - 1, hashA},
		{p.GoalID, p.GoalPin.Version, hashB},
	} {
		r.GoalRef = &ref
		if v, err := VerifyLearning(p, r, "ws1", "env1"); err == nil || v != nil {
			t.Fatal("accepted altered reference")
		}
	}
	p.GoalPin = nil
	r.ProposalFingerprint, _ = ProposalFingerprint(p)
	r.GoalRef = &LearningGoalRef{p.GoalID, 1, hashA}
	if v, err := VerifyLearning(p, r, "ws1", "env1"); err == nil || v != nil {
		t.Fatal("accepted provenance for unpinned proposal")
	}
	// Historical records stay readable, even when the proposal had a pin.
	for _, pinned := range []bool{false, true} {
		if pinned {
			p.GoalPin = &GoalPin{Version: 1, Fingerprint: hashA}
		}
		r.ProposalFingerprint, _ = ProposalFingerprint(p)
		r.GoalRef = nil
		assertLearningRoundTrip(t, p, &r)
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), "goalRef") {
			t.Fatal("changed legacy bytes")
		}
	}
}

func TestLearningGoalRefSearchSnapshot(t *testing.T) {
	r := searchFixture(t)
	r.GoalRef = &LearningGoalRef{"goal", 1<<53 + 1, hashA}
	found, err := SearchLearning([]LearningRecord{r}, LearningSearchQuery{"ws1", "env1", r.Followups[0]})
	if err != nil || len(found.Matches) != 1 || !reflect.DeepEqual(found.Matches[0].GoalRef, r.GoalRef) {
		t.Fatal("lost reference", err)
	}
	found.Matches[0].GoalRef.Version--
	if r.GoalRef.Version != 1<<53+1 {
		t.Fatal("search aliases reference")
	}
}

type learningGoalRefTransport func(*http.Request) (*http.Response, error)

func (f learningGoalRefTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecordLearningGoalRefSnapshotBeforeHTTP(t *testing.T) {
	p, _ := proposalFixture()
	p.GoalPin = &GoalPin{Version: 1<<53 + 1, Fingerprint: hashA}
	want := LearningGoalRef{p.GoalID, p.GoalPin.Version, p.GoalPin.Fingerprint}
	r := searchFixture(t)
	fp, _ := ProposalFingerprint(p)
	reader := readerFor(t, "http://127.0.0.1", nil)
	calls := 0
	reader.client.Transport = learningGoalRefTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatal("HTTP write")
		}
		// Same goroutine, after preflight/snapshot; no concurrent caller mutation.
		p.GoalPin.Version = 2
		p.GoalPin.Fingerprint = hashB
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
	})
	got, err := reader.RecordLearning(t.Context(), p, r.LinkedExperiment, LearningInputs{
		ExpectedProposalFingerprint: fp, Recorder: r.Recorder, RecordedAt: r.RecordedAt, Followups: r.Followups,
	})
	if err != nil || calls == 0 || got.GoalRef == nil || *got.GoalRef != want || got.Observation.Status != NeedsData {
		t.Fatalf("snapshot changed during read: %+v %v", got, err)
	}
}
