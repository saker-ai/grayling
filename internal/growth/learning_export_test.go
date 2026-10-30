package growth

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"testing"
)

func assertLearningRoundTrip(t *testing.T, p Proposal, r *LearningRecord) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLearningRecord(bytes.NewReader(data))
	if err != nil || !reflect.DeepEqual(r, decoded) {
		t.Fatalf("decode round trip: %v", err)
	}
	v, err := VerifyLearning(p, *decoded, p.WorkspaceID, p.EnvironmentID)
	if err != nil || v.ObservationStatus != r.Observation.Status {
		t.Fatalf("verify round trip: %v", err)
	}
	found, err := SearchLearning([]LearningRecord{*decoded}, LearningSearchQuery{p.WorkspaceID, p.EnvironmentID, r.Followups[0]})
	if err != nil || len(found.Matches) != 1 || !reflect.DeepEqual(found.Matches[0], *r) {
		t.Fatalf("search round trip: %v", err)
	}
}
