package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const journalLease = 30 * time.Second
const journalAckInterval = 10 * time.Second
const journalPruneInterval = time.Minute
const journalPruneVersions = 1024

// ackJournal confirms a fully applied version. A dead process stops renewing
// its lease; recovery checks the durable watermark before reading changes.
func (p *pgPersistence) ackJournal(ctx context.Context, cursor int64) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO control_replica_cursors(replica_id,cursor,heartbeat_at)
 VALUES ($1,$2,clock_timestamp()) ON CONFLICT(replica_id) DO UPDATE SET
 cursor=GREATEST(control_replica_cursors.cursor,EXCLUDED.cursor), heartbeat_at=clock_timestamp()`, p.replicaID, cursor)
	if err != nil {
		return fmt.Errorf("platform: confirm journal cursor: %w", err)
	}
	p.lastAck = time.Now()
	p.ackCursor = max(p.ackCursor, cursor)
	return nil
}

func (p *pgPersistence) maintainJournal(ctx context.Context, cursor int64, metrics *syncMetrics) error {
	if cursor > p.ackCursor || time.Since(p.lastAck) >= journalAckInterval {
		if err := p.ackJournal(ctx, cursor); err != nil {
			return err
		}
	}
	if time.Since(p.lastPrune) >= journalPruneInterval {
		if err := p.pruneJournal(ctx, metrics); err != nil {
			return err
		}
		p.lastPrune = time.Now()
	}
	return nil
}

// pruneJournal serializes cleaners on the watermark row. The deletion and
// watermark update commit together, so a recovering replica sees either the
// old complete journal or a watermark requiring a full reload.
func (p *pgPersistence) pruneJournal(ctx context.Context, metrics *syncMetrics) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var watermark, latest int64
	if err = tx.QueryRow(ctx, `SELECT pruned_through FROM control_journal_watermark WHERE id=true FOR UPDATE`).Scan(&watermark); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT control_version FROM meta WHERE id=1`).Scan(&latest); err != nil {
		return err
	}
	var oldest int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MIN(cursor),$1) FROM control_replica_cursors WHERE heartbeat_at > clock_timestamp()-interval '30 seconds'`, latest).Scan(&oldest); err != nil {
		return err
	}
	target := min(latest, oldest, watermark+journalPruneVersions)
	if target > watermark {
		if _, err = tx.Exec(ctx, `DELETE FROM control_changes WHERE version<=$1`, target); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE control_journal_watermark SET pruned_through=$1 WHERE id=true`, target); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM control_replica_cursors WHERE heartbeat_at <= clock_timestamp()-interval '30 seconds'`); err != nil {
		return err
	}
	var rows, bytes int64
	if err = tx.QueryRow(ctx, `SELECT count(*), pg_total_relation_size('control_changes') FROM control_changes`).Scan(&rows, &bytes); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	metrics.journalRows.Store(rows)
	metrics.journalBytes.Store(bytes)
	metrics.oldestUnconfirmed.Store(oldest)
	metrics.prunedThrough.Store(max(watermark, target))
	return nil
}
