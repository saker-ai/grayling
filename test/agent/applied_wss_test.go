//go:build integration

package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func TestAppliedWSSActivateReplicaRecoveryAndReceipt(t *testing.T) {
	f, v, p := appliedFixture(t)
	a, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	b, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(a.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: a.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first := clientEvent(t, c)
	activate := first.GetActivate()
	if activate == nil || activate.View == nil || first.OperationId != p.Source.CommandID || activate.PolicyRevision != p.Document.PolicyRevision || activate.PreparedRevision != p.Document.PreparedRevision || activate.DiscoveryRevision != p.Document.DiscoveryRevision || first.DeadlineUnixMs != p.Resource.ExpiresAt.UnixMilli() {
		t.Fatal("incomplete Activate", first)
	}
	view, _, err := c.DownloadActivation(t.Context(), activate.View, v, f.g.Scope, 1, activationBundleHash(t, p.Release), client.Watermark{}, time.Now())
	if err != nil || view == nil {
		t.Fatal("signed activation unavailable", err)
	}
	opts.URL = "wss" + strings.TrimPrefix(b.URL, "https")
	opts.HTTPClient = b.Client()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	retry := clientEvent(t, successor)
	if retry.GetActivate() == nil || retry.OperationId != first.OperationId || retry.GetActivate().View.Sha256 != activate.View.Sha256 || retry.GetActivate().ExpiresUnixMs != activate.ExpiresUnixMs {
		t.Fatal("reconnect changed activation")
	}
	report := appliedReport(f, p)
	ack, err := successor.ReportApplied(t.Context(), retry.OperationId, report)
	if err != nil || ack.ReportRevision != 1 || ack.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING {
		t.Fatal("Applied receipt missing", err)
	}
	replay, err := successor.ReportApplied(t.Context(), retry.OperationId, report)
	if err != nil || !proto.Equal(ack, replay) {
		t.Fatal("Applied retry changed", err)
	}
	if _, err := c.ReportApplied(t.Context(), first.OperationId, report); err == nil {
		t.Fatal("old connection reported Applied")
	}
	status, err := successor.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || status.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || len(status.Applied) != 1 {
		t.Fatal("network status", status, err)
	}
	successor.Close()
	opts.URL = "wss" + strings.TrimPrefix(a.URL, "https")
	opts.HTTPClient = a.Client()
	last, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer last.Close()
	replay, err = last.ReportApplied(t.Context(), first.OperationId, report)
	if err != nil || !proto.Equal(ack, replay) {
		t.Fatal("cross-replica lost response recovery", err)
	}
	select {
	case e := <-last.Events():
		t.Fatal("completed command redelivered", e)
	case <-time.After(2500 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

func TestAppliedWSSCapabilityNegativeAndDeadline(t *testing.T) {
	f, v, p := appliedFixture(t)
	srv, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activation.read"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.ReportApplied(t.Context(), p.Source.CommandID, appliedReport(f, p))
	if remote, ok := errors.AsType[*client.RemoteError](err); !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatal("unnegotiated receipt", err)
	}
	select {
	case e := <-c.Events():
		t.Fatal("unnegotiated Activate push", e)
	case <-time.After(2200 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	clientEvent(t, successor)
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
	if _, err := tx.Exec(t.Context(), `LOCK TABLE agent_activation_report_heads IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	e, err := client.Message(appliedReport(f, p))
	if err != nil {
		t.Fatal(err)
	}
	e.OperationId = p.Source.CommandID
	e.DeadlineUnixMs = time.Now().Add(300 * time.Millisecond).UnixMilli()
	started := time.Now()
	_, err = successor.Call(t.Context(), e)
	if remote, ok := errors.AsType[*client.RemoteError](err); !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED || time.Since(started) > time.Second {
		t.Fatal("Applied ignored command deadline", err)
	}
	tx.Rollback(t.Context())
	wrongRevision := activationRejection(f, p)
	wrongRevision.ActivationArtifactRevision++
	if _, err := successor.ReportActivationRejected(t.Context(), p.Source.CommandID, wrongRevision); err == nil {
		t.Fatal("WSS accepted a rejection for another activation artifact")
	}
	ack, err := successor.ReportActivationRejected(t.Context(), p.Source.CommandID, activationRejection(f, p))
	if err != nil || !ack.Rejected || ack.ReportRevision != 1 {
		t.Fatal("negative report after timed-out positive", err)
	}
	status, err := successor.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || len(status.Rejected) != 1 || len(status.Applied) != 0 || status.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING {
		t.Fatal("negative outcome lost", status, err)
	}
}

func TestAppliedStatusPaginationPinsCatalogAndPartialSources(t *testing.T) {
	f, publisher, v, r := activationFixture(t)
	other := secondGateInstance(t, f, "az-a")
	target := queueGateTarget(t, other, v, "other", 0)
	if _, err := f.b.RecordPrepared(t.Context(), other.session, target.CommandID, preparedReport(other, r), v); err != nil {
		t.Fatal(err)
	}
	q := activationRequest(f)
	q.RequiredSourceIDs = append(q.RequiredSourceIDs, other.g.InstanceID)
	if _, err := publisher.StartActivation(t.Context(), q, v); err != nil {
		t.Fatal(err)
	}
	one, err := f.a.PendingActivation(t.Context(), f.session, v)
	if err != nil || one == nil {
		t.Fatal(err)
	}
	two, err := f.b.PendingActivation(t.Context(), other.session, v)
	if err != nil || two == nil {
		t.Fatal(err)
	}
	first, err := f.a.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: r.ReleaseID, PageSize: 1})
	if err != nil || !first.More || first.RequiredSources != 2 || len(first.PendingSourceIds) != 1 || first.ReportRevision != 0 {
		t.Fatal("initial page", first, err)
	}
	next := &agentv1.ReleaseStatusQuery{ReleaseId: r.ReleaseID, PageSize: 1, AfterInstanceId: first.NextInstanceId, ExpectedReportRevision: new(first.ReportRevision), ExpectedReleaseRevision: new(first.ReleaseRevision), ExpectedActivationArtifactRevision: new(first.ActivationArtifactRevision)}
	if _, err := f.b.RecordApplied(t.Context(), other.session, two.Source.CommandID, appliedReport(other, two), v); err != nil {
		t.Fatal(err)
	}
	_, err = f.a.ActivationStatus(t.Context(), f.session, next)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESYNC_REQUIRED)
	first, err = f.a.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: r.ReleaseID, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	next.AfterInstanceId = first.NextInstanceId
	next.ExpectedReportRevision = new(first.ReportRevision)
	next.ExpectedReleaseRevision = new(first.ReleaseRevision)
	next.ExpectedActivationArtifactRevision = new(first.ActivationArtifactRevision)
	second, err := f.a.ActivationStatus(t.Context(), f.session, next)
	if err != nil || second.More || len(second.Applied) != 1 || len(first.PendingSourceIds) != 1 {
		t.Fatal("consistent pages", first, second, err)
	}
	if _, err := f.a.RecordActivationRejected(t.Context(), f.session, one.Source.CommandID, activationRejection(f, one), v); err != nil {
		t.Fatal(err)
	}
	full, err := f.b.ActivationStatus(t.Context(), other.session, &agentv1.ReleaseStatusQuery{ReleaseId: r.ReleaseID})
	if err != nil || full.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || full.ReleaseRevision != 5 || full.ReportRevision != 2 || len(full.Applied) != 1 || len(full.Rejected) != 1 || len(full.PendingSourceIds) != 0 {
		t.Fatal("partial success became ACTIVE", full, err)
	}
}
