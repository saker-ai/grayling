package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
)

const maxPublishedSnapshotBytes = 64 << 20

// PublishedSnapshot is a caller-owned, complete readback of one platform
// publication. It contains no drafts and grants no Bundle approval by itself.
type PublishedSnapshot struct {
	ManifestRevision int64
	Payload          []byte
	SHA256           [32]byte
}

// LoadPublishedSnapshotContext pins the expected platform publication while
// holding the same cross-replica lock as publishing. Substrate drift is rejected,
// never adopted or repaired here. Callers must separately approve build/resource
// mapping and publish the Bundle/policy pair atomically.
func (s *Service) LoadPublishedSnapshotContext(ctx context.Context, actor Actor, workspaceID, environmentID string, expectedRevision int64) (PublishedSnapshot, error) {
	if err := Authorize(actor, workspaceID, PermFlagPublish); err != nil {
		return PublishedSnapshot{}, err
	}
	if expectedRevision <= 0 {
		return PublishedSnapshot{}, platform.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return PublishedSnapshot{}, err
	}
	env, err := s.Store.GetEnvironment(workspaceID, environmentID)
	if err != nil {
		return PublishedSnapshot{}, err
	}
	// Internal IDs only: display slugs cannot silently change the signature scope.
	if env.ID != environmentID || env.WorkspaceID != workspaceID {
		return PublishedSnapshot{}, platform.ErrNotFound
	}
	release, err := s.acquirePublishLockContext(ctx, env)
	if err != nil {
		return PublishedSnapshot{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return PublishedSnapshot{}, err
	}
	return s.loadPublishedSnapshotLocked(ctx, workspaceID, environmentID, expectedRevision)
}

// Caller holds the scope publish lock and has reloaded the platform working set.
func (s *Service) loadPublishedSnapshotLocked(ctx context.Context, workspaceID, environmentID string, expectedRevision int64) (PublishedSnapshot, error) {
	ws, err := s.Store.GetWorkspace(workspaceID)
	if err != nil {
		return PublishedSnapshot{}, err
	}
	env, err := s.Store.GetEnvironment(workspaceID, environmentID)
	if err != nil {
		return PublishedSnapshot{}, err
	}
	if s.Store.ManifestRevision(workspaceID, environmentID) != expectedRevision {
		return PublishedSnapshot{}, platform.ErrConflict
	}
	if s.Config == nil {
		return PublishedSnapshot{}, configstore.ErrIncompleteSnapshot
	}
	m, err := s.Config.GetManifestContext(ctx, env.NamespaceID)
	if err != nil {
		return PublishedSnapshot{}, err
	}
	if m == nil || m.SchemaVersion != model.SchemaVersion || m.Workspace != workspaceID || m.Environment != environmentID || m.Revision != expectedRevision || m.GeneratedAt.IsZero() || !manifestMatchesStore(s.Store, ws, env, m) {
		return PublishedSnapshot{}, configstore.ErrReadbackMismatch
	}
	snap := model.Snapshot{SchemaVersion: model.SchemaVersion, Workspace: workspaceID, Environment: environmentID, WorkspaceSlug: m.WorkspaceSlug, EnvironmentSlug: m.EnvironmentSlug, ManifestRevision: m.Revision, GeneratedAt: m.GeneratedAt, Flags: make(map[string]*model.Flag), Segments: make(map[string]*model.Segment)}
	total := 0
	read := func(entry model.ManifestEntry, target any) error {
		body, hash, err := s.Config.GetDocContext(ctx, env.NamespaceID, entry.DataID)
		if err != nil {
			return errors.Join(configstore.ErrIncompleteSnapshot, err)
		}
		total += len(body)
		if len(body) == 0 || total > maxPublishedSnapshotBytes {
			return configstore.ErrIncompleteSnapshot
		}
		digest := sha256.Sum256(body)
		if hash != entry.Hash || hex.EncodeToString(digest[:]) != entry.Hash {
			return configstore.ErrHashMismatch
		}
		if err := json.Unmarshal(body, target, json.RejectUnknownMembers(true)); err != nil {
			return errors.Join(configstore.ErrIncompleteSnapshot, err)
		}
		return nil
	}
	seen := make(map[string]bool, len(m.Flags))
	for _, entry := range m.Flags {
		if seen[entry.Key] || !model.KeyPattern.MatchString(entry.Key) || (entry.Deleted && (entry.Revision < 0 || entry.DataID != "" || entry.Hash != "")) || (!entry.Deleted && (entry.Revision <= 0 || entry.DataID != configstore.FlagDataID(entry.Key, entry.Revision))) {
			return PublishedSnapshot{}, configstore.ErrReadbackMismatch
		}
		seen[entry.Key] = true
		if entry.Deleted {
			continue
		}
		var flag model.Flag
		if err := read(entry, &flag); err != nil {
			return PublishedSnapshot{}, err
		}
		if flag.Key != entry.Key {
			return PublishedSnapshot{}, configstore.ErrReadbackMismatch
		}
		snap.Flags[entry.Key] = &flag
	}
	clear(seen)
	for _, entry := range m.Segments {
		if seen[entry.Key] || !model.KeyPattern.MatchString(entry.Key) || (entry.Deleted && (entry.Revision < 0 || entry.DataID != "" || entry.Hash != "")) || (!entry.Deleted && (entry.Revision <= 0 || entry.DataID != configstore.SegmentDataID(entry.Key, entry.Revision))) {
			return PublishedSnapshot{}, configstore.ErrReadbackMismatch
		}
		seen[entry.Key] = true
		if entry.Deleted {
			continue
		}
		var segment model.Segment
		if err := read(entry, &segment); err != nil {
			return PublishedSnapshot{}, err
		}
		if segment.Key != entry.Key {
			return PublishedSnapshot{}, configstore.ErrReadbackMismatch
		}
		snap.Segments[entry.Key] = &segment
	}
	if _, err := evaluation.NewEvaluator(&snap); err != nil {
		return PublishedSnapshot{}, errors.Join(configstore.ErrIncompleteSnapshot, err)
	}
	body, err := json.Marshal(snap, json.Deterministic(true))
	if err != nil || len(body) > maxPublishedSnapshotBytes {
		return PublishedSnapshot{}, configstore.ErrIncompleteSnapshot
	}
	if err := ctx.Err(); err != nil {
		return PublishedSnapshot{}, err
	}
	return PublishedSnapshot{ManifestRevision: m.Revision, Payload: body, SHA256: sha256.Sum256(body)}, nil
}
