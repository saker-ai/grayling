package platform

import (
	"context"
	"encoding/json/v2"
	"errors"

	"github.com/jackc/pgx/v5"
)

// LockPostgresTokenMetadata reads the production GORM token row in a caller's
// PostgreSQL effect transaction. Its share lock lasts until that transaction
// ends, serializing a new authorized effect against revocation and role changes.
// The transaction must use the same database as this platform store. This does
// not support the legacy pgx schema or grant authorization by itself.
func LockPostgresTokenMetadata(ctx context.Context, tx pgx.Tx, tokenID string) (*Token, error) {
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM database_tokens WHERE token_id=$1 FOR SHARE`, tokenID).Scan(&payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccessDenied
		}
		return nil, err
	}
	var token Token
	if err := json.Unmarshal(payload, &token, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if token.ID != tokenID || token.Revoked {
		return nil, ErrAccessDenied
	}
	token.SecretHash = ""
	return &token, nil
}
