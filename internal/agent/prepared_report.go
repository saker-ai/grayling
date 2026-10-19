package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// RecordPrepared accepts only the current directed command from its current
// authenticated instance. The command ID is the stable report operation ID.
// Lock order: credential -> session -> registration scope -> release head ->
// schema -> instance -> intent/evidence. Prepared never changes release state.
func (s *Sessions) RecordPrepared(ctx context.Context, se Session, command string, report *agentv1.Prepared, v *client.Verifier) (PreparedReceipt, error) {
	if !identifier(command) || report == nil || report.Release == nil || report.Release.Generation == 0 || proto.Size(report) > 32<<10 || v == nil {
		return PreparedReceipt{}, denied()
	}
	report = proto.Clone(report).(*agentv1.Prepared)
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(report)
	if err != nil {
		return PreparedReceipt{}, err
	}
	hash := sha256.Sum256(append([]byte("grayling.agent.v1.prepared-report\x00"), body...))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PreparedReceipt{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead|ReleaseReport); err != nil {
		return PreparedReceipt{}, err
	}
	scope := se.Grant.Scope
	state, err := readRegistrationScope(ctx, tx, scope)
	if err != nil {
		return PreparedReceipt{}, err
	}
	r, now, err := lockPrepareCommandRelease(ctx, tx, scope, report.Release.Generation)
	if err != nil {
		return PreparedReceipt{}, err
	}
	instance, err := readInstance(ctx, tx, scope, se.Grant.InstanceID)
	if err != nil {
		return PreparedReceipt{}, err
	}
	intent, _, err := readPrepareIntent(ctx, tx, scope, command)
	if errors.Is(err, pgx.ErrNoRows) {
		return PreparedReceipt{}, denied()
	}
	if err != nil {
		return PreparedReceipt{}, err
	}
	if intent.Generation != r.Generation || !matchesPrepareTarget(intent, se, instance) || !prepareInstanceValid(instance, state, r) {
		return PreparedReceipt{}, fenced()
	}
	var head, targetRevision string
	if err := tx.QueryRow(ctx, `SELECT command_id,revision::text FROM agent_prepare_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), se.Grant.InstanceID).Scan(&head, &targetRevision); err != nil {
		return PreparedReceipt{}, err
	}
	if head != command {
		return PreparedReceipt{}, fenced()
	}
	if targetRevision != strconv.FormatUint(intent.TargetRevision, 10) {
		return PreparedReceipt{}, client.ErrResource
	}
	if report.Release.ReleaseId != r.ReleaseID || report.Release.BundleHash != r.Definition.BundleHash || report.InstanceId != se.Grant.InstanceID || report.BootId != se.Grant.BootID || report.BuildId != instance.instance.BuildId || report.AcceptedProtocolVersion != instance.instance.ProtocolVersion || report.AppliedGeneration > r.Generation {
		return PreparedReceipt{}, denied()
	}
	if report.PreparedUnixMs <= 0 || report.ExpiresUnixMs <= report.PreparedUnixMs || report.ExpiresUnixMs-report.PreparedUnixMs > releasePreparedTTL.Milliseconds() || report.PreparedUnixMs > now.Add(5*time.Second).UnixMilli() || report.PreparedUnixMs < intent.CreatedAt.Add(-5*time.Second).UnixMilli() {
		return PreparedReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid Prepared time bounds")
	}
	if !instance.expiresAt.After(now) || report.ExpiresUnixMs <= now.UnixMilli() {
		return PreparedReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "Prepared authorization expired")
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return PreparedReceipt{}, err
	}
	var rejected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_prepare_rejections WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND command_id=$5)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), command).Scan(&rejected); err != nil {
		return PreparedReceipt{}, err
	}
	if rejected {
		return PreparedReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "preparation already rejected")
	}
	receipt := PreparedReceipt{CommandID: command, Generation: r.Generation, Report: report}
	var oldHash, oldReport []byte
	var revision string
	err = tx.QueryRow(ctx, `SELECT revision::text,report_hash,report,received_at,expires_at FROM agent_prepared_evidence WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND command_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), command).Scan(&revision, &oldHash, &oldReport, &receipt.ReceivedAt, &receipt.ExpiresAt)
	if err == nil {
		if !bytes.Equal(oldHash, hash[:]) || !bytes.Equal(oldReport, body) {
			return receipt, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "Prepared report changed")
		}
		receipt.Revision, err = strconv.ParseUint(revision, 10, 64)
		if err != nil || receipt.Revision == 0 {
			return receipt, client.ErrResource
		}
		receipt.ReceivedAt = receipt.ReceivedAt.UTC()
		receipt.ExpiresAt = receipt.ExpiresAt.UTC()
		if err := lockedSessionStillValid(ctx, tx, se); err != nil {
			return receipt, err
		}
		if err := preparationStillFresh(ctx, tx, now, time.UnixMilli(min(receipt.ExpiresAt.UnixMilli(), instance.expiresAt.UnixMilli(), se.Grant.ExpiresAt.UnixMilli()))); err != nil {
			return receipt, err
		}
		return receipt, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	if !intent.ExpiresAt.After(now) {
		return receipt, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "prepare command expired")
	}
	receipt.Revision, err = nextPreparedRevision(ctx, tx, scope, r.Generation)
	if err != nil {
		return receipt, err
	}
	receipt.ReceivedAt = now.Truncate(time.Millisecond).UTC()
	receipt.ExpiresAt = time.UnixMilli(min(report.ExpiresUnixMs, now.Add(releasePreparedTTL).UnixMilli(), se.Grant.ExpiresAt.UnixMilli(), r.ResourcesExpiresAt.UnixMilli())).UTC()
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_prepared_evidence VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), command, strconv.FormatUint(receipt.Revision, 10), hash[:], body, receipt.ReceivedAt, receipt.ExpiresAt)
	if err != nil {
		return receipt, err
	}
	if inserted.RowsAffected() != 1 {
		return receipt, client.ErrResource
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return receipt, err
	}
	if err := preparationStillFresh(ctx, tx, now, time.UnixMilli(min(receipt.ExpiresAt.UnixMilli(), intent.ExpiresAt.UnixMilli(), instance.expiresAt.UnixMilli(), se.Grant.ExpiresAt.UnixMilli()))); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}
