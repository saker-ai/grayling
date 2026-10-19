package agent

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

// The release head serializes both positive and negative evidence writes.
func nextPreparedRevision(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64) (uint64, error) {
	g := strconv.FormatUint(generation, 10)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_prepared_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g); err != nil {
		return 0, err
	}
	var text string
	if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_prepared_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g).Scan(&text); err != nil {
		return 0, err
	}
	revision, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, err
	}
	if revision == math.MaxUint64 {
		return 0, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "preparation revision exhausted")
	}
	revision++
	updated, err := tx.Exec(ctx, `UPDATE agent_prepared_heads SET revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, strconv.FormatUint(revision, 10))
	if err != nil {
		return 0, err
	}
	if updated.RowsAffected() != 1 {
		return 0, client.ErrResource
	}
	return revision, nil
}

func preparationStillFresh(ctx context.Context, tx pgx.Tx, started, expires time.Time) error {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	local := time.Now()
	if now.Before(started) || now.Before(local.Add(-5*time.Second)) || now.After(local.Add(5*time.Second)) || !expires.After(now) {
		return protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "preparation time authority changed or expired")
	}
	return nil
}
