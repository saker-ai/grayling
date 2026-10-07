package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyScopedControlObject replaces only one flag, segment, or environment
// scope. Queries use the same primary keys as the persistence writer.
func (p *pgPersistence) applyScopedControlObject(ctx context.Context, d *Data, c controlChange) error {
	if c.coll == collManifestRevisions {
		ws, env, err := splitScope2(c.id)
		if err != nil {
			return err
		}
		var rev int64
		err = p.pool.QueryRow(ctx, `SELECT manifest_revision FROM env_state WHERE workspace_id=$1 AND environment_id=$2`, ws, env).Scan(&rev)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.ManifestRevisions, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		d.ManifestRevisions[c.id] = rev
		return nil
	}
	ws, env, key, err := splitScope3(c.id)
	if err != nil {
		return err
	}
	switch c.coll {
	case collFlagDrafts:
		dr := &FlagDraft{}
		var content []byte
		var state, risk string
		err = p.pool.QueryRow(ctx, `SELECT workspace_id, environment_id, key, content, state, risk, updated_by, updated_at, submitted_by, submitted_at, approved_by, approved_at, review_note, deleted, base_revision, submitted_token_id, submitted_hash, approved_token_id, approved_hash, approval_expires_at FROM flag_drafts WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, key).Scan(&dr.WorkspaceID, &dr.EnvironmentID, &dr.Key, &content, &state, &risk, &dr.UpdatedBy, &dr.UpdatedAt, &dr.SubmittedBy, &dr.SubmittedAt, &dr.ApprovedBy, &dr.ApprovedAt, &dr.ReviewNote, &dr.Deleted, &dr.BaseRevision, &dr.SubmittedTokenID, &dr.SubmittedHash, &dr.ApprovedTokenID, &dr.ApprovedHash, &dr.ApprovalExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.FlagDrafts, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		dr.State = DraftState(state)
		dr.Risk = Risk(risk)
		if err = json.Unmarshal(content, &dr.Content); err != nil {
			return err
		}
		d.FlagDrafts[c.id] = dr
	case collSegmentDrafts:
		dr := &SegmentDraft{}
		var content []byte
		var state, risk string
		err = p.pool.QueryRow(ctx, `SELECT workspace_id, environment_id, key, content, state, risk, updated_by, updated_at, submitted_by, approved_by, base_revision, submitted_token_id, submitted_at, submitted_hash, approved_token_id, approved_at, approved_hash, approval_expires_at FROM segment_drafts WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, key).Scan(&dr.WorkspaceID, &dr.EnvironmentID, &dr.Key, &content, &state, &risk, &dr.UpdatedBy, &dr.UpdatedAt, &dr.SubmittedBy, &dr.ApprovedBy, &dr.BaseRevision, &dr.SubmittedTokenID, &dr.SubmittedAt, &dr.SubmittedHash, &dr.ApprovedTokenID, &dr.ApprovedAt, &dr.ApprovedHash, &dr.ApprovalExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.SegmentDrafts, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		dr.State = DraftState(state)
		dr.Risk = Risk(risk)
		if err = json.Unmarshal(content, &dr.Content); err != nil {
			return err
		}
		d.SegmentDrafts[c.id] = dr
	case collFlagRevisions:
		rows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key, revision, content, hash, data_id, published_at, published_by, note, rollback_of FROM flag_revisions WHERE workspace_id=$1 AND environment_id=$2 AND key=$3 ORDER BY revision`, ws, env, key)
		if err != nil {
			return err
		}
		var revisions []*FlagRevision
		for rows.Next() {
			r := &FlagRevision{}
			var content []byte
			if err = rows.Scan(&r.WorkspaceID, &r.EnvironmentID, &r.Key, &r.Revision, &content, &r.Hash, &r.DataID, &r.PublishedAt, &r.PublishedBy, &r.Note, &r.RollbackOf); err != nil {
				break
			}
			if err = json.Unmarshal(content, &r.Content); err != nil {
				break
			}
			revisions = append(revisions, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return fmt.Errorf("flag revisions: %w", err)
		}
		if len(revisions) == 0 {
			delete(d.FlagRevisions, c.id)
		} else {
			d.FlagRevisions[c.id] = revisions
		}
	case collSegmentRevisions:
		rows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key, revision, content, hash, data_id, published_at, published_by FROM segment_revisions WHERE workspace_id=$1 AND environment_id=$2 AND key=$3 ORDER BY revision`, ws, env, key)
		if err != nil {
			return err
		}
		var revisions []*SegmentRevision
		for rows.Next() {
			r := &SegmentRevision{}
			var content []byte
			if err = rows.Scan(&r.WorkspaceID, &r.EnvironmentID, &r.Key, &r.Revision, &content, &r.Hash, &r.DataID, &r.PublishedAt, &r.PublishedBy); err != nil {
				break
			}
			if err = json.Unmarshal(content, &r.Content); err != nil {
				break
			}
			revisions = append(revisions, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return fmt.Errorf("segment revisions: %w", err)
		}
		if len(revisions) == 0 {
			delete(d.SegmentRevisions, c.id)
		} else {
			d.SegmentRevisions[c.id] = revisions
		}
	case collFlagState:
		var current, next int64
		var tomb *int64
		err = p.pool.QueryRow(ctx, `SELECT current_revision,next_revision,tombstone_manifest_rev FROM flag_state WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, key).Scan(&current, &next, &tomb)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		delete(d.CurrentFlagRev, c.id)
		delete(d.NextFlagRev, c.id)
		delete(d.FlagTombstones, c.id)
		if err == nil {
			if current > 0 {
				d.CurrentFlagRev[c.id] = current
			}
			if next > 0 {
				d.NextFlagRev[c.id] = next
			}
			if tomb != nil {
				d.FlagTombstones[c.id] = *tomb
			}
		}
	case collSegmentState:
		var current, next int64
		err = p.pool.QueryRow(ctx, `SELECT current_revision,next_revision FROM segment_state WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, key).Scan(&current, &next)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		delete(d.CurrentSegmentRev, c.id)
		delete(d.NextSegmentRev, c.id)
		if err == nil {
			if current > 0 {
				d.CurrentSegmentRev[c.id] = current
			}
			if next > 0 {
				d.NextSegmentRev[c.id] = next
			}
		}
	default:
		return fmt.Errorf("unsupported scoped collection %s", c.coll)
	}
	return nil
}
