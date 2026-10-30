//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func appliedFixture(t *testing.T) (*registrationFixture, *client.Verifier, *control.ActivationDispatch) {
	t.Helper()
	f, p, v, _ := activationFixture(t)
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	pending, err := f.a.PendingActivation(t.Context(), f.session, v)
	if err != nil || pending == nil {
		t.Fatal("activation dispatch missing", err)
	}
	return f, v, pending
}
func appliedReport(f *registrationFixture, p *control.ActivationDispatch) *agentv1.Applied {
	d := p.Document
	return &agentv1.Applied{Release: &agentv1.ReleaseIdentity{ReleaseId: p.Release.ReleaseID, Generation: p.Release.Generation, BundleHash: p.Release.Definition.BundleHash}, InstanceId: f.g.InstanceID, BootId: f.g.BootID, PolicyRevision: d.PolicyRevision, DiscoveryRevision: d.DiscoveryRevision, PreparedRevision: d.PreparedRevision, RevocationEpoch: d.RevocationEpoch, ActivationArtifactRevision: p.Resource.Revision, ControlPlaneEpoch: p.Release.ControlPlaneEpoch}
}
func activationRejection(f *registrationFixture, p *control.ActivationDispatch) *agentv1.Rejected {
	r := rejectionReport(p.Release)
	r.InstanceId = f.g.InstanceID
	r.BootId = f.g.BootID
	r.ActivationArtifactRevision = p.Resource.Revision
	return r
}

func TestAppliedReplicaImmutableReceiptStatusAndNoActive(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	report := appliedReport(f, p)
	out := make(chan *agentv1.ActivationReportAck, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			node := f.a
			if i%2 != 0 {
				node = f.b
			}
			ack, err := node.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
			if err != nil {
				t.Error(err)
				return
			}
			out <- ack
		})
	}
	wg.Wait()
	close(out)
	var first *agentv1.ActivationReportAck
	for ack := range out {
		if first == nil {
			first = ack
		}
		if !proto.Equal(first, ack) {
			t.Fatal("replay changed receipt")
		}
	}
	if first == nil || first.ReportRevision != 1 || first.ReleaseRevision != 4 || first.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || first.Rejected {
		t.Fatal("invalid Applied receipt", first)
	}
	pending, err := f.b.PendingActivation(t.Context(), f.session, v)
	if err != nil || pending != nil {
		t.Fatal("reported source redelivered", err)
	}
	cold := openSessions(t, f.dsn)
	again, err := cold.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !proto.Equal(first, again) {
		t.Fatal("cold replay", err)
	}
	status, err := cold.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID, PageSize: 1})
	if err != nil || status.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || status.ReportRevision != 1 || status.RequiredSources != 1 || len(status.Applied) != 1 || len(status.PendingSourceIds) != 0 || status.More {
		t.Fatal("status inferred ACTIVE or lost report", status, err)
	}
	_, err = f.a.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, activationRejection(f, p), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	_, err = f.a.AbortRelease(t.Context(), control.AbortReleaseRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "no-undo", Generation: 1, ExpectedRevision: 4, Reason: "already applied"})
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activation_reports`)[0][0]; n != "1" {
		t.Fatal("duplicate outcome", n)
	}
}
func TestAppliedVersionsIdentityAndOutcomeRace(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	report := appliedReport(f, p)
	for _, change := range []func(*agentv1.Applied){func(r *agentv1.Applied) { r.BootId = "old" }, func(r *agentv1.Applied) { r.InstanceId = "other" }, func(r *agentv1.Applied) { r.Release.BundleHash = strings.Repeat("f", 64) }, func(r *agentv1.Applied) { r.PolicyRevision++ }, func(r *agentv1.Applied) { r.DiscoveryRevision++ }, func(r *agentv1.Applied) { r.PreparedRevision++ }, func(r *agentv1.Applied) { r.RevocationEpoch++ }, func(r *agentv1.Applied) { r.ActivationArtifactRevision++ }, func(r *agentv1.Applied) { r.ControlPlaneEpoch++ }} {
		bad := proto.Clone(report).(*agentv1.Applied)
		change(bad)
		if _, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, bad, v); err == nil {
			t.Fatal("inconsistent Applied accepted")
		}
	}
	if _, err := f.a.RecordApplied(t.Context(), f.session, "activate-unsolicited", report, v); err == nil {
		t.Fatal("unsolicited command accepted")
	}
	var positive, negative error
	var wg sync.WaitGroup
	wg.Go(func() { _, positive = f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v) })
	wg.Go(func() {
		_, negative = f.b.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, activationRejection(f, p), v)
	})
	wg.Wait()
	if (positive == nil) == (negative == nil) {
		t.Fatal("outcomes not exclusive", positive, negative)
	}
	failure := positive
	if positive == nil {
		failure = negative
	}
	expectCode(t, failure, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
}
func TestAppliedWriteRollbackDeadlineAndNegativeReadiness(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	report := appliedReport(f, p)
	assertClean := func() {
		t.Helper()
		r, err := f.b.Release(t.Context(), f.g.Scope, 1)
		if err != nil || r.Revision != 3 || r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING {
			t.Fatal("partial report state", r, err)
		}
		if n := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_activation_reports)+(SELECT count(*) FROM agent_activation_report_heads)`)[0][0]; n != "0" {
			t.Fatal("partial report", n)
		}
	}
	for _, table := range []string{"agent_activation_report_heads", "agent_activation_reports", "agent_release_events"} {
		pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_applied() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; CREATE TRIGGER suppress_applied BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION suppress_applied()`)
		if _, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v); err == nil {
			t.Fatal("suppressed write committed", table)
		}
		assertClean()
		pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_applied ON `+table+`; DROP FUNCTION suppress_applied()`)
	}
	db, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `LOCK TABLE agent_activation_reports IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	_, err = f.a.RecordApplied(ctx, f.session, p.Source.CommandID, report, v)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("report ignored deadline", err)
	}
	tx.Rollback(t.Context())
	assertClean()
	instance := proto.Clone(f.request.Instance).(*agentv1.Instance)
	instance.Lease = &agentv1.Lease{Id: p.Source.LeaseID, Epoch: p.Source.LeaseEpoch, ExpiresUnixMs: p.Document.Members[0].Instance.Lease.ExpiresUnixMs}
	instance.Ready = false
	instance.Draining = true
	if _, err := f.a.UpdateRegisteredInstance(t.Context(), f.session, "installation-failed", &agentv1.UpdateInstance{Instance: instance}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v); err == nil {
		t.Fatal("unready source claimed Applied")
	}
	ack, err := f.b.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, activationRejection(f, p), v)
	if err != nil || !ack.Rejected || ack.ReportRevision != 1 {
		t.Fatal("unready source could not report failure", err)
	}
}

func TestAppliedV9MigrationPreservesActivationAndUnknownFields(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	original := p.Resource.Body
	f.a.Close()
	f.b.Close()
	activationV10Fixture(t, f.dsn)
	pgtest.Exec(t, f.dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads; UPDATE agent_schema SET version=9 WHERE id=true`)
	f.a = openSessions(t, f.dsn)
	f.b = openSessions(t, f.dsn)
	pending, err := f.b.PendingActivation(t.Context(), f.session, v)
	if err != nil || pending == nil || !bytes.Equal(original, pending.Resource.Body) {
		t.Fatal("v9 migration lost activation", err)
	}
	report := appliedReport(f, p)
	optional := protowire.AppendTag(nil, 99, protowire.BytesType)
	optional = protowire.AppendString(optional, "optional")
	report.Release.ProtoReflect().SetUnknown(optional)
	first, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.b.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !proto.Equal(first, again) {
		t.Fatal("optional field changed replay", err)
	}
	status, err := f.b.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || len(status.Applied) != 1 || len(status.Applied[0].Release.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("bounded known-schema status", status, err)
	}
	if n := pgtest.Query(t, f.dsn, `SELECT version FROM agent_schema WHERE id=true`)[0][0]; n != "11" {
		t.Fatal("migration version", n)
	}
}

func TestAppliedExpiredAuthorizationKeepsDiagnosticFact(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, _ := activationFixture(t)
	publisher, err := control.NewPublisher(f.a, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	pending, err := f.a.PendingActivation(t.Context(), f.session, v)
	if err != nil || pending == nil {
		t.Fatal(err)
	}
	report := appliedReport(f, pending)
	if _, err := f.a.RecordApplied(t.Context(), f.session, pending.Source.CommandID, report, v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(pending.Resource.ExpiresAt) + 10*time.Millisecond)
	if _, err := f.b.RecordApplied(t.Context(), f.session, pending.Source.CommandID, report, v); err == nil {
		t.Fatal("expired activation report authority renewed")
	}
	status, err := f.b.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: pending.Release.ReleaseID})
	if err != nil || len(status.Applied) != 1 || status.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING {
		t.Fatal("expiry erased historical fact or inferred ACTIVE", status, err)
	}
}

func TestAppliedUint64CountersFailClosed(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	report := appliedReport(f, p)
	pgtest.Exec(t, f.dsn, `INSERT INTO agent_activation_report_heads VALUES($1,$2,$3,1,18446744073709551615)`, f.g.Scope.WorkspaceID, f.g.Scope.EnvironmentID, f.g.Scope.ServiceKey)
	_, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	pgtest.Exec(t, f.dsn, `DELETE FROM agent_activation_report_heads; UPDATE agent_releases SET revision=18446744073709551615 WHERE generation=1`)
	_, err = f.b.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activation_reports`)[0][0]; n != "0" {
		t.Fatal("overflow committed report")
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_releases SET revision=3 WHERE generation=1`)
	ack, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || ack.ReportRevision != 1 || ack.ReleaseRevision != 4 {
		t.Fatal("overflow wrapped or left partial counter", err)
	}
}

func TestAppliedOldLeaseCrossScopeAndActiveReplayBarriers(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	report := appliedReport(f, p)
	foreign := f.g
	foreign.Scope.WorkspaceID = "foreign-workspace"
	if err := f.a.SetScopeLimit(t.Context(), foreign.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, f.a, foreign)
	outsider, err := f.a.Connect(t.Context(), secret, helloFor(foreign))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.RecordApplied(t.Context(), outsider, p.Source.CommandID, report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	if _, err := f.a.ActivationStatus(t.Context(), outsider, &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID}); err == nil {
		t.Fatal("cross-scope status disclosed")
	}
	ack, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture-only state injection exercises historical receipt replay after the
	// future probe gate has completed; this does not simulate actual probes.
	pgtest.Exec(t, f.dsn, `UPDATE agent_releases SET state=$1,revision=5 WHERE generation=1`, int32(agentv1.ReleaseState_RELEASE_STATE_ACTIVE))
	same, err := f.b.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !proto.Equal(ack, same) {
		t.Fatal("ACTIVE replay changed history", err)
	}
	if _, err := f.a.RegisterInstance(t.Context(), f.session, "lease-rotation-after-activation", f.request); err != nil {
		t.Fatal(err)
	}
	_, err = f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	pending, err := f.b.PendingActivation(t.Context(), f.session, v)
	if err != nil || pending != nil {
		t.Fatal("rotated lease inherited command", err)
	}
}

func TestActivationRejectedExactArtifactDoesNotConsumeCommand(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	report := activationRejection(f, p)
	for _, revision := range []uint64{0, 2, ^uint64(0)} {
		report.ActivationArtifactRevision = revision
		if _, err := f.a.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, report, v); err == nil {
			t.Fatalf("accepted rejection for artifact %d", revision)
		}
		pending, err := f.b.PendingActivation(t.Context(), f.session, v)
		if err != nil || pending == nil || pending.Source.CommandID != p.Source.CommandID {
			t.Fatal("invalid rejection consumed current activation", pending, err)
		}
	}
	report.ActivationArtifactRevision = p.Resource.Revision
	ack, err := f.b.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !ack.Rejected || ack.ReportRevision != 1 {
		t.Fatal("exact artifact rejection failed", ack, err)
	}
	replay, err := f.a.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !proto.Equal(ack, replay) {
		t.Fatal("exact rejection replay changed", replay, err)
	}
}
