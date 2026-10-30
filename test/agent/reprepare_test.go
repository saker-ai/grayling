//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func reprepareRequest(f *registrationFixture, op string, revision uint64) control.QueuePrepareRequest {
	return control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "release-controller", OperationID: op, Generation: 1, InstanceID: f.g.InstanceID, ExpectedTargetRevision: revision}
}

func TestPreparedEvidenceIndependentOfCommandAndLeaseDeadline(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "initial", 0), v)
	if err != nil {
		t.Fatal(err)
	}
	report := preparedReport(f, r)
	receipt, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(candidates.Members) != 1 {
		t.Fatal(err)
	}
	lease := candidates.Members[0].Instance.Lease
	if intent.ExpiresAt.After(intent.CreatedAt.Add(120*time.Second)) || !intent.ExpiresAt.After(time.UnixMilli(lease.ExpiresUnixMs)) || !receipt.ExpiresAt.After(intent.ExpiresAt) || receipt.ExpiresAt.After(receipt.ReceivedAt.Add(300*time.Second)) || receipt.ExpiresAt.After(r.ResourcesExpiresAt) {
		t.Fatal("command/evidence deadlines were conflated", intent, receipt, lease)
	}
	if _, err := f.b.RenewInstanceLease(t.Context(), f.session, "renew-same-lease", &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: lease}); err != nil {
		t.Fatal(err)
	}
	confirmPreparedNaming(t, f)
	replay, err := f.b.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	if err != nil || !bytes.Equal(preparedBytes(t, receipt), preparedBytes(t, replay)) {
		t.Fatal("lease renewal changed evidence TTL", replay, err)
	}
}

func TestReprepareAfterInitialWindowAndExpiredCommand(t *testing.T) {
	parallelAgentFixture(t)
	f, p, v, r := preparedFixture(t)
	// Shorten only the initial fixture window, then wait on real clocks. The
	// production 120s/300s constants remain unchanged and are asserted separately.
	pgtest.Exec(t, f.dsn, `UPDATE agent_releases SET prepare_deadline=clock_timestamp()+interval '3 seconds'`)
	q := reprepareRequest(f, "initial-short-window", 0)
	intent, err := f.a.QueuePrepare(t.Context(), q, v)
	if err != nil {
		t.Fatal(err)
	}
	report := preparedReport(f, r)
	evidence, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate", 1), v); err != nil {
		t.Fatal(err)
	}
	first, err := p.StartActivation(t.Context(), activationRequest(f), v)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(intent.ExpiresAt) + 10*time.Millisecond)
	replay, err := f.b.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v)
	if err != nil || !bytes.Equal(preparedBytes(t, evidence), preparedBytes(t, replay)) {
		t.Fatal("expired command discarded accepted evidence", replay, err)
	}
	aq := activationRequest(f)
	aq.OperationID = "refresh-after-initial-window"
	aq.ExpectedRevision = first.Release.Revision
	refreshed, err := p.RefreshActivation(t.Context(), aq, 1, v)
	if err != nil || !refreshed.ExpiresAt.After(first.ExpiresAt) {
		t.Fatal("expired command blocked valid positive evidence", refreshed, err)
	}
	renewal := reprepareRequest(f, "reprepare", 1)
	intents := make(chan control.PrepareIntent, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			node := f.a
			if i%2 != 0 {
				node = f.b
			}
			next, err := node.QueueReprepare(t.Context(), renewal, v)
			if err != nil {
				t.Error(err)
				return
			}
			intents <- next
		})
	}
	wg.Wait()
	close(intents)
	var next control.PrepareIntent
	for x := range intents {
		if next.TargetRevision == 0 {
			next = x
		}
		if !bytes.Equal(preparedBytes(t, next), preparedBytes(t, x)) {
			t.Fatal("reprepare retry extended command")
		}
	}
	zeroDeadline := renewal
	zeroDeadline.AuthorizationExpiresAt = time.UnixMilli(0)
	if _, err := f.a.QueueReprepare(t.Context(), zeroDeadline, v); err == nil {
		t.Fatal("Unix epoch approval aliased an absent deadline")
	}
	if next.TargetRevision != 2 || next.CommandID == intent.CommandID || !next.ExpiresAt.After(intent.ExpiresAt) || next.ExpiresAt.After(next.CreatedAt.Add(120*time.Second)) {
		t.Fatal("invalid replacement command", next)
	}
	pending, err := f.b.PendingPrepare(t.Context(), f.session, v)
	if err != nil || pending == nil || pending.Intent.CommandID != next.CommandID {
		t.Fatal("established release did not dispatch preparation", pending, err)
	}
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v); err == nil {
		t.Fatal("old command inherited new target")
	}
	accepted, err := f.b.RecordPrepared(t.Context(), f.session, next.CommandID, preparedReport(f, r), v)
	if err != nil || accepted.Revision != 2 {
		t.Fatal("new preparation evidence missing", accepted, err)
	}
	aq.OperationID = "refresh-with-new-evidence"
	aq.ExpectedRevision = refreshed.Release.Revision
	latest, err := p.RefreshActivation(t.Context(), aq, 2, v)
	if err != nil || latest.ArtifactRevision != 3 || latest.PreparedRevision != 2 {
		t.Fatal("refreshed view did not bind new evidence", latest, err)
	}
}

func TestReprepareExpiredFirstReportAndApprovalRollback(t *testing.T) {
	parallelAgentFixture(t)
	f, v, _ := appliedFixture(t)
	q := reprepareRequest(f, "short-command", 1)
	q.AuthorizationExpiresAt = time.Now().Add(300 * time.Millisecond)
	intent, err := f.a.QueueReprepare(t.Context(), q, v)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(intent.ExpiresAt) + 10*time.Millisecond)
	r, err := f.a.Release(t.Context(), f.g.Scope, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v); err == nil {
		t.Fatal("expired command accepted a first report")
	}
	if got := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; got != "1" {
		t.Fatal("expired report mutated catalog", got)
	}
	q = reprepareRequest(f, "expired-approval", 2)
	q.AuthorizationExpiresAt = time.Now().Add(30 * time.Millisecond)
	q.Authorize = func(context.Context, pgx.Tx) error { time.Sleep(50 * time.Millisecond); return nil }
	if _, err := f.b.QueueReprepare(t.Context(), q, v); err == nil {
		t.Fatal("expired approval queued new command")
	}
	if got := pgtest.Query(t, f.dsn, `SELECT revision::text FROM agent_prepare_heads`)[0][0]; got != "2" {
		t.Fatal("failed queue moved head", got)
	}
}

func TestReprepareWSSNegativeThenNewPositiveAndActivation(t *testing.T) {
	parallelAgentFixture(t)
	f, v, activation := appliedFixture(t)
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare", "release.prepare.reject", "release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	if event.GetActivate() == nil {
		t.Fatal("initial activation missing", event)
	}
	installed, err := c.ReportApplied(t.Context(), event.OperationId, appliedReport(f, activation))
	if err != nil {
		t.Fatal(err)
	}
	negative, err := f.a.QueueReprepare(t.Context(), reprepareRequest(f, "renew-negative", 1), v)
	if err != nil {
		t.Fatal(err)
	}
	prepare := clientEvent(t, c)
	if prepare.GetPrepare() == nil || prepare.OperationId != negative.CommandID {
		t.Fatal("reprepare not delivered", prepare)
	}
	if _, _, err := c.DownloadBundle(t.Context(), prepare.GetPrepare().Bundle, v, f.g.Scope, activationBundleHash(t, activation.Release), client.Watermark{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rejected, err := c.ReportPrepareRejected(t.Context(), negative.CommandID, rejectionReport(activation.Release))
	if err != nil || rejected.EvidenceRevision != 2 {
		t.Fatal("established NACK failed", rejected, err)
	}
	p, _ := publisher(t, f.a)
	aq := activationRequest(f)
	aq.OperationID = "cannot-refresh-negative"
	aq.ExpectedRevision = installed.ReleaseRevision
	if _, err := p.RefreshActivation(t.Context(), aq, 1, v); err == nil {
		t.Fatal("negative target reused old positive evidence")
	}
	positive, err := f.a.QueueReprepare(t.Context(), reprepareRequest(f, "renew-positive", 2), v)
	if err != nil {
		t.Fatal(err)
	}
	prepare = clientEvent(t, c)
	if prepare.GetPrepare() == nil || prepare.OperationId != positive.CommandID {
		t.Fatal("new target not redelivered", prepare)
	}
	accepted, err := c.ReportPrepared(t.Context(), positive.CommandID, preparedReport(f, activation.Release))
	if err != nil || accepted.EvidenceRevision != 3 {
		t.Fatal("new positive evidence failed", accepted, err)
	}
	aq.OperationID = "refresh-new-positive"
	renewed, err := p.RefreshActivation(t.Context(), aq, 1, v)
	if err != nil || renewed.PreparedRevision != 3 {
		t.Fatal("new evidence not activated", renewed, err)
	}
	event = clientEvent(t, c)
	if event.GetActivate() == nil || event.GetActivate().View.Revision != 2 || event.GetActivate().PreparedRevision != 3 {
		t.Fatal("new activation not delivered", event)
	}
}

func TestPreparedStoredTimingCannotBeLaundered(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "initial", 0), v)
	if err != nil {
		t.Fatal(err)
	}
	report := preparedReport(f, r)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v); err != nil {
		t.Fatal(err)
	}
	report.PreparedUnixMs = 0
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(append([]byte("grayling.agent.v1.prepared-report\x00"), body...))
	conn, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `UPDATE agent_prepared_evidence SET report=$1,report_hash=$2`, body, hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.EvaluatePreparation(t.Context(), gateRequest(f, "reject-invalid-stored-time", 1), v); err == nil {
		t.Fatal("stored report timing was accepted after recomputing outer hash")
	}
	current, err := f.a.Release(t.Context(), f.g.Scope, 1)
	if err != nil || current.Revision != 1 || current.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("invalid stored evidence advanced release", current, err)
	}
}
