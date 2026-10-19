package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestPublishedSnapshotPinsApprovedRevisionAndExcludesDrafts(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))
	first, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.publisher, f.ws.ID, f.env.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManifestRevision != 1 || sha256.Sum256(first.Payload) != first.SHA256 {
		t.Fatal("source identity lost")
	}
	var snap model.Snapshot
	if err := json.Unmarshal(first.Payload, &snap); err != nil {
		t.Fatal(err)
	}
	evaluator, err := evaluation.NewEvaluator(&snap)
	if err != nil || evaluator.Evaluate("checkout", model.EvaluationContext{Attributes: map[string]any{"user_id": "user"}}).Failed {
		t.Fatal("incomplete source evaluator", err)
	}
	draft := testFlag("checkout")
	draft.DisplayName = "unapproved draft"
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, draft, platform.RiskNormal, 1); err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.publisher, f.ws.ID, f.env.ID, 1)
	if err != nil || !bytes.Equal(first.Payload, second.Payload) || bytes.Contains(second.Payload, []byte("unapproved draft")) {
		t.Fatal("draft entered published source", err)
	}
	first.Payload[0] ^= 1
	third, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.publisher, f.ws.ID, f.env.ID, 1)
	if err != nil || !bytes.Equal(second.Payload, third.Payload) {
		t.Fatal("source aliases caller bytes", err)
	}
	for _, revision := range []int64{0, 2} {
		if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, revision); !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("revision %d accepted: %v", revision, err)
		}
	}
	for _, actor := range []Actor{f.viewer, f.otherWS} {
		if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), actor, f.ws.ID, f.env.ID, 1); err == nil {
			t.Fatal("unauthorized source read accepted")
		}
	}
	if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.Slug, f.env.ID, 1); err == nil {
		t.Fatal("display slug became signature scope")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.LoadPublishedSnapshotContext(ctx, f.owner, f.ws.ID, f.env.ID, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
}

func TestPublishedSnapshotRejectsSubstrateManifestDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*model.Manifest)
	}{
		{"foreign scope", func(m *model.Manifest) { m.Workspace = "foreign" }},
		{"unknown schema", func(m *model.Manifest) { m.SchemaVersion++ }},
		{"duplicate entry", func(m *model.Manifest) { m.Flags = append(m.Flags, m.Flags[0]) }},
		{"different content head", func(m *model.Manifest) { m.Flags[0].Hash = "foreign" }},
		{"ahead revision", func(m *model.Manifest) { m.Revision++ }},
		{"missing generated time", func(m *model.Manifest) { m.GeneratedAt = time.Time{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))
			m, err := f.svc.Config.GetManifestContext(t.Context(), f.env.NamespaceID)
			if err != nil {
				t.Fatal(err)
			}
			test.change(m)
			if err := f.svc.Config.PublishManifestContext(t.Context(), f.env.NamespaceID, m); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, 1); err == nil {
				t.Fatal("manifest drift accepted", err)
			}
		})
	}
}

type dishonestSourceStore struct{ configstore.Store }

func (s dishonestSourceStore) GetDocContext(ctx context.Context, namespace, dataID string) ([]byte, string, error) {
	body, hash, err := s.Store.GetDocContext(ctx, namespace, dataID)
	return append(body, ' '), hash, err
}
func TestPublishedSnapshotComputesReadbackHashIndependently(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))
	f.svc.Config = dishonestSourceStore{Store: f.svc.Config}
	if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, 1); !errors.Is(err, configstore.ErrHashMismatch) {
		t.Fatal("backend-provided false hash trusted", err)
	}
}
func TestPublishedSnapshotRejectsMissingPublishedDocument(t *testing.T) {
	f := newFixture(t)
	published := fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))
	if err := f.svc.Config.DeleteDocContext(t.Context(), f.env.NamespaceID, published.DataID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, 1); !errors.Is(err, configstore.ErrIncompleteSnapshot) {
		t.Fatal("missing document accepted", err)
	}
}

func TestPublishedSnapshotIncludesApprovedDeletion(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("gone"))
	if err := f.svc.MarkFlagDeleted(f.publisher, f.ws.ID, f.env.ID, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, "gone", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublishFlag(f.owner, f.ws.ID, f.env.ID, "gone", ""); err != nil {
		t.Fatal(err)
	}
	source, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	var snap model.Snapshot
	if err := json.Unmarshal(source.Payload, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Flags) != 0 || source.ManifestRevision != 2 {
		t.Fatal("approved deletion omitted from source")
	}
}

func TestPublishedSnapshotIncludesExactSegmentDependency(t *testing.T) {
	f := newFixture(t)
	segment := &model.Segment{SchemaVersion: 1, Key: "beta-users", Constraints: []model.Constraint{{Field: "user_id", Op: model.OpRegex, Value: "^beta-"}}}
	if _, err := f.svc.SaveSegmentDraft(f.publisher, f.ws.ID, f.env.ID, segment, platform.RiskNormal); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitSegmentDraft(f.publisher, f.ws.ID, f.env.ID, segment.Key); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ApproveSegmentDraft(f.owner, f.ws.ID, f.env.ID, segment.Key); err != nil {
		t.Fatal(err)
	}
	published, err := f.svc.PublishSegment(f.owner, f.ws.ID, f.env.ID, segment.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	flag := testFlag("checkout")
	flag.Targeting = []model.Rule{{Segments: []string{segment.Key}, Variation: "on"}}
	fullPublishFlow(t, f, f.publisher, f.owner, flag)
	source, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	var snap model.Snapshot
	if err := json.Unmarshal(source.Payload, &snap); err != nil {
		t.Fatal(err)
	}
	evaluator, err := evaluation.NewEvaluator(&snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Segments) != 1 || evaluator.Evaluate("checkout", model.EvaluationContext{Attributes: map[string]any{"user_id": "beta-user"}}).Value != true {
		t.Fatal("segment dependency absent or mixed")
	}
	if err := f.svc.Config.DeleteDocContext(t.Context(), f.env.NamespaceID, published.DataID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.owner, f.ws.ID, f.env.ID, 2); !errors.Is(err, configstore.ErrIncompleteSnapshot) {
		t.Fatal("missing segment accepted", err)
	}
}
