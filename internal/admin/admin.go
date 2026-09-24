package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/evaluation"
	"github.com/godeps/golab/pkg/model"
)

// Listener is notified after a manifest revision becomes readable. The API
// layer wires this to the SSE hub; push is only an acceleration signal —
// confirmation of publication is the read-back itself (§3).
type Listener func(workspaceID, environmentID string, manifestRevision int64)

// Service orchestrates the control plane.
type Service struct {
	Store    *platform.Store
	Config   configstore.Store
	listener Listener
	now      func() time.Time

	// publishMu serializes the whole publish pipeline (revision
	// allocation → document write → revision record → manifest rebuild →
	// manifest write). Two concurrent publishes would otherwise allocate
	// the same manifest revision and the loser's flag revision could be
	// missing from the surviving manifest — a silent snapshot/store
	// divergence. Control-plane publishes are low-QPS; one global lock is
	// the correct trade for a single-node deployment (multi-node needs
	// database-level sequencing, see docs/实施状态.md).
	publishMu sync.Mutex

	// FlagPublishHook runs immediately before a flag revision is written to
	// the substrate (draft publication AND operational republishes). It
	// exists for cross-domain validation the admin service must not know
	// about — e.g. the AI allowlist check on modelRef variations (§7).
	// A non-nil error aborts the publish.
	FlagPublishHook func(workspaceID string, content *model.Flag) error
}

// NewService assembles the admin service. listener may be nil.
func NewService(store *platform.Store, cfg configstore.Store, listener Listener) *Service {
	return &Service{Store: store, Config: cfg, listener: listener, now: func() time.Time {
		return time.Now().UTC()
	}}
}

// PublishResult reports one successful publish pipeline run.
type PublishResult struct {
	FlagRevision     int64  `json:"flagRevision"`
	ManifestRevision int64  `json:"manifestRevision"`
	Hash             string `json:"hash"`
	DataID           string `json:"dataId"`
}

func (s *Service) notify(wsID, envID string, rev int64) {
	if s.listener != nil {
		s.listener(wsID, envID, rev)
	}
}

func (s *Service) audit(actor Actor, action, wsID, envID, object string, detail any) {
	d := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			d = string(b)
		}
	}
	_ = s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, action, wsID, envID, object, d)
}

// resolveEnv loads and authorizes the (workspace, environment) pair. The
// path value may be a slug or surrogate ID; authorization always compares
// surrogate IDs (token scopes store IDs, never slugs).
func (s *Service) resolveEnv(workspaceID, environmentID string, actor Actor, perm Permission) (*platform.Workspace, *platform.Environment, error) {
	ws, err := s.Store.GetWorkspace(workspaceID)
	if err != nil {
		return nil, nil, err
	}
	if err := Authorize(actor, ws.ID, perm); err != nil {
		return nil, nil, err
	}
	env, err := s.Store.GetEnvironment(ws.ID, environmentID)
	if err != nil {
		return nil, nil, err
	}
	return ws, env, nil
}

// ---------------------------------------------------------------------------
// Draft lifecycle (thin RBAC wrappers over the platform store)
// ---------------------------------------------------------------------------

// SaveFlagDraft validates, quota-checks and stores the working draft.
func (s *Service) SaveFlagDraft(actor Actor, workspaceID, environmentID string,
	content *model.Flag, risk platform.Risk, baseRevision int64) (*platform.FlagDraft, error) {

	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagWrite)
	if err != nil {
		return nil, err
	}
	if content == nil {
		return nil, fmt.Errorf("%w: nil flag", platform.ErrValidation)
	}
	// Enforce per-flag size quota before touching state (§11).
	raw, err := model.CanonicalJSON(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", platform.ErrValidation, err)
	}
	q := s.Store.GetQuotas(env.WorkspaceID)
	if len(raw) > q.MaxFlagBytes {
		return nil, fmt.Errorf("%w: flag document is %d bytes, max %d",
			platform.ErrQuotaExceeded, len(raw), q.MaxFlagBytes)
	}
	draft, err := s.Store.SaveFlagDraft(env.WorkspaceID, env.ID, content, risk, actor.Name, baseRevision)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "flag.draft.save", env.WorkspaceID, env.ID, content.Key, map[string]any{"risk": risk})
	return draft, nil
}

// SubmitFlagDraft moves a draft into review.
func (s *Service) SubmitFlagDraft(actor Actor, workspaceID, environmentID, key string) error {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagWrite)
	if err != nil {
		return err
	}
	if err := s.Store.SubmitFlagDraft(env.WorkspaceID, env.ID, key, actor.Name); err != nil {
		return err
	}
	s.audit(actor, "flag.draft.submit", env.WorkspaceID, env.ID, key, nil)
	return nil
}

// ApproveFlagDraft approves a submitted draft (two-person rule for
// high-risk is enforced by the store).
func (s *Service) ApproveFlagDraft(actor Actor, workspaceID, environmentID, key, note string) error {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagApprove)
	if err != nil {
		return err
	}
	if err := s.Store.ApproveFlagDraft(env.WorkspaceID, env.ID, key, actor.Name, note); err != nil {
		return err
	}
	s.audit(actor, "flag.draft.approve", env.WorkspaceID, env.ID, key, map[string]any{"note": note})
	return nil
}

// RejectFlagDraft returns a draft to editing.
func (s *Service) RejectFlagDraft(actor Actor, workspaceID, environmentID, key, note string) error {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagApprove)
	if err != nil {
		return err
	}
	if err := s.Store.RejectFlagDraft(env.WorkspaceID, env.ID, key, actor.Name, note); err != nil {
		return err
	}
	s.audit(actor, "flag.draft.reject", env.WorkspaceID, env.ID, key, map[string]any{"note": note})
	return nil
}

// MarkFlagDeleted prepares deletion: creates (or reuses) a draft carrying
// the current content, marks it deleted and moves it through the same
// submit/approve flow so deletions are reviewed and auditable.
func (s *Service) MarkFlagDeleted(actor Actor, workspaceID, environmentID, key string) error {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagWrite)
	if err != nil {
		return err
	}
	if _, err := s.Store.GetFlagDraft(env.WorkspaceID, env.ID, key); errors.Is(err, platform.ErrNotFound) {
		// Seed a draft from the revision in force.
		current := s.Store.CurrentFlagContents(env.WorkspaceID, env.ID)[key]
		if current == nil {
			return fmt.Errorf("%w: flag %q was never published", platform.ErrNotFound, key)
		}
		if _, err := s.Store.SaveFlagDraft(env.WorkspaceID, env.ID, current.Content,
			platform.RiskNormal, actor.Name, current.Revision); err != nil {
			return err
		}
	}
	if err := s.Store.MarkFlagDraftDeleted(env.WorkspaceID, env.ID, key, actor.Name); err != nil {
		return err
	}
	s.audit(actor, "flag.delete.mark", env.WorkspaceID, env.ID, key, nil)
	return nil
}

// ---------------------------------------------------------------------------
// Publish pipeline
// ---------------------------------------------------------------------------

// PublishFlag publishes an APPROVED draft as the next immutable revision and
// republishes the manifest. Order (§3): flag document write + read-back →
// platform revision record → manifest write + read-back → notify. If the
// manifest step fails, the flag document is already durable but unreferenced;
// retrying the publish converges (idempotent revision records).
func (s *Service) PublishFlag(actor Actor, workspaceID, environmentID, key, note string) (*PublishResult, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagPublish)
	if err != nil {
		return nil, err
	}
	draft, err := s.Store.GetFlagDraft(env.WorkspaceID, env.ID, key)
	if err != nil {
		return nil, err
	}
	if draft.State != platform.DraftApproved {
		return nil, fmt.Errorf("%w: draft %q is %s, want approved", platform.ErrInvalidState, key, draft.State)
	}
	if draft.Deleted {
		return s.publishDeletion(actor, env, key)
	}

	content := *draft.Content
	rev := s.Store.NextFlagRevision(env.WorkspaceID, env.ID, key)
	content.Version = model.FormatRevision(rev)
	fr, m, err := s.publishRevision(actor, env, &content, note)
	if err != nil {
		s.audit(actor, "flag.publish.failed", env.WorkspaceID, env.ID, key, map[string]any{"error": err.Error()})
		return nil, err
	}
	s.audit(actor, "flag.publish", env.WorkspaceID, env.ID, key, map[string]any{
		"revision": fr.Revision, "manifestRevision": m.Revision, "hash": fr.Hash,
	})
	return &PublishResult{FlagRevision: fr.Revision, ManifestRevision: m.Revision,
		Hash: fr.Hash, DataID: fr.DataID}, nil
}

// publishRevision writes one immutable flag revision + manifest. Shared by
// draft publication and operational republishes (experiment transitions).
// The entire pipeline is serialized (see publishMu).
func (s *Service) publishRevision(actor Actor, env *platform.Environment,
	content *model.Flag, note string) (*platform.FlagRevision, *model.Manifest, error) {

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	ws, err := s.Store.GetWorkspace(env.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	raw, err := model.CanonicalJSON(content)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", platform.ErrValidation, err)
	}
	q := s.Store.GetQuotas(env.WorkspaceID)
	if len(raw) > q.MaxFlagBytes {
		return nil, nil, fmt.Errorf("%w: %d bytes > max %d", platform.ErrQuotaExceeded, len(raw), q.MaxFlagBytes)
	}
	if s.FlagPublishHook != nil {
		if err := s.FlagPublishHook(env.WorkspaceID, content); err != nil {
			return nil, nil, err
		}
	}
	rev := s.Store.NextFlagRevision(env.WorkspaceID, env.ID, content.Key)
	dataID := configstore.FlagDataID(content.Key, rev)
	if _, err := s.Config.PublishDoc(env.NamespaceID, dataID, raw); err != nil {
		return nil, nil, err
	}
	hash := model.HashBytes(raw)
	fr := &platform.FlagRevision{
		WorkspaceID: env.WorkspaceID, EnvironmentID: env.ID, Key: content.Key,
		Revision: rev, Content: content, Hash: hash, DataID: dataID,
		PublishedAt: s.now(), PublishedBy: actor.Name, Note: note,
	}
	if err := s.Store.AppendFlagRevision(fr); err != nil {
		return nil, nil, err
	}
	m, err := s.publishManifest(ws, env)
	if err != nil {
		return fr, nil, err
	}
	return fr, m, nil
}

// publishDeletion tombstones a flag: remove from the current set and
// republish the manifest (the immutable revisions stay for audit; the
// tombstone tells delta-capable clients to evict, full-sync clients evict on
// absence).
func (s *Service) publishDeletion(actor Actor, env *platform.Environment, key string) (*PublishResult, error) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	ws, err := s.Store.GetWorkspace(env.WorkspaceID)
	if err != nil {
		return nil, err
	}
	newRev := s.Store.ManifestRevision(env.WorkspaceID, env.ID) + 1
	if err := s.Store.DeleteFlagCurrent(env.WorkspaceID, env.ID, key, newRev); err != nil {
		return nil, err
	}
	m, err := s.publishManifest(ws, env)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "flag.delete", env.WorkspaceID, env.ID, key, map[string]any{"manifestRevision": m.Revision})
	return &PublishResult{ManifestRevision: m.Revision}, nil
}

// buildManifest assembles the next manifest revision from the in-force
// content set. Entries are key-sorted for byte-level determinism.
func (s *Service) buildManifest(ws *platform.Workspace, env *platform.Environment) (*model.Manifest, error) {
	rev := s.Store.ManifestRevision(ws.ID, env.ID) + 1
	m := &model.Manifest{
		SchemaVersion: model.SchemaVersion,
		Workspace:     ws.ID, Environment: env.ID,
		WorkspaceSlug: ws.Slug, EnvironmentSlug: env.Slug,
		Revision:    rev,
		GeneratedAt: s.now().Truncate(time.Millisecond),
	}
	flags := s.Store.CurrentFlagContents(ws.ID, env.ID)
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fr := flags[k]
		m.Flags = append(m.Flags, model.ManifestEntry{
			Key: fr.Key, Revision: fr.Revision, DataID: fr.DataID, Hash: fr.Hash,
		})
	}
	segs := s.Store.CurrentSegmentContents(ws.ID, env.ID)
	skeys := make([]string, 0, len(segs))
	for k := range segs {
		skeys = append(skeys, k)
	}
	sort.Strings(skeys)
	for _, k := range skeys {
		sr := segs[k]
		m.Segments = append(m.Segments, model.ManifestEntry{
			Key: sr.Key, Revision: sr.Revision, DataID: sr.DataID, Hash: sr.Hash,
		})
	}
	m.Flags = append(m.Flags, s.Store.FlagTombstones(ws.ID, env.ID, rev)...)
	sort.Slice(m.Flags, func(i, j int) bool { return m.Flags[i].Key < m.Flags[j].Key })
	return m, nil
}

// publishManifest writes and confirms one manifest revision.
func (s *Service) publishManifest(ws *platform.Workspace, env *platform.Environment) (*model.Manifest, error) {
	m, err := s.buildManifest(ws, env)
	if err != nil {
		return nil, err
	}
	if err := s.Config.PublishManifest(env.NamespaceID, m); err != nil {
		return nil, err
	}
	if err := s.Store.SetManifestRevision(ws.ID, env.ID, m.Revision); err != nil {
		return nil, err
	}
	s.notify(ws.ID, env.ID, m.Revision)
	return m, nil
}

// RepublishManifest rebuilds and publishes the manifest without touching any
// flag revision — used by segment publication and reconciliation.
func (s *Service) RepublishManifest(actor Actor, workspaceID, environmentID, reason string) (*model.Manifest, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagPublish)
	if err != nil {
		return nil, err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	ws, err := s.Store.GetWorkspace(env.WorkspaceID)
	if err != nil {
		return nil, err
	}
	m, err := s.publishManifest(ws, env)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "manifest.republish", env.WorkspaceID, env.ID, configstore.ManifestDataID,
		map[string]any{"revision": m.Revision, "reason": reason})
	return m, nil
}

// RollbackFlag publishes a NEW manifest revision pointing at an existing,
// verified, retained flag revision (§5.3). It never fabricates content and
// never treats gonacos history as an atomic rollback API: the target
// revision must exist in the GoLab platform store AND read back from the
// config substrate with the recorded hash.
func (s *Service) RollbackFlag(actor Actor, workspaceID, environmentID, key string,
	targetRevision int64, reason string) (*PublishResult, error) {

	ws, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagPublish)
	if err != nil {
		return nil, err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if reason == "" {
		return nil, fmt.Errorf("%w: rollback requires a reason", platform.ErrValidation)
	}
	target, err := s.Store.GetFlagRevision(env.WorkspaceID, env.ID, key, targetRevision)
	if err != nil {
		return nil, err
	}
	// Verify the document is still retained and uncorrupted in the substrate.
	doc, hash, err := s.Config.GetDoc(env.NamespaceID, target.DataID)
	if err != nil {
		return nil, fmt.Errorf("%w: rollback target r%d not readable: %v", platform.ErrNotFound, targetRevision, err)
	}
	if hash != target.Hash {
		return nil, fmt.Errorf("%w: rollback target r%d hash drift", platform.ErrConflict, targetRevision)
	}
	var content model.Flag
	if err := json.Unmarshal(doc, &content); err != nil {
		return nil, fmt.Errorf("%w: rollback target r%d does not parse", platform.ErrValidation, targetRevision)
	}
	if err := s.Store.SetCurrentFlagRevision(env.WorkspaceID, env.ID, key, targetRevision, actor.Name); err != nil {
		return nil, err
	}
	m, err := s.publishManifest(ws, env)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "flag.rollback", env.WorkspaceID, env.ID, key, map[string]any{
		"targetRevision": targetRevision, "manifestRevision": m.Revision, "reason": reason,
	})
	return &PublishResult{FlagRevision: targetRevision, ManifestRevision: m.Revision,
		Hash: target.Hash, DataID: target.DataID}, nil
}

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

// SaveSegmentDraft stores a segment draft after cross-validation.
func (s *Service) SaveSegmentDraft(actor Actor, workspaceID, environmentID string,
	content *model.Segment, risk platform.Risk) (*platform.SegmentDraft, error) {

	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagWrite)
	if err != nil {
		return nil, err
	}
	d, err := s.Store.SaveSegmentDraft(env.WorkspaceID, env.ID, content, risk, actor.Name)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "segment.draft.save", env.WorkspaceID, env.ID, content.Key, nil)
	return d, nil
}

// SubmitSegmentDraft / ApproveSegmentDraft mirror the flag review flow.
func (s *Service) SubmitSegmentDraft(actor Actor, workspaceID, environmentID, key string) error {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagWrite)
	if err != nil {
		return err
	}
	if err := s.Store.SubmitSegmentDraft(env.WorkspaceID, env.ID, key, actor.Name); err != nil {
		return err
	}
	s.audit(actor, "segment.draft.submit", env.WorkspaceID, env.ID, key, nil)
	return nil
}

func (s *Service) ApproveSegmentDraft(actor Actor, workspaceID, environmentID, key string) error {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagApprove)
	if err != nil {
		return err
	}
	if err := s.Store.ApproveSegmentDraft(env.WorkspaceID, env.ID, key, actor.Name); err != nil {
		return err
	}
	s.audit(actor, "segment.draft.approve", env.WorkspaceID, env.ID, key, nil)
	return nil
}

// PublishSegment publishes an approved segment draft as an immutable
// revision and republishes the manifest.
func (s *Service) PublishSegment(actor Actor, workspaceID, environmentID, key, note string) (*PublishResult, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagPublish)
	if err != nil {
		return nil, err
	}
	draft, err := s.Store.GetSegmentDraft(env.WorkspaceID, env.ID, key)
	if err != nil {
		return nil, err
	}
	if draft.State != platform.DraftApproved {
		return nil, fmt.Errorf("%w: segment draft %q is %s", platform.ErrInvalidState, key, draft.State)
	}
	content := *draft.Content
	rev := s.Store.NextSegmentRevision(env.WorkspaceID, env.ID, key)
	content.Version = model.FormatRevision(rev)
	raw, err := model.CanonicalJSON(&content)
	if err != nil {
		return nil, err
	}
	dataID := configstore.SegmentDataID(key, rev)
	if _, err := s.Config.PublishDoc(env.NamespaceID, dataID, raw); err != nil {
		return nil, err
	}
	sr := &platform.SegmentRevision{
		WorkspaceID: env.WorkspaceID, EnvironmentID: env.ID, Key: key,
		Revision: rev, Content: &content, Hash: model.HashBytes(raw), DataID: dataID,
		PublishedAt: s.now(), PublishedBy: actor.Name,
	}
	if err := s.Store.AppendSegmentRevision(sr); err != nil {
		return nil, err
	}
	ws, err := s.Store.GetWorkspace(env.WorkspaceID)
	if err != nil {
		return nil, err
	}
	m, err := s.publishManifest(ws, env)
	if err != nil {
		return nil, err
	}
	s.audit(actor, "segment.publish", env.WorkspaceID, env.ID, key, map[string]any{
		"revision": rev, "manifestRevision": m.Revision, "note": note,
	})
	return &PublishResult{FlagRevision: rev, ManifestRevision: m.Revision,
		Hash: sr.Hash, DataID: dataID}, nil
}

// ---------------------------------------------------------------------------
// Read models for the API/console
// ---------------------------------------------------------------------------

// FlagView is the console read model of one flag.
type FlagView struct {
	Key             string                   `json:"key"`
	Draft           *platform.FlagDraft      `json:"draft,omitempty"`
	CurrentRevision int64                    `json:"currentRevision"`
	Current         *platform.FlagRevision   `json:"current,omitempty"`
	Revisions       []*platform.FlagRevision `json:"revisions,omitempty"`
}

// GetFlagView assembles draft + published state of one flag.
func (s *Service) GetFlagView(actor Actor, workspaceID, environmentID, key string, withHistory bool) (*FlagView, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, err
	}
	view := &FlagView{Key: key}
	if d, err := s.Store.GetFlagDraft(env.WorkspaceID, env.ID, key); err == nil {
		view.Draft = d
	}
	view.CurrentRevision = s.Store.CurrentFlagRevision(env.WorkspaceID, env.ID, key)
	if view.CurrentRevision > 0 {
		rev, err := s.Store.GetFlagRevision(env.WorkspaceID, env.ID, key, view.CurrentRevision)
		if err == nil {
			view.Current = rev
		}
	}
	if withHistory {
		view.Revisions = s.Store.ListFlagRevisions(env.WorkspaceID, env.ID, key)
	}
	return view, nil
}

// ListFlags returns the console list for one environment.
func (s *Service) ListFlags(actor Actor, workspaceID, environmentID string) ([]*FlagView, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*FlagView{}
	for _, d := range s.Store.ListFlagDrafts(env.WorkspaceID, env.ID) {
		byKey[d.Key] = &FlagView{Key: d.Key, Draft: d}
	}
	for key, cur := range s.Store.CurrentFlagContents(env.WorkspaceID, env.ID) {
		v, ok := byKey[key]
		if !ok {
			v = &FlagView{Key: key}
			byKey[key] = v
		}
		v.CurrentRevision = cur.Revision
		v.Current = cur
	}
	out := make([]*FlagView, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// DebugEvaluate runs one evaluation against the CURRENT published snapshot
// without recording exposures (§11: 评估调试器默认不写曝光).
func (s *Service) DebugEvaluate(actor Actor, workspaceID, environmentID, key string,
	evalCtx model.EvaluationContext) (*model.Result, *model.Snapshot, error) {

	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := s.Config.LoadSnapshot(env.NamespaceID)
	if err != nil {
		return nil, nil, err
	}
	ev, err := evaluation.NewEvaluator(snapshot)
	if err != nil {
		return nil, nil, err
	}
	res := ev.Evaluate(key, evalCtx)
	// Debugger output is diagnostic: strip exposure bookkeeping so nobody
	// can accidentally inflate an experiment by debugging it.
	res.TrackEvents = false
	res.Experiment = nil
	return &res, snapshot, nil
}
