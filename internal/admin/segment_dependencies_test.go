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

func TestSegmentMatchPreviewUsesSubmittedDocumentWithoutPublishing(t *testing.T) {
	f := newFixture(t)
	publishTestSegment(t, f, &model.Segment{SchemaVersion: model.SchemaVersion, Key: "audience",
		Constraints: []model.Constraint{{Field: "country", Op: model.OpEq, Value: "CN"}}})
	candidate := &model.Segment{SchemaVersion: model.SchemaVersion, Key: "audience",
		Constraints: []model.Constraint{{Field: "country", Op: model.OpEq, Value: "US"}}}
	preview, err := f.svc.PreviewSegmentMatch(f.viewer, f.ws.ID, f.env.ID, "audience", candidate,
		model.EvaluationContext{Attributes: map[string]any{"country": "US"}})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.DraftMatched || preview.PublishedMatched == nil || *preview.PublishedMatched ||
		preview.PublishedRevision != 1 {
		t.Fatalf("draft vs published: %+v", preview)
	}
	current := f.store.CurrentSegmentContents(f.ws.ID, f.env.ID)["audience"]
	if current == nil || current.Content.Constraints[0].Value != "CN" {
		t.Fatalf("preview changed published content: %+v", current)
	}
	if _, err := f.svc.PreviewSegmentMatch(f.otherWS, f.ws.ID, f.env.ID, "audience", candidate,
		model.EvaluationContext{}); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace preview: %v", err)
	}
	invalid := &model.Segment{SchemaVersion: model.SchemaVersion, Key: "audience", Segments: []string{"missing"}}
	if _, err := f.svc.PreviewSegmentMatch(f.viewer, f.ws.ID, f.env.ID, "audience", invalid,
		model.EvaluationContext{}); !errors.Is(err, platform.ErrValidation) {
		t.Fatalf("invalid candidate: %v", err)
	}
	newSegment := &model.Segment{SchemaVersion: model.SchemaVersion, Key: "new",
		Constraints: []model.Constraint{{Field: "country", Op: model.OpEq, Value: "US"}}}
	newPreview, err := f.svc.PreviewSegmentMatch(f.viewer, f.ws.ID, f.env.ID, "new", newSegment,
		model.EvaluationContext{Attributes: map[string]any{"country": "US"}})
	if err != nil || !newPreview.DraftMatched || newPreview.PublishedMatched != nil || newPreview.PublishedRevision != 0 {
		t.Fatalf("new segment preview: %+v %v", newPreview, err)
	}
}

func TestSegmentHistoryOrdersPublishedVersionsAndEnforcesScope(t *testing.T) {
	f := newFixture(t)
	key := "history"
	for _, country := range []string{"CN", "US"} {
		publishTestSegment(t, f, &model.Segment{SchemaVersion: model.SchemaVersion, Key: key,
			Constraints: []model.Constraint{{Field: "country", Op: model.OpEq, Value: country}}})
	}
	history, err := f.svc.GetSegmentHistory(f.viewer, f.ws.ID, f.env.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	if history.CurrentRevision != 2 || len(history.Revisions) != 2 ||
		history.Revisions[0].Revision != 2 || history.Revisions[0].Content.Constraints[0].Value != "US" ||
		history.Revisions[1].Revision != 1 || history.Revisions[1].Content.Constraints[0].Value != "CN" {
		t.Fatalf("history: %+v", history)
	}
	if _, err := f.svc.GetSegmentHistory(f.otherWS, f.ws.ID, f.env.ID, key); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace history: %v", err)
	}
	empty, err := f.svc.GetSegmentHistory(f.viewer, f.ws.ID, f.env.ID, "new")
	if err != nil || empty.CurrentRevision != 0 || len(empty.Revisions) != 0 {
		t.Fatalf("unpublished history: %+v %v", empty, err)
	}
}
