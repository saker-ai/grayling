package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RolloutLeadership is the database-clock view of the one active rollout worker.
// Generation changes on every takeover and fences a previous holder.
type RolloutLeadership struct {
	Holder         string     `json:"holder"`
	Generation     int64      `json:"generation"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	LastRunAt      *time.Time `json:"lastRunAt,omitempty"`
	LastTakeoverAt *time.Time `json:"lastTakeoverAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
}

type rolloutLeaseBackend interface {
	acquireRollout(context.Context, string, time.Duration) (int64, bool, error)
	checkRollout(context.Context, string, int64) error
	finishRollout(context.Context, string, int64, string) error
	rolloutStatus(context.Context) (RolloutLeadership, error)
}

var ErrRolloutLeaseLost = errors.New("rollout lease lost")

// AcquireRolloutLease elects one worker using PostgreSQL's clock. File and
// memory stores have only one process and require no election.
func (s *Store) AcquireRolloutLease(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
	if p, ok := s.persist.(rolloutLeaseBackend); ok {
		return p.acquireRollout(ctx, holder, ttl)
	}
	return 1, true, nil
}

func (s *Store) CheckRolloutLease(ctx context.Context, holder string, generation int64) error {
	if p, ok := s.persist.(rolloutLeaseBackend); ok {
		return p.checkRollout(ctx, holder, generation)
	}
	return nil
}

func (s *Store) FinishRolloutLease(ctx context.Context, holder string, generation int64, runError string) error {
	if p, ok := s.persist.(rolloutLeaseBackend); ok {
		return p.finishRollout(ctx, holder, generation, runError)
	}
	return nil
}

func (s *Store) RolloutLeadership(ctx context.Context) (RolloutLeadership, error) {
	if p, ok := s.persist.(rolloutLeaseBackend); ok {
		return p.rolloutStatus(ctx)
	}
	return RolloutLeadership{Holder: "local"}, nil
}

func (p *pgPersistence) acquireRollout(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
	if holder == "" || ttl <= 0 {
		return 0, false, fmt.Errorf("invalid rollout lease holder or duration")
	}
	var generation int64
	err := p.pool.QueryRow(ctx, `INSERT INTO rollout_leader (id, holder, generation, expires_at, last_takeover_at)
		VALUES (true, $1, 1, clock_timestamp() + $2::interval, clock_timestamp())
		ON CONFLICT (id) DO UPDATE SET
			holder = EXCLUDED.holder,
			generation = rollout_leader.generation + CASE WHEN rollout_leader.expires_at <= clock_timestamp() THEN 1 ELSE 0 END,
			expires_at = clock_timestamp() + $2::interval,
			last_takeover_at = CASE WHEN rollout_leader.expires_at <= clock_timestamp() THEN clock_timestamp() ELSE rollout_leader.last_takeover_at END
		WHERE rollout_leader.expires_at <= clock_timestamp() OR rollout_leader.holder = EXCLUDED.holder
		RETURNING generation`, holder, ttl).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("acquire rollout lease: %w", err)
	}
	return generation, true, nil
}

func (p *pgPersistence) checkRollout(ctx context.Context, holder string, generation int64) error {
	var valid bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM rollout_leader WHERE id AND holder = $1 AND generation = $2
		AND expires_at > clock_timestamp())`, holder, generation).Scan(&valid)
	if err != nil {
		return fmt.Errorf("check rollout lease: %w", err)
	}
	if !valid {
		return ErrRolloutLeaseLost
	}
	return nil
}

func (p *pgPersistence) finishRollout(ctx context.Context, holder string, generation int64, runError string) error {
	cmd, err := p.pool.Exec(ctx, `UPDATE rollout_leader SET last_run_at = clock_timestamp(),
		last_error = $3
		WHERE id AND holder = $1 AND generation = $2 AND expires_at > clock_timestamp()`, holder, generation, runError)
	if err != nil {
		return fmt.Errorf("finish rollout lease: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrRolloutLeaseLost
	}
	return nil
}

func (p *pgPersistence) rolloutStatus(ctx context.Context) (RolloutLeadership, error) {
	var status RolloutLeadership
	err := p.pool.QueryRow(ctx, `SELECT CASE WHEN expires_at > clock_timestamp() THEN holder ELSE '' END,
		generation, expires_at, last_run_at, last_takeover_at, last_error
		FROM rollout_leader WHERE id`).Scan(&status.Holder, &status.Generation,
		&status.ExpiresAt, &status.LastRunAt, &status.LastTakeoverAt, &status.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return status, fmt.Errorf("read rollout leadership: %w", err)
	}
	return status, nil
}
