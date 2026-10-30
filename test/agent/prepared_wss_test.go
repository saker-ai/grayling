//go:build integration

package agent_test

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func TestPreparedWSSReplicaRedeliveryDownloadAndReceipt(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent, err := f.a.QueuePrepare(t.Context(), control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "wire-target", Generation: 1, InstanceID: f.g.InstanceID}, v)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	b, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(a.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: a.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first := clientEvent(t, c)
	if first.GetPrepare() == nil || first.OperationId != intent.CommandID || first.DeadlineUnixMs != intent.ExpiresAt.UnixMilli() || first.GetPrepare().Release.Generation != 1 {
		t.Fatal("directed command missing", first)
	}
	if !slices.Contains(c.Welcome().GetWelcome().Capabilities, "release.prepare") {
		t.Fatal("capability missing")
	}
	var hash [32]byte
	raw, _ := hex.DecodeString(r.Definition.BundleHash)
	copy(hash[:], raw)
	bundle, _, err := c.DownloadBundle(t.Context(), first.GetPrepare().Bundle, v, f.g.Scope, hash, client.Watermark{}, time.Now())
	if err != nil || bundle == nil {
		t.Fatal("exact Prepare download failed", err)
	}
	// No receipt yet: a different replica recovers the same durable command.
	opts.URL = "wss" + strings.TrimPrefix(b.URL, "https")
	opts.HTTPClient = b.Client()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	retry := clientEvent(t, successor)
	if retry.OperationId != first.OperationId || !proto.Equal(retry.GetPrepare().Release, first.GetPrepare().Release) || retry.GetPrepare().ExpiresUnixMs != first.GetPrepare().ExpiresUnixMs {
		t.Fatal("reconnect replaced immutable intent")
	}
	report := preparedReport(f, r)
	ack, err := successor.ReportPrepared(t.Context(), intent.CommandID, report)
	if err != nil || ack.EvidenceRevision != 1 || ack.ExpiresUnixMs <= intent.ExpiresAt.UnixMilli() || ack.ExpiresUnixMs > report.ExpiresUnixMs || ack.ExpiresUnixMs > r.ResourcesExpiresAt.UnixMilli() {
		t.Fatal("receipt unavailable", err)
	}
	same, err := successor.ReportPrepared(t.Context(), intent.CommandID, report)
	if err != nil || !proto.Equal(ack, same) {
		t.Fatal("wire replay refreshed receipt", err)
	}
	changed := proto.Clone(report).(*agentv1.Prepared)
	changed.ExpiresUnixMs--
	_, err = successor.ReportPrepared(t.Context(), intent.CommandID, changed)
	if remote, ok := errors.AsType[*client.RemoteError](err); !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Fatal("changed report accepted", err)
	}
	if _, err := c.ReportPrepared(t.Context(), intent.CommandID, report); err == nil {
		t.Fatal("fenced connection accepted")
	}
	current, err := f.a.Release(t.Context(), f.g.Scope, 1)
	if err != nil || current.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("receipt inferred release readiness", err)
	}
	// Accepted command must not be re-emitted after yet another reconnect.
	successor.Close()
	last, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer last.Close()
	select {
	case e := <-last.Events():
		t.Fatal("accepted command redelivered", e)
	case <-time.After(2500 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

func TestPreparedWSSCapabilityRequiresTrustAndExplicitOptIn(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	noTrust, _ := channelServer(t, f.a)
	opts := client.Options{URL: "wss" + strings.TrimPrefix(noTrust.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: noTrust.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare"}
	if c, err := client.Dial(t.Context(), opts); err == nil {
		c.Close()
		t.Fatal("unconfigured trust advertised capability")
	}
	srv, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts.URL = "wss" + strings.TrimPrefix(srv.URL, "https")
	opts.HTTPClient = srv.Client()
	opts.Hello.RequiredCapabilities = nil
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = f.a.QueuePrepare(t.Context(), control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "legacy-target", Generation: 1, InstanceID: f.g.InstanceID}, v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ReportPrepared(t.Context(), "legacy-target", preparedReport(f, r))
	if remote, ok := errors.AsType[*client.RemoteError](err); !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatal("unnegotiated report accepted", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()
	select {
	case e := <-c.Events():
		t.Fatal("unnegotiated push", e)
	case <-ctx.Done():
	}
}

func TestPreparedWSSDeadlineRollsBackEvidence(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, r := preparedFixture(t)
	intent, err := f.a.QueuePrepare(t.Context(), control.QueuePrepareRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "deadline-target", Generation: 1, InstanceID: f.g.InstanceID}, v)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	clientEvent(t, c)
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
	if _, err := tx.Exec(t.Context(), "LOCK TABLE agent_prepared_heads IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	report := preparedReport(f, r)
	// An explicit envelope deadline leaves client transport time to receive NACK.
	e, err := client.Message(report)
	if err != nil {
		t.Fatal(err)
	}
	e.OperationId = intent.CommandID
	e.DeadlineUnixMs = time.Now().Add(300 * time.Millisecond).UnixMilli()
	start := time.Now()
	_, err = c.Call(t.Context(), e)
	remote, ok := errors.AsType[*client.RemoteError](err)
	if !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED || time.Since(start) > time.Second {
		t.Fatal("report command ignored deadline", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	ack, err := c.ReportPrepared(t.Context(), intent.CommandID, report)
	if err != nil || ack.EvidenceRevision != 1 {
		t.Fatal("timeout committed evidence or lost session", err)
	}
}
