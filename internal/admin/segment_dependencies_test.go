package admin

import (
	"errors"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func publishTestSegment(t *testing.T, f *fixture, segment *model.Segment) {
	t.Helper()
	if _, err := f.svc.SaveSegmentDraft(f.publisher, f.ws.ID, f.env.ID, segment, platform.RiskNormal); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitSegmentDraft(f.publisher, f.ws.ID, f.env.ID, segment.Key); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ApproveSegmentDraft(f.owner, f.ws.ID, f.env.ID, segment.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublishSegment(f.owner, f.ws.ID, f.env.ID, segment.Key, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentDependenciesFollowPublishedNestedReferences(t *testing.T) {
	f := newFixture(t)
	publishTestSegment(t, f, &model.Segment{SchemaVersion: model.SchemaVersion, Key: "base",
		Constraints: []model.Constraint{{Field: "user_id", Op: model.OpEq, Value: "alice"}}})
	publishTestSegment(t, f, &model.Segment{SchemaVersion: model.SchemaVersion, Key: "nested",
		Segments: []string{"base"}})
	direct := testFlag("direct")
	direct.Targeting = []model.Rule{{Segments: []string{"base"}, Variation: "on"}}
	fullPublishFlow(t, f, f.publisher, f.owner, direct)
	throughNested := testFlag("nested-flag")
	throughNested.Targeting = []model.Rule{{Segments: []string{"nested"}, Variation: "on"}}
	fullPublishFlow(t, f, f.publisher, f.owner, throughNested)
	draftOnly := testFlag("draft-only")
	draftOnly.Targeting = []model.Rule{{Segments: []string{"base"}, Variation: "on"}}
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, draftOnly, platform.RiskNormal, 0); err != nil {
		t.Fatal(err)
	}
	result, err := f.svc.GetSegmentDependencies(f.viewer, f.ws.ID, f.env.ID, "base")
	if err != nil {
		t.Fatal(err)
	}
	if result.SegmentRevision != 1 || len(result.Flags) != 2 ||
		result.Flags[0].Key != "direct" || len(result.Flags[0].Via) != 1 || result.Flags[0].Via[0] != "base" ||
		result.Flags[1].Key != "nested-flag" || len(result.Flags[1].Via) != 2 ||
		result.Flags[1].Via[0] != "nested" || result.Flags[1].Via[1] != "base" {
		t.Fatalf("dependencies: %+v", result)
	}
	if _, err := f.svc.GetSegmentDependencies(f.otherWS, f.ws.ID, f.env.ID, "base"); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace read: %v", err)
	}
	if _, err := f.svc.GetSegmentDependencies(f.viewer, f.ws.ID, f.env.ID, "missing"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("missing segment: %v", err)
	}
}
