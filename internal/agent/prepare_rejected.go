package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

type PrepareRejectedReceipt struct {
	CommandID             string
	Generation, Revision  uint64
	ReceivedAt, ExpiresAt time.Time
	Report                *agentv1.Rejected
}

// RecordPrepareRejected binds a negative outcome to the same current target as
// Prepared. Ready/Draining cannot erase a negative report. One command has exactly
// one immutable outcome; recovery requires an explicit new target revision.
func (s *Sessions) RecordPrepareRejected(ctx context.Context, se Session, command string, report *agentv1.Rejected, v *client.Verifier) (PrepareRejectedReceipt, error) {
	if !identifier(command) || report == nil || report.Release == nil || report.Release.Generation == 0 || report.Error == nil || report.Error.Code == 0 || proto.Size(report) > 32<<10 || v == nil {
		return PrepareRejectedReceipt{}, denied()
	}
	if _, ok := agentv1.ErrorCode_name[int32(report.Error.Code)]; !ok || len(report.Error.Reason) > 1024 || !utf8.ValidString(report.Error.Reason) || strings.TrimSpace(report.Error.Reason) == "" || strings.ContainsRune(report.Error.Reason, 0) {
		return PrepareRejectedReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid rejection")
	}
	report = proto.Clone(report).(*agentv1.Rejected)
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(report)
	if err != nil {
		return PrepareRejectedReceipt{}, err
	}
	hash := sha256.Sum256(append([]byte("grayling.agent.v1.prepare-rejected\x00"), body...))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PrepareRejectedReceipt{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead|ReleaseReport); err != nil {
		return PrepareRejectedReceipt{}, err
	}
	scope := se.Grant.Scope
	if _, err := readRegistrationScope(ctx, tx, scope); err != nil {
		return PrepareRejectedReceipt{}, err
	}
	r, now, err := lockPrepareCommandRelease(ctx, tx, scope, report.Release.Generation)
	if err != nil {
		return PrepareRejectedReceipt{}, err
	}
	instance, err := readInstance(ctx, tx, scope, se.Grant.InstanceID)
	if err != nil {
		return PrepareRejectedReceipt{}, err
	}
	intent, _, err := readPrepareIntent(ctx, tx, scope, command)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrepareRejectedReceipt{}, denied()
	}
	if err != nil {
		return PrepareRejectedReceipt{}, err
	}
	if intent.Generation != r.Generation || !matchesPrepareTarget(intent, se, instance) || !instance.active {
		return PrepareRejectedReceipt{}, fenced()
	}
	var head, revision string
	if err := tx.QueryRow(ctx, `SELECT command_id,revision::text FROM agent_prepare_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), se.Grant.InstanceID).Scan(&head, &revision); err != nil {
		return PrepareRejectedReceipt{}, err
	}
	if head != command {
		return PrepareRejectedReceipt{}, fenced()
	}
	if revision != strconv.FormatUint(intent.TargetRevision, 10) {
		return PrepareRejectedReceipt{}, client.ErrResource
	}
	if report.Release.ReleaseId != r.ReleaseID || report.Release.BundleHash != r.Definition.BundleHash {
		return PrepareRejectedReceipt{}, denied()
	}
	expiry := time.UnixMilli(min(intent.ExpiresAt.UnixMilli(), instance.expiresAt.UnixMilli(), se.Grant.ExpiresAt.UnixMilli(), r.ResourcesExpiresAt.UnixMilli())).UTC()
	if !expiry.After(now) {
		return PrepareRejectedReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "preparation authorization expired")
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return PrepareRejectedReceipt{}, err
	}
	var positive bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_prepared_evidence WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND command_id=$5)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), command).Scan(&positive); err != nil {
		return PrepareRejectedReceipt{}, err
	}
	if positive {
		return PrepareRejectedReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "preparation already accepted")
	}
	receipt := PrepareRejectedReceipt{CommandID: command, Generation: r.Generation, Report: report}
	var oldHash, oldBody []byte
	err = tx.QueryRow(ctx, `SELECT revision::text,report_hash,report,received_at,expires_at FROM agent_prepare_rejections WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND command_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), command).Scan(&revision, &oldHash, &oldBody, &receipt.ReceivedAt, &receipt.ExpiresAt)
	if err == nil {
		if !bytes.Equal(oldHash, hash[:]) || !bytes.Equal(oldBody, body) {
			return receipt, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "rejection report changed")
		}
		receipt.Revision, err = strconv.ParseUint(revision, 10, 64)
		if err != nil || receipt.Revision == 0 {
			return receipt, client.ErrResource
		}
		receipt.ReceivedAt = receipt.ReceivedAt.UTC()
		receipt.ExpiresAt = receipt.ExpiresAt.UTC()
	} else if errors.Is(err, pgx.ErrNoRows) {
		receipt.Revision, err = nextPreparedRevision(ctx, tx, scope, r.Generation)
		if err != nil {
			return receipt, err
		}
		receipt.ReceivedAt = now.Truncate(time.Millisecond).UTC()
		receipt.ExpiresAt = expiry
		inserted, err := tx.Exec(ctx, `INSERT INTO agent_prepare_rejections VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), command, strconv.FormatUint(receipt.Revision, 10), hash[:], body, receipt.ReceivedAt, receipt.ExpiresAt)
		if err != nil {
			return receipt, err
		}
		if inserted.RowsAffected() != 1 {
			return receipt, client.ErrResource
		}
	} else {
		return receipt, err
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return receipt, err
	}
	if err := preparationStillFresh(ctx, tx, now, receipt.ExpiresAt); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}
