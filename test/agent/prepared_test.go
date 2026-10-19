//go:build integration

package agent_test

import (
	"bytes"
	"encoding/json/v2"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func confirmPreparedNaming(t *testing.T, f *registrationFixture) {
	t.Helper()
	items, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Present {
			if err := f.a.ConfirmNamingIntent(t.Context(), item); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func preparedFixture(t *testing.T) (*registrationFixture, *control.Publisher, *client.Verifier, control.ReleaseRecord) {
	t.Helper()
	return preparedFixtureWithCapacity(t, control.ReleaseCapacity{Profile: "demo-v1", RequiredZones: []string{"az-a"}, MinimumPerBuildZone: 1})
}
func preparedFixtureWithCapacity(t *testing.T, capacity control.ReleaseCapacity) (*registrationFixture, *control.Publisher, *client.Verifier, control.ReleaseRecord) {
	t.Helper()
	f := newRegistrationFixtureWithPermissions(t, 2, control.ReleaseReport)
	registerInstance(t, f, "register")
	confirmPreparedNaming(t, f)
	p, v := publisher(t, f.a)
	request := bundlePairRequest(t)
	request.Document.Builds[0].ID = "build-a"
	request.Document.ValueBuilds["backend"] = "build-a"
	pair, err := p.PublishBundle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	release := releaseRequest(pair)
	release.Capacity = capacity
	r, err := p.StartRelease(t.Context(), release, v)
	if err != nil {
		t.Fatal(err)
	}
	return f, p, v, r
}
func preparedReport(f *registrationFixture, r control.ReleaseRecord) *agentv1.Prepared {
	now := time.Now().UnixMilli()
	return &agentv1.Prepared{Release: &agentv1.ReleaseIdentity{ReleaseId: r.ReleaseID, Generation: r.Generation, BundleHash: r.Definition.BundleHash}, InstanceId: f.g.InstanceID, BootId: f.g.BootID, BuildId: "build-a", AcceptedProtocolVersion: 1, PreparedUnixMs: now, ExpiresUnixMs: now + 300000}
}
func preparedBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestPreparedDirectedIntentReplicaDedupAndTargetReplacement(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	report := preparedReport(f, r)
	_, err := f.a.RecordPrepared(t.Context(), f.session, "unsolicited", report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	q := control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "release-controller", OperationID: "prepare-1", Generation: r.Generation, InstanceID: f.g.InstanceID}
	intent, err := f.a.QueuePrepare(t.Context(), q, v)
	if err != nil {
		t.Fatal(err)
	}
	if intent.LeaseEpoch != 1 || intent.Instance.BootId != f.g.BootID || intent.TargetRevision != 1 || intent.CredentialID != f.session.Grant.ID {
		t.Fatal("intent omitted exact target")
	}
	pending, err := f.b.PendingPrepare(t.Context(), f.session, v)
	if err != nil || pending == nil || pending.Intent.CommandID != intent.CommandID || pending.Bundle.Revision != 1 {
		t.Fatal("durable intent not dispatchable", err)
	}
	replay, err := f.b.QueuePrepare(t.Context(), q, v)
	if err != nil || !bytes.Equal(preparedBytes(t, intent), preparedBytes(t, replay)) {
		t.Fatal("intent replay changed target", err)
	}
	changed := q
	changed.InstanceID = "other"
	_, err = f.b.QueuePrepare(t.Context(), changed, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	out := make(chan control.PreparedReceipt, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			s := f.a
			if i%2 != 0 {
				s = f.b
			}
			receipt, err := s.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
			if err != nil {
				t.Error(err)
				return
			}
			out <- receipt
		})
	}
	wg.Wait()
	close(out)
	var first control.PreparedReceipt
	for receipt := range out {
		if first.Revision == 0 {
			first = receipt
		}
		if !bytes.Equal(preparedBytes(t, receipt), preparedBytes(t, first)) {
			t.Fatal("repeat extended Prepared TTL")
		}
	}
	if first.Revision != 1 || first.ExpiresAt.After(r.ResourcesExpiresAt) || first.ExpiresAt.After(first.ReceivedAt.Add(300*time.Second)) || !first.ReceivedAt.Before(first.ExpiresAt) {
		t.Fatal("invalid evidence bounds", first)
	}
	pending, err = f.a.PendingPrepare(t.Context(), f.session, v)
	if err != nil || pending != nil {
		t.Fatal("accepted command was redelivered", err)
	}
	bad := proto.Clone(report).(*agentv1.Prepared)
	bad.ExpiresUnixMs--
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, bad, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	q.OperationID = "prepare-2"
	q.ExpectedTargetRevision = 1
	second, err := f.b.QueuePrepare(t.Context(), q, v)
	if err != nil || second.TargetRevision != 2 {
		t.Fatal("target replacement failed", err)
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	receipt, err := f.b.RecordPrepared(t.Context(), f.session, second.CommandID, preparedReport(f, r), v)
	if err != nil || receipt.Revision != 2 {
		t.Fatal("new target did not allocate evidence revision", err)
	}
	current, err := f.a.Release(t.Context(), f.g.Scope, r.Generation)
	if err != nil || current.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("report inferred capacity or ACTIVE", err)
	}
}
func TestPreparedLeaseRotationReadinessAndOldConnectionFenced(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	q := control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "target", Generation: 1, InstanceID: f.g.InstanceID}
	intent, err := f.a.QueuePrepare(t.Context(), q, v)
	if err != nil {
		t.Fatal(err)
	}
	report := preparedReport(f, r)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v); err != nil {
		t.Fatal(err)
	}
	// Same boot and address with a new lease cannot inherit earlier evidence.
	registered := registerInstance(t, f, "register-again")
	confirmPreparedNaming(t, f)
	if registered.Instance.Lease.Epoch != 2 {
		t.Fatal("lease did not rotate")
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	q.OperationID = "target-new-lease"
	q.ExpectedTargetRevision = 1
	next, err := f.a.QueuePrepare(t.Context(), q, v)
	if err != nil || next.LeaseEpoch != 2 {
		t.Fatal(err)
	}
	draining := proto.Clone(registered.Instance).(*agentv1.Instance)
	draining.Draining = true
	if _, err := f.a.UpdateRegisteredInstance(t.Context(), f.session, "draining", &agentv1.UpdateInstance{Instance: draining}); err != nil {
		t.Fatal(err)
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, next.CommandID, preparedReport(f, r), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	// A replacement connection fences the old report path independently of lease.
	replacement, err := f.b.Connect(t.Context(), f.secret, helloFor(f.g))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, next.CommandID, preparedReport(f, r), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	if replacement.Epoch <= f.session.Epoch {
		t.Fatal("connection did not advance")
	}
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; n != "1" {
		t.Fatal("invalid report persisted evidence", n)
	}
}
func TestPreparedEvidenceInsertFailureRollsBackRevision(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	intent, err := f.a.QueuePrepare(t.Context(), control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "target", Generation: 1, InstanceID: f.g.InstanceID}, v)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_prepared() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; CREATE TRIGGER suppress_prepared BEFORE INSERT ON agent_prepared_evidence FOR EACH ROW EXECUTE FUNCTION suppress_prepared()`)
	report := preparedReport(f, r)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v); err == nil {
		t.Fatal("suppressed evidence accepted")
	}
	for _, table := range []string{"agent_prepared_evidence", "agent_prepared_heads"} {
		if n := pgtest.Query(t, f.dsn, "SELECT count(*) FROM "+table)[0][0]; n != "0" {
			t.Fatal("failed report changed state", table, n)
		}
	}
	pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_prepared ON agent_prepared_evidence`)
	receipt, err := f.b.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	if err != nil || receipt.Revision != 1 {
		t.Fatal("failed operation could not retry", err)
	}
}

func TestPreparedIdentityTimeAndExpiryBarriers(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	q := control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "target", Generation: 1, InstanceID: f.g.InstanceID}
	intent, err := f.a.QueuePrepare(t.Context(), q, v)
	if err != nil {
		t.Fatal(err)
	}
	original := preparedReport(f, r)
	for _, change := range []func(*agentv1.Prepared){func(p *agentv1.Prepared) { p.BootId = "other-boot" }, func(p *agentv1.Prepared) { p.InstanceId = "other-instance" }, func(p *agentv1.Prepared) { p.BuildId = "unapproved-build" }, func(p *agentv1.Prepared) { p.AcceptedProtocolVersion = 2 }, func(p *agentv1.Prepared) { p.Release.BundleHash = "foreign" }, func(p *agentv1.Prepared) { p.AppliedGeneration = 2 }} {
		bad := proto.Clone(original).(*agentv1.Prepared)
		change(bad)
		_, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, bad, v)
		expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	}
	future := proto.Clone(original).(*agentv1.Prepared)
	future.PreparedUnixMs = time.Now().Add(6 * time.Second).UnixMilli()
	future.ExpiresUnixMs = future.PreparedUnixMs + 300000
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, future, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; n != "0" {
		t.Fatal("invalid report wrote evidence", n)
	}
	// Heartbeat only extends the connection; it cannot renew expired intent.
	pgtest.Exec(t, f.dsn, `UPDATE agent_prepare_intents SET created_at=clock_timestamp()-interval '5 seconds',expires_at=clock_timestamp()-interval '1 second'`)
	if err := f.a.Heartbeat(t.Context(), f.session); err != nil {
		t.Fatal(err)
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, original, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	pending, err := f.a.PendingPrepare(t.Context(), f.session, v)
	if err != nil || pending != nil {
		t.Fatal("heartbeat resurrected expired intent", err)
	}
}

func TestPreparedQueueRollbackAndReleaseAbortFence(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	request := control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "target", Generation: 1, InstanceID: f.g.InstanceID}
	pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_prepare_head() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; CREATE TRIGGER suppress_prepare_head BEFORE INSERT ON agent_prepare_heads FOR EACH ROW EXECUTE FUNCTION suppress_prepare_head()`)
	if _, err := f.a.QueuePrepare(t.Context(), request, v); err == nil {
		t.Fatal("untracked prepare intent committed")
	}
	for _, table := range []string{"agent_prepare_intents", "agent_prepare_heads"} {
		if n := pgtest.Query(t, f.dsn, "SELECT count(*) FROM "+table)[0][0]; n != "0" {
			t.Fatal("failed queue left state", table, n)
		}
	}
	pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_prepare_head ON agent_prepare_heads`)
	intent, err := f.b.QueuePrepare(t.Context(), request, v)
	if err != nil || intent.TargetRevision != 1 {
		t.Fatal("failed queue could not retry", err)
	}
	if _, err := f.a.AbortRelease(t.Context(), control.AbortReleaseRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "abort", Generation: 1, ExpectedRevision: 1, Reason: "stopped before preparation"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	pending, err := f.a.PendingPrepare(t.Context(), f.session, v)
	if err != nil || pending != nil {
		t.Fatal("aborted command was dispatched", err)
	}
	historical, err := f.a.QueuePrepare(t.Context(), request, v)
	if err != nil || historical.CommandID != intent.CommandID {
		t.Fatal("historical intent unavailable", err)
	}
}

func TestPreparedRequiresExplicitReportPermissionAndV6Migration(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	f.a.Close()
	f.b.Close()
	pgtest.Exec(t, f.dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations,agent_preparation_decisions,agent_prepare_rejections,agent_prepared_evidence,agent_prepared_heads,agent_prepare_heads,agent_prepare_intents; UPDATE agent_schema SET version=6 WHERE id=true`)
	f.a = openSessions(t, f.dsn)
	f.b = openSessions(t, f.dsn)
	if n := pgtest.Query(t, f.dsn, `SELECT version FROM agent_schema WHERE id=true`)[0][0]; n != "11" {
		t.Fatal("migration did not advance", n)
	}
	current, err := f.b.Release(t.Context(), f.g.Scope, r.Generation)
	if err != nil || current.Definition.BundleHash != r.Definition.BundleHash || current.Generation != r.Generation {
		t.Fatal("migration lost release", err)
	}
	if err := f.b.CheckSession(t.Context(), f.session); err != nil {
		t.Fatal("migration lost session", err)
	}
	intent, err := f.a.QueuePrepare(t.Context(), control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "post-migration", Generation: 1, InstanceID: f.g.InstanceID}, v)
	if err != nil {
		t.Fatal("migration lost lease/Naming proof", err)
	}
	if _, err := f.b.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	// Strip only the report permission as an authoritative test fixture. Config,
	// registration and heartbeat rights cannot substitute for release.report.
	pgtest.Exec(t, f.dsn, `UPDATE agent_credentials SET permissions=permissions & ~$1::bigint WHERE id=$2`, int64(control.ReleaseReport), f.session.Grant.ID)
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
}
