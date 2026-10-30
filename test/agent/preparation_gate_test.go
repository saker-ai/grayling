//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func queueGateTarget(t *testing.T, f *registrationFixture, v *client.Verifier, op string, expected uint64) control.PrepareIntent {
	t.Helper()
	intent, err := f.a.QueuePrepare(t.Context(), control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: op, Generation: 1, InstanceID: f.g.InstanceID, ExpectedTargetRevision: expected}, v)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
func gateRequest(f *registrationFixture, op string, revision uint64) control.EvaluatePreparationRequest {
	return control.EvaluatePreparationRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: op, Generation: 1, ExpectedRevision: revision}
}
func rejectionReport(r control.ReleaseRecord) *agentv1.Rejected {
	return &agentv1.Rejected{Release: &agentv1.ReleaseIdentity{ReleaseId: r.ReleaseID, Generation: r.Generation, BundleHash: r.Definition.BundleHash}, Error: &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_NOT_READY, Reason: "local health check failed"}}
}
func secondGateInstance(t *testing.T, f *registrationFixture, zone string, builds ...string) *registrationFixture {
	t.Helper()
	g := f.g
	g.InstanceID += "-second"
	g.BootID += "-second"
	g.AgentID += "-second"
	_, secret := issue(t, f.a, g)
	se, err := f.b.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	instance := registrationDescriptor(g)
	instance.Address = "10.20.0.11"
	instance.Zone = zone
	if len(builds) > 0 {
		instance.BuildId = builds[0]
		instance.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	}
	authorization, err := f.a.AuthorizeDeployment(t.Context(), adminMutation("deployment-second"), g.Scope, g.PrincipalID, g.AgentID, f.policy.Revision, instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	instance.Ready = true
	other := &registrationFixture{a: f.a, b: f.b, dsn: f.dsn, g: g, session: se, secret: secret, policy: f.policy, request: &agentv1.Register{Instance: instance, DeploymentAuthorization: []byte(authorization.ID)}}
	registerInstance(t, other, "register-second")
	confirmPreparedNaming(t, f)
	return other
}

func TestPreparationRejectedConcurrentMutualExclusionAndReadiness(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent := queueGateTarget(t, f, v, "nack", 0)
	changed := proto.Clone(f.request.Instance).(*agentv1.Instance)
	current, err := f.a.RegisterInstance(t.Context(), f.session, "register", f.request)
	if err != nil {
		t.Fatal(err)
	}
	changed.Lease = current.Instance.Lease
	changed.Ready = false
	if _, err := f.a.UpdateRegisteredInstance(t.Context(), f.session, "unready", &agentv1.UpdateInstance{Instance: changed}); err != nil {
		t.Fatal(err)
	}
	report := rejectionReport(r)
	out := make(chan control.PrepareRejectedReceipt, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			node := f.a
			if i%2 != 0 {
				node = f.b
			}
			receipt, err := node.RecordPrepareRejected(t.Context(), f.session, intent.CommandID, report, v)
			if err != nil {
				t.Error(err)
				return
			}
			out <- receipt
		})
	}
	wg.Wait()
	close(out)
	var first control.PrepareRejectedReceipt
	for receipt := range out {
		if first.Revision == 0 {
			first = receipt
		}
		if !bytes.Equal(preparedBytes(t, receipt), preparedBytes(t, first)) {
			t.Fatal("negative replay changed")
		}
	}
	if first.Revision != 1 {
		t.Fatal("negative revision", first.Revision)
	}
	_, err = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v)
	// Readiness may fence first, but can never replace the negative outcome.
	if err == nil {
		t.Fatal("positive overwrote NACK")
	}
	bad := proto.Clone(report).(*agentv1.Rejected)
	bad.Error.Reason = "changed"
	_, err = f.a.RecordPrepareRejected(t.Context(), f.session, intent.CommandID, bad, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	pending, err := f.a.PendingPrepare(t.Context(), f.session, v)
	if err != nil || pending != nil {
		t.Fatal("NACK redelivered", err)
	}
	eval, err := f.b.EvaluatePreparation(t.Context(), gateRequest(f, "nack-gate", 1), v)
	if err != nil || eval.Eligible || eval.Failed != 1 || eval.Targets != 1 || eval.Release.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("NACK disappeared when unready", eval, err)
	}
}

func TestPreparationGateAtomicReplicaReplayExpiryAndAbort(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	q := gateRequest(f, "gate", 1)
	empty, err := f.a.EvaluatePreparation(t.Context(), q, v)
	if err != nil || empty.Eligible || empty.Targets != 0 {
		t.Fatal("empty capacity", empty, err)
	}
	intent := queueGateTarget(t, f, v, "positive", 0)
	pending, err := f.b.EvaluatePreparation(t.Context(), q, v)
	if err != nil || pending.Eligible || pending.Pending != 1 {
		t.Fatal("pending granted readiness", pending, err)
	}
	report := preparedReport(f, r)
	report.ExpiresUnixMs = time.Now().Add(3 * time.Second).UnixMilli()
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v); err != nil {
		t.Fatal(err)
	}
	out := make(chan control.PreparationEvaluation, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			node := f.a
			if i%2 != 0 {
				node = f.b
			}
			e, err := node.EvaluatePreparation(t.Context(), q, v)
			if err != nil {
				t.Error(err)
				return
			}
			out <- e
		})
	}
	wg.Wait()
	close(out)
	var first control.PreparationEvaluation
	for e := range out {
		if first.Release.Revision == 0 {
			first = e
		}
		if !bytes.Equal(preparedBytes(t, e), preparedBytes(t, first)) {
			t.Fatal("gate replay changed")
		}
	}
	if !first.Eligible || first.Prepared != 1 || first.EvidenceRevision != 1 || first.Release.Revision != 2 || first.Release.State != agentv1.ReleaseState_RELEASE_STATE_PREPARED || len(first.Cells) != 1 || first.Cells[0].Prepared != 1 {
		t.Fatal("capacity not committed", first)
	}
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_preparation_decisions`)[0][0]; n != "1" {
		t.Fatal("duplicate decision", n)
	}
	time.Sleep(time.Until(time.UnixMilli(report.ExpiresUnixMs)) + 10*time.Millisecond)
	expired, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "expired", 2), v)
	if err != nil || expired.Eligible || expired.Failed != 1 || expired.Prepared != 0 {
		t.Fatal("expired Prepared counted", expired, err)
	}
	historical, err := f.b.EvaluatePreparation(t.Context(), q, v)
	if err != nil || !bytes.Equal(preparedBytes(t, historical), preparedBytes(t, first)) {
		t.Fatal("history renewed or changed", err)
	}
	_, err = f.a.AbortRelease(t.Context(), control.AbortReleaseRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "abort-prepared", Generation: 1, ExpectedRevision: 2, Reason: "capacity expired"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.a.EvaluatePreparation(t.Context(), gateRequest(f, "after-abort", 3), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
}

func TestPreparationGateFailureUpperBoundAndEveryZone(t *testing.T) {
	parallelAgentFixture(t)
	for _, threshold := range []uint32{0, 4999, 5000} {
		t.Run(strconv.FormatUint(uint64(threshold), 10), func(t *testing.T) {
			capacity := control.ReleaseCapacity{Profile: "demo-v1", RequiredZones: []string{"az-a"}, MinimumPerBuildZone: 1, MaximumFailureBasisPoints: threshold}
			f, _, v, r := preparedFixtureWithCapacity(t, capacity)
			other := secondGateInstance(t, f, "az-a")
			one := queueGateTarget(t, f, v, "one", 0)
			two := queueGateTarget(t, other, v, "two", 0)
			if _, err := f.a.RecordPrepared(t.Context(), f.session, one.CommandID, preparedReport(f, r), v); err != nil {
				t.Fatal(err)
			}
			// Pending cannot dilute a failure: it participates in the worst-case numerator.
			pending, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "pending", 1), v)
			if err != nil || pending.Pending != 1 || pending.Eligible != (threshold >= 5000) {
				t.Fatal("pending upper bound", pending, err)
			}
			if _, err := f.b.RecordPrepareRejected(t.Context(), other.session, two.CommandID, rejectionReport(r), v); err != nil {
				t.Fatal(err)
			}
			revision := uint64(1)
			if pending.Eligible {
				revision = 2
			}
			result, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "rejected", revision), v)
			if err != nil || result.Failed != 1 || result.Prepared != 1 || result.Eligible != (threshold >= 5000) {
				t.Fatal("failure threshold boundary", result, err)
			}
		})
	}
	capacity := control.ReleaseCapacity{Profile: "demo-v1", RequiredZones: []string{"az-a", "az-b"}, MinimumPerBuildZone: 1, MaximumFailureBasisPoints: 10000}
	f, _, v, r := preparedFixtureWithCapacity(t, capacity)
	one := queueGateTarget(t, f, v, "only-a", 0)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, one.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	result, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "missing-zone", 1), v)
	if err != nil || result.Eligible || len(result.Cells) != 2 || result.Cells[1].Prepared != 0 {
		t.Fatal("global count filled absent AZ", result, err)
	}
}

func TestPreparationGateRollbackAndAuthorizationExpiry(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent := queueGateTarget(t, f, v, "atomic", 0)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	assertClean := func() {
		t.Helper()
		current, err := f.b.Release(t.Context(), f.g.Scope, 1)
		if err != nil || current.Revision != 1 || current.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
			t.Fatal("partial gate state", current, err)
		}
		if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_preparation_decisions`)[0][0]; n != "0" {
			t.Fatal("partial decision")
		}
	}
	denied := gateRequest(f, "guard", 1)
	denied.Authorize = func(context.Context, pgx.Tx) error { return errors.New("authorization rejected") }
	if _, err := f.a.EvaluatePreparation(t.Context(), denied, v); err == nil {
		t.Fatal("guard ignored")
	}
	assertClean()
	for _, table := range []string{"agent_release_events", "agent_preparation_decisions"} {
		pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_gate() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; CREATE TRIGGER suppress_gate BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION suppress_gate()`)
		if _, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "suppressed-"+table, 1), v); err == nil {
			t.Fatal("suppressed write committed")
		}
		assertClean()
		pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_gate ON `+table+`; DROP FUNCTION suppress_gate()`)
	}
	deadline := gateRequest(f, "expired-authorization", 1)
	deadline.AuthorizationExpiresAt = time.Now().Add(150 * time.Millisecond)
	deadline.Authorize = func(context.Context, pgx.Tx) error { time.Sleep(180 * time.Millisecond); return nil }
	_, err := f.a.EvaluatePreparation(t.Context(), deadline, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	assertClean()
	successful, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "valid", 1), v)
	if err != nil || !successful.Eligible {
		t.Fatal("rollback prevented recovery", err)
	}
}

func TestPreparationPositiveNegativeRaceAndBootTakeover(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent := queueGateTarget(t, f, v, "race", 0)
	var positive, negative error
	var wg sync.WaitGroup
	wg.Go(func() {
		_, positive = f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v)
	})
	wg.Go(func() {
		_, negative = f.b.RecordPrepareRejected(t.Context(), f.session, intent.CommandID, rejectionReport(r), v)
	})
	wg.Wait()
	if (positive == nil) == (negative == nil) {
		t.Fatal("outcomes not exclusive", positive, negative)
	}
	failed := positive
	if positive == nil {
		failed = negative
	}
	expectCode(t, failed, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	if n := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_prepared_evidence)+(SELECT count(*) FROM agent_prepare_rejections)`)[0][0]; n != "1" {
		t.Fatal("multiple outcome rows", n)
	}
	// A new explicit target recovers from either terminal outcome.
	replacement := queueGateTarget(t, f, v, "retry", 1)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, replacement.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	g := f.g
	g.BootID = "takeover-boot"
	g.TakeoverFromBootID = f.g.BootID
	_, secret := issue(t, f.a, g)
	if _, err := f.b.Connect(t.Context(), secret, helloFor(g)); err != nil {
		t.Fatal(err)
	}
	result, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "after-takeover", 1), v)
	if err != nil || result.Eligible || result.Failed != 1 || result.Prepared != 0 {
		t.Fatal("old boot inherited Prepared", result, err)
	}
}

func TestPreparationV7MigrationPreservesPositiveEvidence(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent := queueGateTarget(t, f, v, "migration", 0)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	f.a.Close()
	f.b.Close()
	pgtest.Exec(t, f.dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations,agent_preparation_decisions,agent_prepare_rejections; UPDATE agent_schema SET version=7 WHERE id=true`)
	f.a = openSessions(t, f.dsn)
	f.b = openSessions(t, f.dsn)
	result, err := f.b.EvaluatePreparation(t.Context(), gateRequest(f, "after-migration", 1), v)
	if err != nil || !result.Eligible || result.EvidenceRevision != 1 {
		t.Fatal("v7 migration lost evidence", result, err)
	}
	if n := pgtest.Query(t, f.dsn, `SELECT version FROM agent_schema WHERE id=true`)[0][0]; n != "11" {
		t.Fatal("migration version", n)
	}
}

func TestPreparationGateEveryCarriedBuild(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixtureWithPermissions(t, 2, control.ReleaseReport)
	registerInstance(t, f, "register")
	confirmPreparedNaming(t, f)
	p, v := publisher(t, f.a)
	request := bundlePairRequest(t)
	request.Document.Builds[0].ID = "build-a"
	request.Document.ValueBuilds["backend"] = "build-a"
	request.Document.Builds = append(request.Document.Builds, client.BundleBuild{ID: "build-b", ImageDigest: "sha256:" + strings.Repeat("b", 64), ProtocolVersion: 1, Resources: []client.BundleResource{}})
	pair, err := p.PublishBundle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	release := releaseRequest(pair)
	release.Capacity = control.ReleaseCapacity{Profile: "demo-v1", RequiredZones: []string{"az-a"}, MinimumPerBuildZone: 1}
	r, err := p.StartRelease(t.Context(), release, v)
	if err != nil {
		t.Fatal(err)
	}
	first := queueGateTarget(t, f, v, "build-a", 0)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, first.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	missing, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "missing-build", 1), v)
	if err != nil || missing.Eligible || len(missing.Cells) != 2 || missing.Cells[1].BuildID != "build-b" || missing.Cells[1].Prepared != 0 {
		t.Fatal("other build capacity substituted", missing, err)
	}
	other := secondGateInstance(t, f, "az-a", "build-b")
	second := queueGateTarget(t, other, v, "build-b", 0)
	report := preparedReport(other, r)
	report.BuildId = "build-b"
	if _, err := f.b.RecordPrepared(t.Context(), other.session, second.CommandID, report, v); err != nil {
		t.Fatal(err)
	}
	ready, err := f.b.EvaluatePreparation(t.Context(), gateRequest(f, "all-builds", 1), v)
	if err != nil || !ready.Eligible || ready.Prepared != 2 || ready.Cells[0].Prepared != 1 || ready.Cells[1].Prepared != 1 {
		t.Fatal("complete build matrix rejected", ready, err)
	}
}
