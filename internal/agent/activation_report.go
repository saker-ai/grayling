package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func (s *Sessions) RecordApplied(ctx context.Context, se Session, command string, report *agentv1.Applied, v *client.Verifier) (*agentv1.ActivationReportAck, error) {
	return s.recordActivationReport(ctx, se, command, report, v)
}
func (s *Sessions) RecordActivationRejected(ctx context.Context, se Session, command string, report *agentv1.Rejected, v *client.Verifier) (*agentv1.ActivationReportAck, error) {
	return s.recordActivationReport(ctx, se, command, report, v)
}
func activationReportHash(kind string, body []byte) [32]byte {
	return sha256.Sum256(append([]byte("grayling.agent.v1.activation-"+kind+"\x00"), body...))
}

// One source command has one immutable outcome. New evidence enters VERIFYING,
// never ACTIVE, and atomically advances both report and release revisions.
// Lock order: credential -> session -> registration scope -> release -> schema.
func (s *Sessions) recordActivationReport(ctx context.Context, se Session, command string, input proto.Message, v *client.Verifier) (*agentv1.ActivationReportAck, error) {
	if !identifier(command) || input == nil || !input.ProtoReflect().IsValid() || proto.Size(input) > 32<<10 || v == nil {
		return nil, denied()
	}
	input = proto.Clone(input)
	var identity *agentv1.ReleaseIdentity
	kind := "applied"
	negative := false
	var artifactRevision uint64
	switch report := input.(type) {
	case *agentv1.Applied:
		identity = report.Release
		artifactRevision = report.ActivationArtifactRevision
		if report.InstanceId != se.Grant.InstanceID || report.BootId != se.Grant.BootID {
			return nil, denied()
		}
	case *agentv1.Rejected:
		identity = report.Release
		artifactRevision = report.ActivationArtifactRevision
		kind = "rejected"
		negative = true
		if report.InstanceId != se.Grant.InstanceID || report.BootId != se.Grant.BootID || report.Error == nil {
			return nil, denied()
		}
		if _, known := agentv1.ErrorCode_name[int32(report.Error.Code)]; !known || report.Error.Code == 0 || strings.TrimSpace(report.Error.Reason) == "" || len(report.Error.Reason) > 1024 || !utf8.ValidString(report.Error.Reason) || strings.ContainsRune(report.Error.Reason, 0) {
			return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid activation rejection")
		}
	default:
		return nil, denied()
	}
	if identity == nil || identity.Generation == 0 || artifactRevision == 0 && !negative {
		return nil, denied()
	}
	// Legacy v10 NACKs omitted the artifact. They may replay exact persisted
	// outcomes for artifact 1, but can never create a new unbound outcome.
	legacy := artifactRevision == 0
	if legacy {
		artifactRevision = 1
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(input)
	if err != nil {
		return nil, err
	}
	hash := activationReportHash(kind, body)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead|DiscoveryRead|ReleaseReport); err != nil {
		return nil, err
	}
	locked, err := lockActivationSource(ctx, tx, se, identity.Generation, artifactRevision, v, !negative)
	if err != nil {
		return nil, err
	}
	r := locked.Release
	d := locked.View.Document()
	scope := se.Grant.Scope
	g := strconv.FormatUint(r.Generation, 10)
	if command != locked.Source.CommandID {
		return nil, fenced()
	}
	if identity.ReleaseId != r.ReleaseID || identity.BundleHash != r.Definition.BundleHash {
		return nil, denied()
	}
	if report, ok := input.(*agentv1.Applied); ok {
		if report.PolicyRevision != d.PolicyRevision || report.DiscoveryRevision != d.DiscoveryRevision || report.PreparedRevision != d.PreparedRevision || report.RevocationEpoch != d.RevocationEpoch || report.ActivationArtifactRevision != locked.Resource.Revision || report.ControlPlaneEpoch != r.ControlPlaneEpoch {
			return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "Applied view versions differ from activation")
		}
	}
	old, err := readActivationReport(ctx, tx, scope, r.Generation, se.Grant.InstanceID, artifactRevision)
	if err == nil {
		if old.command != command || old.kind != kind || !bytes.Equal(old.hash, hash[:]) || !bytes.Equal(old.body, body) {
			return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "activation outcome changed")
		}
		if err := lockedSessionStillValid(ctx, tx, se); err != nil {
			return nil, err
		}
		if err := preparationStillFresh(ctx, tx, locked.Now, locked.Resource.ExpiresAt); err != nil {
			return nil, err
		}
		return old.ack, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if legacy {
		return nil, denied()
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT resource_revision::text FROM agent_activations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g).Scan(&current); err != nil {
		return nil, err
	}
	if current != strconv.FormatUint(artifactRevision, 10) {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "activation artifact changed")
	}
	if r.State == agentv1.ReleaseState_RELEASE_STATE_ACTIVE {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "active release cannot accept new outcomes")
	}
	if r.Revision == math.MaxUint64 {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "release revision exhausted")
	}
	revision, err := nextActivationReportRevision(ctx, tx, scope, r.Generation)
	if err != nil {
		return nil, err
	}
	from := r.State
	r.State = agentv1.ReleaseState_RELEASE_STATE_VERIFYING
	r.Revision++
	updated, err := tx.Exec(ctx, `UPDATE agent_releases SET state=$5,revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, int32(r.State), strconv.FormatUint(r.Revision, 10))
	if err != nil {
		return nil, err
	}
	if updated.RowsAffected() != 1 {
		return nil, client.ErrResource
	}
	if err := releaseEvent(ctx, tx, r, from, se.Grant.PrincipalID, command, "source "+kind+" activation"); err != nil {
		return nil, err
	}
	received := locked.Now.Truncate(time.Millisecond).UTC()
	ack := &agentv1.ActivationReportAck{Release: identity, CommandId: command, ReportRevision: revision, ReceivedUnixMs: received.UnixMilli(), Rejected: negative, State: r.State, ReleaseRevision: r.Revision}
	receipt, err := (proto.MarshalOptions{Deterministic: true}).Marshal(ack)
	if err != nil {
		return nil, err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_activation_reports VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, se.Grant.InstanceID, command, kind, strconv.FormatUint(revision, 10), hash[:], body, received, receipt, strconv.FormatUint(artifactRevision, 10))
	if err != nil {
		return nil, err
	}
	if inserted.RowsAffected() != 1 {
		return nil, client.ErrResource
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return nil, err
	}
	if err := preparationStillFresh(ctx, tx, locked.Now, locked.Resource.ExpiresAt); err != nil {
		return nil, err
	}
	return ack, tx.Commit(ctx)
}

func nextActivationReportRevision(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64) (uint64, error) {
	g := strconv.FormatUint(generation, 10)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_activation_report_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g); err != nil {
		return 0, err
	}
	var text string
	if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_activation_report_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g).Scan(&text); err != nil {
		return 0, err
	}
	revision, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, err
	}
	if revision == math.MaxUint64 {
		return 0, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "activation report revision exhausted")
	}
	revision++
	changed, err := tx.Exec(ctx, `UPDATE agent_activation_report_heads SET revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, strconv.FormatUint(revision, 10))
	if err != nil {
		return 0, err
	}
	if changed.RowsAffected() != 1 {
		return 0, client.ErrResource
	}
	return revision, nil
}

type activationReportRecord struct {
	command, kind string
	hash, body    []byte
	ack           *agentv1.ActivationReportAck
	report        proto.Message
}

func readActivationReport(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64, instance string, artifactRevision uint64) (activationReportRecord, error) {
	var record activationReportRecord
	var receipt []byte
	var revision string
	var received time.Time
	err := tx.QueryRow(ctx, `SELECT command_id,outcome,revision::text,report_hash,report,received_at,receipt FROM agent_activation_reports WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5 AND artifact_revision=$6`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(generation, 10), instance, strconv.FormatUint(artifactRevision, 10)).Scan(&record.command, &record.kind, &revision, &record.hash, &record.body, &received, &receipt)
	if err != nil {
		return record, err
	}
	digest := activationReportHash(record.kind, record.body)
	record.ack = new(agentv1.ActivationReportAck)
	if len(record.body) > 32<<10 || !bytes.Equal(digest[:], record.hash) || proto.Unmarshal(receipt, record.ack) != nil {
		return record, client.ErrResource
	}
	ack := record.ack
	if ack.Release == nil || ack.Release.Generation != generation || ack.Release.ReleaseId != releaseID(generation) || ack.CommandId != record.command || ack.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || ack.ReleaseRevision == 0 || ack.ReceivedUnixMs != received.UnixMilli() || strconv.FormatUint(ack.ReportRevision, 10) != revision || ack.ReportRevision == 0 {
		return record, client.ErrResource
	}
	var identity *agentv1.ReleaseIdentity
	switch record.kind {
	case "applied":
		report := new(agentv1.Applied)
		if proto.Unmarshal(record.body, report) != nil || ack.Rejected || report.InstanceId != instance || report.ActivationArtifactRevision != artifactRevision {
			return record, client.ErrResource
		}
		record.report = report
		identity = report.Release
	case "rejected":
		report := new(agentv1.Rejected)
		if proto.Unmarshal(record.body, report) != nil || !ack.Rejected || report.InstanceId != instance || report.Error == nil || report.ActivationArtifactRevision != artifactRevision && !(report.ActivationArtifactRevision == 0 && artifactRevision == 1) {
			return record, client.ErrResource
		}
		record.report = report
		identity = report.Release
	default:
		return record, client.ErrResource
	}
	if !proto.Equal(identity, ack.Release) {
		return record, client.ErrResource
	}
	return record, nil
}
