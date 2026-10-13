package platform

import (
	"fmt"
	"sort"
	"time"

	"github.com/saker-ai/grayling/pkg/model"
)

// ApprovalTTL bounds the time between review and execution. A changed draft
// must be submitted and approved again, even while the old grant is live.
const ApprovalTTL = 24 * time.Hour

func draftHash(content any, risk Risk, deleted bool, baseRevision int64) (string, error) {
	raw, err := model.CanonicalJSON(struct {
		Content      any   `json:"content"`
		Risk         Risk  `json:"risk"`
		Deleted      bool  `json:"deleted"`
		BaseRevision int64 `json:"baseRevision"`
	}{content, risk, deleted, baseRevision})
	if err != nil {
		return "", err
	}
	return model.HashBytes(raw), nil
}

// FlagDraftFingerprint identifies the complete saved draft state seen by an
// editor. An empty fingerprint means no draft existed at read time.
func FlagDraftFingerprint(d *FlagDraft) (string, error) {
	if d == nil {
		return "", nil
	}
	raw, err := model.CanonicalJSON(d)
	if err != nil {
		return "", fmt.Errorf("flag draft fingerprint: %w", err)
	}
	return model.HashBytes(raw), nil
}

// VerifyFlagApproval checks the exact reviewed bytes and current base revision.
func (s *Store) VerifyFlagApproval(d *FlagDraft) error {
	if d.State != DraftApproved || d.ApprovalExpiresAt == nil || !s.now().Before(*d.ApprovalExpiresAt) {
		return fmt.Errorf("%w: flag approval is missing or expired", ErrInvalidState)
	}
	hash, err := draftHash(d.Content, d.Risk, d.Deleted, d.BaseRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if hash != d.ApprovedHash || hash != d.SubmittedHash {
		return fmt.Errorf("%w: flag changed after approval", ErrConflict)
	}
	if s.CurrentFlagRevision(d.WorkspaceID, d.EnvironmentID, d.Key) != d.BaseRevision {
		return fmt.Errorf("%w: published flag changed after approval", ErrConflict)
	}
	return nil
}

// VerifySegmentApproval applies the same grant to a segment draft.
func (s *Store) VerifySegmentApproval(d *SegmentDraft) error {
	if d.State != DraftApproved || d.ApprovalExpiresAt == nil || !s.now().Before(*d.ApprovalExpiresAt) {
		return fmt.Errorf("%w: segment approval is missing or expired", ErrInvalidState)
	}
	hash, err := draftHash(d.Content, d.Risk, false, d.BaseRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if hash != d.ApprovedHash || hash != d.SubmittedHash {
		return fmt.Errorf("%w: segment changed after approval", ErrConflict)
	}
	if s.CurrentSegmentRevision(d.WorkspaceID, d.EnvironmentID, d.Key) != d.BaseRevision {
		return fmt.Errorf("%w: published segment changed after approval", ErrConflict)
	}
	return nil
}

func flagScopeKey(workspaceID, environmentID, key string) string {
	return workspaceID + "\x00" + environmentID + "\x00" + key
}

func scopeKey(workspaceID, environmentID string) string {
	return workspaceID + "\x00" + environmentID
}

// ---------------------------------------------------------------------------
// Flag drafts
// ---------------------------------------------------------------------------

// SaveFlagDraft creates or updates the working draft. baseRevision enforces
// optimistic locking (§11: 乐观锁 revision): checked saves require both the
// published revision and complete draft fingerprint to match the editor's
// snapshot. Internal callers without a fingerprint retain the revision check
// when baseRevision > 0.
func (s *Store) SaveFlagDraft(workspaceID, environmentID string, content *model.Flag,
	risk Risk, actor string, baseRevision int64, expectedDraftHash ...string) (*FlagDraft, error) {

	if content == nil {
		return nil, fmt.Errorf("%w: nil flag content", ErrValidation)
	}
	if err := model.ValidateFlag(content, s.segmentsForValidation(workspaceID, environmentID)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if risk == "" {
		risk = RiskNormal
	}
	if risk != RiskNormal && risk != RiskHigh {
		return nil, fmt.Errorf("%w: invalid risk %q", ErrValidation, risk)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkEnvLocked(workspaceID, environmentID); err != nil {
		return nil, err
	}
	q := s.quotasLocked(workspaceID)
	scope := flagScopeKey(workspaceID, environmentID, content.Key)

	existing := s.data.FlagDrafts[scope]
	if len(expectedDraftHash) > 0 {
		actual, err := FlagDraftFingerprint(existing)
		if err != nil {
			return nil, err
		}
		if actual != expectedDraftHash[0] {
			return nil, fmt.Errorf("%w: flag draft changed since it was read; reload and merge", ErrConflict)
		}
	}
	if existing == nil {
		// New flag: quota check on distinct published+draft keys.
		if s.countFlagsLocked(workspaceID, environmentID) >= q.MaxFlags {
			return nil, fmt.Errorf("%w: max %d flags in workspace", ErrQuotaExceeded, q.MaxFlags)
		}
	}
	if baseRevision > 0 || len(expectedDraftHash) > 0 {
		current := s.data.CurrentFlagRev[scope]
		if baseRevision != current {
			return nil, fmt.Errorf("%w: draft is based on revision %d but current is %d; reload and reapply",
				ErrConflict, baseRevision, current)
		}
	}
	now := s.now()
	draft := &FlagDraft{
		WorkspaceID: workspaceID, EnvironmentID: environmentID, Key: content.Key,
		Content: content, State: DraftEditing, Risk: risk,
		UpdatedBy: actor, UpdatedAt: now,
		BaseRevision: s.data.CurrentFlagRev[scope],
	}
	if existing != nil {
		draft.Deleted = existing.Deleted
	}
	s.data.FlagDrafts[scope] = draft
	s.changes.markDirty(collFlagDrafts, scope)
	return draft, s.commitLocked()
}

// MarkFlagDraftDeleted flags a draft for tombstone publication. The draft
// still goes through submit/approve/publish so deletions are auditable.
func (s *Store) MarkFlagDraftDeleted(workspaceID, environmentID, key, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.FlagDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return fmt.Errorf("%w: draft %q", ErrNotFound, key)
	}
	d.Deleted = true
	d.Risk = RiskHigh
	d.UpdatedBy = actor
	d.UpdatedAt = s.now()
	s.changes.markDirty(collFlagDrafts, flagScopeKey(workspaceID, environmentID, key))
	return s.commitLocked()
}

// SubmitFlagDraft moves editing → submitted.
func (s *Store) SubmitFlagDraft(workspaceID, environmentID, key, actor string, tokenID ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.FlagDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return fmt.Errorf("%w: draft %q", ErrNotFound, key)
	}
	if d.State != DraftEditing {
		return fmt.Errorf("%w: draft %q is %s, want editing", ErrInvalidState, key, d.State)
	}
	d.State = DraftSubmitted
	d.SubmittedBy = actor
	if len(tokenID) > 0 {
		d.SubmittedTokenID = tokenID[0]
	}
	hash, err := draftHash(d.Content, d.Risk, d.Deleted, d.BaseRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	d.SubmittedHash = hash
	now := s.now()
	d.SubmittedAt = &now
	s.changes.markDirty(collFlagDrafts, flagScopeKey(workspaceID, environmentID, key))
	return s.commitLocked()
}

// ApproveFlagDraft moves submitted → approved, enforcing the two-person rule.
func (s *Store) ApproveFlagDraft(workspaceID, environmentID, key, actor string, note string, tokenID ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.FlagDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return fmt.Errorf("%w: draft %q", ErrNotFound, key)
	}
	if d.State != DraftSubmitted {
		return fmt.Errorf("%w: draft %q is %s, want submitted", ErrInvalidState, key, d.State)
	}
	if d.SubmittedBy == actor || len(tokenID) > 0 && tokenID[0] != "" && tokenID[0] == d.SubmittedTokenID {
		return ErrSelfApproval
	}
	hash, err := draftHash(d.Content, d.Risk, d.Deleted, d.BaseRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if hash != d.SubmittedHash {
		return fmt.Errorf("%w: draft changed after submission", ErrConflict)
	}
	d.State = DraftApproved
	d.ApprovedBy = actor
	if len(tokenID) > 0 {
		d.ApprovedTokenID = tokenID[0]
	}
	d.ApprovedHash = hash
	d.ReviewNote = note
	now := s.now()
	d.ApprovedAt = &now
	expires := now.Add(ApprovalTTL)
	d.ApprovalExpiresAt = &expires
	s.changes.markDirty(collFlagDrafts, flagScopeKey(workspaceID, environmentID, key))
	return s.commitLocked()
}

// RejectFlagDraft returns submitted → editing with a note.
func (s *Store) RejectFlagDraft(workspaceID, environmentID, key, actor, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.FlagDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return fmt.Errorf("%w: draft %q", ErrNotFound, key)
	}
	if d.State != DraftSubmitted {
		return fmt.Errorf("%w: draft %q is %s, want submitted", ErrInvalidState, key, d.State)
	}
	d.State = DraftEditing
	d.ReviewNote = note
	d.UpdatedBy = actor
	d.UpdatedAt = s.now()
	d.SubmittedBy = ""
	d.SubmittedTokenID = ""
	d.SubmittedAt = nil
	d.SubmittedHash = ""
	d.ApprovedBy = ""
	d.ApprovedTokenID = ""
	d.ApprovedAt = nil
	d.ApprovedHash = ""
	d.ApprovalExpiresAt = nil
	s.changes.markDirty(collFlagDrafts, flagScopeKey(workspaceID, environmentID, key))
	return s.commitLocked()
}

// GetFlagDraft returns one draft.
func (s *Store) GetFlagDraft(workspaceID, environmentID, key string) (*FlagDraft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.data.FlagDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return nil, fmt.Errorf("%w: draft %q", ErrNotFound, key)
	}
	return d, nil
}

// ListFlagDrafts returns all drafts in one environment ordered by key.
func (s *Store) ListFlagDrafts(workspaceID, environmentID string) []*FlagDraft {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	out := []*FlagDraft{}
	for k, d := range s.data.FlagDrafts {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ---------------------------------------------------------------------------
// Flag revisions (immutable)
// ---------------------------------------------------------------------------

// AppendFlagRevision records a newly published immutable revision and moves
// the current pointer. Called by the admin service AFTER the config store
// confirmed the write is readable (publish confirmation = read-back, §3).
func (s *Store) AppendFlagRevision(rev *FlagRevision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := flagScopeKey(rev.WorkspaceID, rev.EnvironmentID, rev.Key)
	for _, existing := range s.data.FlagRevisions[scope] {
		if existing.Revision == rev.Revision {
			// Idempotent re-record (publish retry after partial failure):
			// identical revision+hash is a no-op, different content with the
			// same revision number is a hard inconsistency.
			if existing.Hash == rev.Hash {
				s.data.CurrentFlagRev[scope] = rev.Revision
				s.changes.markDirty(collFlagState, scope)
				return s.commitLocked()
			}
			return fmt.Errorf("%w: revision %d of %q already exists with different content",
				ErrConflict, rev.Revision, rev.Key)
		}
	}
	if rev.PublishedAt.IsZero() {
		rev.PublishedAt = s.now()
	}
	s.data.FlagRevisions[scope] = append(s.data.FlagRevisions[scope], rev)
	next := s.data.NextFlagRev[scope]
	if rev.Revision >= next {
		s.data.NextFlagRev[scope] = rev.Revision + 1
	}
	s.data.CurrentFlagRev[scope] = rev.Revision
	s.changes.markDirty(collFlagRevisions, scope)
	s.changes.markDirty(collFlagState, scope)
	// An approved draft becomes the published state; reset it to editing so
	// further changes start a fresh review cycle against the new base.
	if d, ok := s.data.FlagDrafts[scope]; ok && d.State == DraftApproved && !d.Deleted {
		d.State = DraftEditing
		d.BaseRevision = rev.Revision
		d.SubmittedBy = ""
		d.SubmittedAt = nil
		d.SubmittedTokenID = ""
		d.SubmittedHash = ""
		d.ApprovedBy = ""
		d.ApprovedAt = nil
		d.ApprovedTokenID = ""
		d.ApprovedHash = ""
		d.ApprovalExpiresAt = nil
		s.changes.markDirty(collFlagDrafts, scope)
	}
	if d, ok := s.data.FlagDrafts[scope]; ok && d.Deleted {
		delete(s.data.FlagDrafts, scope)
		s.changes.markDeleted(collFlagDrafts, scope)
	}
	return s.commitLocked()
}

// NextFlagRevision allocates the next revision number for a flag.
func (s *Store) NextFlagRevision(workspaceID, environmentID, key string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	next := s.data.NextFlagRev[flagScopeKey(workspaceID, environmentID, key)]
	if next == 0 {
		return 1
	}
	return next
}

// CurrentFlagRevision returns the revision in force (0 when never published).
func (s *Store) CurrentFlagRevision(workspaceID, environmentID, key string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.CurrentFlagRev[flagScopeKey(workspaceID, environmentID, key)]
}

// GetFlagRevision returns one immutable revision.
func (s *Store) GetFlagRevision(workspaceID, environmentID, key string, rev int64) (*FlagRevision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.data.FlagRevisions[flagScopeKey(workspaceID, environmentID, key)] {
		if r.Revision == rev {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: %s revision %d", ErrNotFound, key, rev)
}

// ListFlagRevisions returns revisions newest-first.
func (s *Store) ListFlagRevisions(workspaceID, environmentID, key string) []*FlagRevision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.data.FlagRevisions[flagScopeKey(workspaceID, environmentID, key)]
	out := make([]*FlagRevision, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out
}

// CurrentFlagContents returns the (key → revision) pairs in force for one
// environment — the manifest input.
func (s *Store) CurrentFlagContents(workspaceID, environmentID string) map[string]*FlagRevision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]*FlagRevision{}
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	for scope, cur := range s.data.CurrentFlagRev {
		if len(scope) <= len(prefix) || scope[:len(prefix)] != prefix || cur == 0 {
			continue
		}
		for _, r := range s.data.FlagRevisions[scope] {
			if r.Revision == cur {
				out[r.Key] = r
				break
			}
		}
	}
	return out
}

// SetCurrentFlagRevision repoints the in-force revision at an EXISTING
// immutable revision — the store half of a rollback. The admin service must
// have verified the backing document is still readable before calling this
// (§5.3: 回滚是发布一个新 manifest revision，指向经验证且仍保留的旧配置版本).
func (s *Store) SetCurrentFlagRevision(workspaceID, environmentID, key string, rev int64, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := flagScopeKey(workspaceID, environmentID, key)
	found := false
	for _, r := range s.data.FlagRevisions[scope] {
		if r.Revision == rev {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: revision %d of %q not retained", ErrNotFound, rev, key)
	}
	s.data.CurrentFlagRev[scope] = rev
	s.changes.markDirty(collFlagState, scope)
	if d, ok := s.data.FlagDrafts[scope]; ok {
		d.BaseRevision = rev
		s.changes.markDirty(collFlagDrafts, scope)
	}
	return s.commitLocked()
}

// TombstoneWindow is how many manifest revisions a deleted key keeps
// appearing as a tombstone entry (§3: 删除用 tombstone；full-sync clients
// evict on absence, tombstones exist for future delta protocols).
const TombstoneWindow = 10

// DeleteFlagCurrent removes a flag from the in-force set and records a
// tombstone at the given manifest revision. Immutable revisions are kept
// (audit + potential un-delete via rollback) and pruned by policy later.
func (s *Store) DeleteFlagCurrent(workspaceID, environmentID, key string, manifestRev int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := flagScopeKey(workspaceID, environmentID, key)
	if _, ok := s.data.FlagRevisions[scope]; !ok {
		return fmt.Errorf("%w: flag %q was never published", ErrNotFound, key)
	}
	delete(s.data.CurrentFlagRev, scope)
	delete(s.data.FlagDrafts, scope)
	s.data.FlagTombstones[scope] = manifestRev
	s.changes.markDirty(collFlagState, scope)
	s.changes.markDeleted(collFlagDrafts, scope)
	return s.commitLocked()
}

// FlagTombstones returns tombstone manifest entries whose deletion is within
// TombstoneWindow of currentRev.
func (s *Store) FlagTombstones(workspaceID, environmentID string, currentRev int64) []model.ManifestEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	var out []model.ManifestEntry
	for scope, deletedAt := range s.data.FlagTombstones {
		if len(scope) <= len(prefix) || scope[:len(prefix)] != prefix {
			continue
		}
		if currentRev-deletedAt >= TombstoneWindow {
			continue
		}
		key := scope[len(prefix):]
		// Report the last revision that existed so delta clients can align.
		var lastRev int64
		if revs := s.data.FlagRevisions[scope]; len(revs) > 0 {
			lastRev = revs[len(revs)-1].Revision
		}
		out = append(out, model.ManifestEntry{
			Key: key, Revision: lastRev, Deleted: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// SetManifestRevision records the newest manifest revision published for one
// environment. Monotonic: older values are ignored (idempotent retries).
func (s *Store) SetManifestRevision(workspaceID, environmentID string, rev int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := scopeKey(workspaceID, environmentID)
	if rev > s.data.ManifestRevisions[scope] {
		s.data.ManifestRevisions[scope] = rev
		s.changes.markDirty(collManifestRevisions, scope)
	}
	return s.commitLocked()
}

// ManifestRevision returns the last published manifest revision (0 = none).
func (s *Store) ManifestRevision(workspaceID, environmentID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.ManifestRevisions[scopeKey(workspaceID, environmentID)]
}

// ---------------------------------------------------------------------------
// Segments (same lifecycle, smaller surface)
// ---------------------------------------------------------------------------

// SaveSegmentDraft creates/updates a segment draft, validating against other
// published segments in scope.
func (s *Store) SaveSegmentDraft(workspaceID, environmentID string, content *model.Segment,
	risk Risk, actor string) (*SegmentDraft, error) {

	if content == nil {
		return nil, fmt.Errorf("%w: nil segment content", ErrValidation)
	}
	segments := s.segmentsForValidation(workspaceID, environmentID)
	segments[content.Key] = content
	if err := model.ValidateSegment(content, segments); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if risk == "" {
		risk = RiskNormal
	}
	if risk != RiskNormal && risk != RiskHigh {
		return nil, fmt.Errorf("%w: invalid risk %q", ErrValidation, risk)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkEnvLocked(workspaceID, environmentID); err != nil {
		return nil, err
	}
	scope := flagScopeKey(workspaceID, environmentID, content.Key)
	if s.data.SegmentDrafts[scope] == nil {
		q := s.quotasLocked(workspaceID)
		if len(s.data.SegmentDrafts)+len(s.data.SegmentRevisions) >= q.MaxSegments*2 {
			return nil, fmt.Errorf("%w: max %d segments in workspace", ErrQuotaExceeded, q.MaxSegments)
		}
	}
	d := &SegmentDraft{
		WorkspaceID: workspaceID, EnvironmentID: environmentID, Key: content.Key,
		Content: content, State: DraftEditing, Risk: risk,
		UpdatedBy: actor, UpdatedAt: s.now(),
		BaseRevision: s.data.CurrentSegmentRev[scope],
	}
	s.data.SegmentDrafts[scope] = d
	s.changes.markDirty(collSegmentDrafts, scope)
	return d, s.commitLocked()
}

// SubmitSegmentDraft / ApproveSegmentDraft mirror the flag workflow.
func (s *Store) SubmitSegmentDraft(workspaceID, environmentID, key, actor string, tokenID ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.SegmentDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return fmt.Errorf("%w: segment draft %q", ErrNotFound, key)
	}
	if d.State != DraftEditing {
		return fmt.Errorf("%w: segment %q is %s", ErrInvalidState, key, d.State)
	}
	d.State = DraftSubmitted
	d.SubmittedBy = actor
	if len(tokenID) > 0 {
		d.SubmittedTokenID = tokenID[0]
	}
	now := s.now()
	d.SubmittedAt = &now
	hash, err := draftHash(d.Content, d.Risk, false, d.BaseRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	d.SubmittedHash = hash
	s.changes.markDirty(collSegmentDrafts, flagScopeKey(workspaceID, environmentID, key))
	return s.commitLocked()
}

func (s *Store) ApproveSegmentDraft(workspaceID, environmentID, key, actor string, tokenID ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.SegmentDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return fmt.Errorf("%w: segment draft %q", ErrNotFound, key)
	}
	if d.State != DraftSubmitted {
		return fmt.Errorf("%w: segment %q is %s", ErrInvalidState, key, d.State)
	}
	if d.SubmittedBy == actor || len(tokenID) > 0 && tokenID[0] != "" && tokenID[0] == d.SubmittedTokenID {
		return ErrSelfApproval
	}
	hash, err := draftHash(d.Content, d.Risk, false, d.BaseRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if hash != d.SubmittedHash {
		return fmt.Errorf("%w: segment changed after submission", ErrConflict)
	}
	d.State = DraftApproved
	d.ApprovedBy = actor
	if len(tokenID) > 0 {
		d.ApprovedTokenID = tokenID[0]
	}
	d.ApprovedHash = hash
	now := s.now()
	d.ApprovedAt = &now
	expires := now.Add(ApprovalTTL)
	d.ApprovalExpiresAt = &expires
	s.changes.markDirty(collSegmentDrafts, flagScopeKey(workspaceID, environmentID, key))
	return s.commitLocked()
}

// GetSegmentDraft returns one segment draft.
func (s *Store) GetSegmentDraft(workspaceID, environmentID, key string) (*SegmentDraft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.data.SegmentDrafts[flagScopeKey(workspaceID, environmentID, key)]
	if !ok {
		return nil, fmt.Errorf("%w: segment draft %q", ErrNotFound, key)
	}
	return d, nil
}

// ListSegmentDrafts returns all segment drafts of one environment.
func (s *Store) ListSegmentDrafts(workspaceID, environmentID string) []*SegmentDraft {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	out := []*SegmentDraft{}
	for k, d := range s.data.SegmentDrafts {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// AppendSegmentRevision records a published immutable segment revision.
func (s *Store) AppendSegmentRevision(rev *SegmentRevision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := flagScopeKey(rev.WorkspaceID, rev.EnvironmentID, rev.Key)
	for _, existing := range s.data.SegmentRevisions[scope] {
		if existing.Revision == rev.Revision {
			if existing.Hash == rev.Hash {
				s.data.CurrentSegmentRev[scope] = rev.Revision
				s.changes.markDirty(collSegmentState, scope)
				return s.commitLocked()
			}
			return fmt.Errorf("%w: segment revision %d exists with different content", ErrConflict, rev.Revision)
		}
	}
	if rev.PublishedAt.IsZero() {
		rev.PublishedAt = s.now()
	}
	s.data.SegmentRevisions[scope] = append(s.data.SegmentRevisions[scope], rev)
	next := s.data.NextSegmentRev[scope]
	if rev.Revision >= next {
		s.data.NextSegmentRev[scope] = rev.Revision + 1
	}
	s.data.CurrentSegmentRev[scope] = rev.Revision
	s.changes.markDirty(collSegmentRevisions, scope)
	s.changes.markDirty(collSegmentState, scope)
	if d, ok := s.data.SegmentDrafts[scope]; ok && d.State == DraftApproved {
		d.State = DraftEditing
		d.SubmittedBy = ""
		d.SubmittedTokenID = ""
		d.SubmittedAt = nil
		d.SubmittedHash = ""
		d.ApprovedBy = ""
		d.ApprovedTokenID = ""
		d.ApprovedAt = nil
		d.ApprovedHash = ""
		d.ApprovalExpiresAt = nil
		s.changes.markDirty(collSegmentDrafts, scope)
	}
	return s.commitLocked()
}

// NextSegmentRevision allocates the next segment revision number.
func (s *Store) NextSegmentRevision(workspaceID, environmentID, key string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	next := s.data.NextSegmentRev[flagScopeKey(workspaceID, environmentID, key)]
	if next == 0 {
		return 1
	}
	return next
}

// CurrentSegmentRevision returns the in-force revision, or zero if absent.
func (s *Store) CurrentSegmentRevision(workspaceID, environmentID, key string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.CurrentSegmentRev[flagScopeKey(workspaceID, environmentID, key)]
}

// ListSegmentRevisions returns immutable published revisions newest first.
func (s *Store) ListSegmentRevisions(workspaceID, environmentID, key string) []*SegmentRevision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.data.SegmentRevisions[flagScopeKey(workspaceID, environmentID, key)]
	out := make([]*SegmentRevision, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out
}

// CurrentSegmentContents returns key → current revision for the manifest.
func (s *Store) CurrentSegmentContents(workspaceID, environmentID string) map[string]*SegmentRevision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]*SegmentRevision{}
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	for scope, cur := range s.data.CurrentSegmentRev {
		if len(scope) <= len(prefix) || scope[:len(prefix)] != prefix || cur == 0 {
			continue
		}
		for _, r := range s.data.SegmentRevisions[scope] {
			if r.Revision == cur {
				out[r.Key] = r
				break
			}
		}
	}
	return out
}

// segmentsForValidation builds the current published segment set for
// cross-validation of drafts (the revision in force per key).
func (s *Store) segmentsForValidation(workspaceID, environmentID string) map[string]*model.Segment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]*model.Segment{}
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	for scope, cur := range s.data.CurrentSegmentRev {
		if len(scope) <= len(prefix) || scope[:len(prefix)] != prefix || cur == 0 {
			continue
		}
		for _, r := range s.data.SegmentRevisions[scope] {
			if r.Revision == cur {
				out[r.Key] = r.Content
				break
			}
		}
	}
	return out
}

// checkEnvLocked verifies the (workspace, environment) pair exists.
func (s *Store) checkEnvLocked(workspaceID, environmentID string) error {
	env, ok := s.data.Environments[environmentID]
	if !ok || env.WorkspaceID != workspaceID {
		return fmt.Errorf("%w: environment %q not in workspace %q", ErrNotFound, environmentID, workspaceID)
	}
	return nil
}

// quotasLocked returns configured quotas (caller holds lock).
func (s *Store) quotasLocked(workspaceID string) Quotas {
	if q, ok := s.data.Quotas[workspaceID]; ok {
		return q
	}
	return DefaultQuotas()
}

// countFlagsLocked counts distinct flag keys (drafts ∪ published) in one env.
func (s *Store) countFlagsLocked(workspaceID, environmentID string) int {
	prefix := scopeKey(workspaceID, environmentID) + "\x00"
	seen := map[string]bool{}
	for k := range s.data.FlagDrafts {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			seen[k] = true
		}
	}
	for k, revs := range s.data.FlagRevisions {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix && len(revs) > 0 {
			seen[k] = true
		}
	}
	return len(seen)
}

// PruneFlagRevisions removes old revisions of one flag, always keeping:
// the current revision, everything newer than olderThan, and at least the
// newest `keep` revisions (§5.1: 旧版本保留到 SDK 最大陈旧期及回滚窗口之
// 后，按配额清理). Returns removed dataIds so the caller can delete them
// from the config store.
func (s *Store) PruneFlagRevisions(workspaceID, environmentID, key string, keep int, olderThan time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := flagScopeKey(workspaceID, environmentID, key)
	revs := s.data.FlagRevisions[scope]
	if len(revs) <= keep {
		return nil, nil
	}
	// revs are append-ordered ascending; protect the newest `keep`.
	protectedRev := revs[len(revs)-keep].Revision
	current := s.data.CurrentFlagRev[scope]
	cutoff := s.now().Add(-olderThan)
	kept := make([]*FlagRevision, 0, len(revs))
	var removed []string
	for _, r := range revs {
		if r.Revision >= protectedRev || r.Revision == current || r.PublishedAt.After(cutoff) {
			kept = append(kept, r)
			continue
		}
		removed = append(removed, r.DataID)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	s.data.FlagRevisions[scope] = kept
	s.changes.markDirty(collFlagRevisions, scope)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return removed, nil
}
