package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	client "github.com/saker-ai/grayling/pkg/agent"
)

// RegistrationScopes enumerates one bounded keyset page. A zero cursor starts
// a new sweep; scope identity and sort order match the durable primary key.
func (s *Sessions) RegistrationScopes(ctx context.Context, after client.Scope) ([]client.Scope, error) {
	if after != (client.Scope{}) && !scopeValid(after) {
		return nil, denied()
	}
	rows, err := s.pool.Query(ctx, `SELECT workspace_id,environment_id,service_key FROM agent_registration_scopes WHERE (workspace_id,environment_id,service_key)>($1,$2,$3) ORDER BY workspace_id,environment_id,service_key LIMIT 100`, after.WorkspaceID, after.EnvironmentID, after.ServiceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]client.Scope, 0, 100)
	for rows.Next() {
		var scope client.Scope
		if err := rows.Scan(&scope.WorkspaceID, &scope.EnvironmentID, &scope.ServiceKey); err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
}

// RunNaming owns at most four scope workers and eight substrate calls. It
// retries durable intents, including on restart, without retaining all scopes.
// The owner must wait for this method before closing Sessions or its backend.
// A substrate ignoring context may delay return; no replacement workers spawn.
func (s *Sessions) RunNaming(ctx context.Context, backend NamingBackend, logf func(string, ...any)) {
	if backend == nil {
		return
	}
	var after client.Scope
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		query, cancel := context.WithTimeout(ctx, 2*time.Second)
		scopes, err := s.RegistrationScopes(query, after)
		cancel()
		if err != nil {
			if ctx.Err() == nil && logf != nil {
				logf("agent: Naming scope scan unavailable")
			}
			timer.Reset(time.Second)
			continue
		}
		jobs := make(chan client.Scope, len(scopes))
		for _, scope := range scopes {
			jobs <- scope
		}
		close(jobs)
		var workers sync.WaitGroup
		var incomplete atomic.Bool
		for range min(4, len(scopes)) {
			workers.Go(func() {
				for scope := range jobs {
					if ctx.Err() != nil {
						return
					}
					attempt, done := context.WithTimeout(ctx, 5*time.Second)
					_, err := s.ReconcileNaming(attempt, scope, backend, 2)
					done()
					if err != nil && ctx.Err() == nil {
						incomplete.Store(true)
					}
				}
			})
		}
		workers.Wait()
		if incomplete.Load() && logf != nil {
			logf("agent: Naming reconciliation page incomplete")
		}
		if len(scopes) < 100 {
			after = client.Scope{}
		} else {
			after = scopes[len(scopes)-1]
		}
		timer.Reset(500 * time.Millisecond)
	}
}
