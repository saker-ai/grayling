package agent

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

// RevocationRefreshCandidate contains only bounded metadata, never catalog bodies.
type RevocationRefreshCandidate struct {
	Scope            client.Scope
	ArtifactRevision uint64
	ExpiresAt        time.Time
}

// DueRevocationProofs scans a bounded tuple-keyset page. It includes scopes with
// no registration/admission entry and never holds row locks while minting proof.
func (s *Sessions) DueRevocationProofs(ctx context.Context, after client.Scope, before time.Duration) ([]RevocationRefreshCandidate, error) {
	if after != (client.Scope{}) && !scopeValid(after) || before <= 0 || before > 24*time.Hour {
		return nil, client.ErrResource
	}
	rows, err := s.pool.Query(ctx, `SELECT h.workspace_id,h.environment_id,h.service_key,h.revision::text,r.expires_at FROM agent_resource_heads h JOIN agent_resources r USING(workspace_id,environment_id,service_key,resource_key,revision) WHERE h.resource_key=$1 AND h.revision>0 AND (h.workspace_id,h.environment_id,h.service_key)>($2,$3,$4) AND r.expires_at<=clock_timestamp()+$5::bigint*interval '1 millisecond' ORDER BY h.workspace_id,h.environment_id,h.service_key LIMIT 100`, revocationStorageKey, after.WorkspaceID, after.EnvironmentID, after.ServiceKey, before.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RevocationRefreshCandidate, 0, 100)
	for rows.Next() {
		var item RevocationRefreshCandidate
		var revision string
		if err := rows.Scan(&item.Scope.WorkspaceID, &item.Scope.EnvironmentID, &item.Scope.ServiceKey, &revision, &item.ExpiresAt); err != nil {
			return nil, err
		}
		item.ArtifactRevision, err = strconv.ParseUint(revision, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// refreshRevocationCandidate is internal deployment-authorized proof maintenance.
// It cannot add or remove denials. All replicas use the same stable service actor
// and operation for an observed artifact; CAS/atomic receipts resolve races.
func (p *Publisher) refreshRevocationCandidate(ctx context.Context, item RevocationRefreshCandidate, v *client.Verifier) (StoredResource, error) {
	tx, err := p.sessions.pool.Begin(ctx)
	if err != nil {
		return StoredResource{}, err
	}
	defer tx.Rollback(ctx)
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch); err != nil {
		return StoredResource{}, err
	}
	d, old, err := lockRevocations(ctx, tx, item.Scope, uint64(epoch), false, v)
	if err != nil {
		return old, err
	}
	if old.Revision != item.ArtifactRevision || d.Epoch == 0 {
		return old, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "revocation proof changed")
	}
	if err := tx.Commit(ctx); err != nil {
		return old, err
	}
	// Scope is also part of the operation primary key and deterministic request.
	operation := "proof-refresh-" + strconv.FormatUint(d.Epoch, 10) + "-" + strconv.FormatUint(old.Revision, 10)
	return p.RevokeBundles(ctx, item.Scope, AdminMutation{ActorID: "system-agent-revocation-refresh", ActorRole: "owner", OperationID: operation}, RevokeBundlesRequest{ExpectedEpoch: d.Epoch, ExpectedArtifactRevision: old.Revision, RefreshOnly: true, Reason: "refresh revocation delivery proof"}, v)
}

// RunRevocationRefresh maintains delivery proof only. Enabling it delegates this
// narrow signing action to the deployment service identity. The caller owns the
// context and must await return before closing Sessions. Four workers and a
// two-second page budget bound concurrency even when a scope is unavailable.
func (p *Publisher) RunRevocationRefresh(ctx context.Context, v *client.Verifier, logf func(string, ...any)) error {
	if p == nil || v == nil {
		return client.ErrResource
	}
	before := p.lifetime / 3
	interval := min(time.Second, p.lifetime/6)
	var after client.Scope
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		query, cancel := context.WithTimeout(ctx, 2*time.Second)
		page, err := p.sessions.DueRevocationProofs(query, after, before)
		cancel()
		if err != nil {
			if ctx.Err() == nil && logf != nil {
				logf("agent: revocation proof scan unavailable")
			}
			timer.Reset(interval)
			continue
		}
		pageCtx, pageCancel := context.WithTimeout(ctx, 2*time.Second)
		jobs := make(chan int, len(page))
		for i := range page {
			jobs <- i
		}
		close(jobs)
		var last atomic.Int32
		last.Store(-1)
		var failed atomic.Bool
		var workers sync.WaitGroup
		for range min(4, len(page)) {
			workers.Go(func() {
				for i := range jobs {
					if pageCtx.Err() != nil {
						return
					}
					for old := last.Load(); int32(i) > old; old = last.Load() {
						if last.CompareAndSwap(old, int32(i)) {
							break
						}
					}
					_, err := p.refreshRevocationCandidate(pageCtx, page[i], v)
					if err != nil && pageCtx.Err() == nil {
						detail, stale := errors.AsType[*agentv1.ProtocolError](err)
						if !(stale && detail.Code == agentv1.ErrorCode_ERROR_CODE_STALE_REVISION) {
							failed.Store(true)
						}
					}
				}
			})
		}
		workers.Wait()
		timedOut := pageCtx.Err() != nil
		pageCancel()
		if ctx.Err() != nil {
			return nil
		}
		if (failed.Load() || timedOut) && logf != nil {
			logf("agent: revocation proof page incomplete")
		}
		attempted := int(last.Load()) + 1
		if attempted > 0 {
			after = page[attempted-1].Scope
		}
		if attempted == len(page) && len(page) < 100 {
			after = client.Scope{}
			timer.Reset(interval)
		} else {
			timer.Reset(0)
		}
	}
}
