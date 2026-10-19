//go:build integration

package agent_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestWSSCommandDeadlineCancelsStorageAndPreservesSession(t *testing.T) {
	dsn := pgtest.DSN(t)
	sessions := openSessions(t, dsn)
	p, _ := publisher(t, sessions)
	g := grant()
	if err := sessions.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, sessions, g)
	for _, key := range []string{"routing", "alternate"} {
		if _, err := p.PublishPolicy(t.Context(), control.PublishPolicyRequest{Scope: g.Scope, PrincipalID: "approved", OperationID: key, Key: key, BundleHash: sha256.Sum256([]byte(key)), Payload: policyPayload(t, true)}); err != nil {
			t.Fatal(err)
		}
	}
	srv, _ := channelServer(t, sessions)
	c := dialAgent(t, srv, secret)
	welcome := handshake(t, c, g)
	db, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	lock := func(table string) pgx.Tx {
		t.Helper()
		tx, err := db.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Identifiers are constants in this test, never user input.
		if _, err := tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{table}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	request := func(id string) *agentv1.Envelope {
		return &agentv1.Envelope{ProtocolMajor: 1, MessageId: id, SessionId: welcome.SessionId, ConnectionEpoch: welcome.ConnectionEpoch, DeadlineUnixMs: time.Now().Add(300 * time.Millisecond).UnixMilli()}
	}
	assertDeadline := func(e *agentv1.Envelope) {
		t.Helper()
		start := time.Now()
		writeEnvelope(t, c, e)
		got := readEnvelope(t, c)
		if got.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED {
			t.Fatalf("deadline reported as %v", got)
		}
		if time.Since(start) > time.Second {
			t.Fatal("command ignored its short deadline")
		}
		if e.GetSubscribe() != nil {
			if got.SubscriptionId != e.SubscriptionId {
				t.Fatalf("subscription deadline lost correlation: %v", got)
			}
		} else if got.ReplyTo != e.MessageId {
			t.Fatalf("deadline lost request correlation: %v", got)
		}
	}
	tx := lock("agent_resources")
	fetch := request("fetch-deadline")
	fetch.Payload = &agentv1.Envelope_FetchResource{FetchResource: &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing"}}
	assertDeadline(fetch)
	sub := request("subscribe-deadline")
	sub.SubscriptionId = "deadline-sub"
	sub.Payload = &agentv1.Envelope_Subscribe{Subscribe: &agentv1.Subscribe{Cursor: &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing"}}}
	assertDeadline(sub)
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A failed first subscribe must not reserve its ID or consume stream sequence.
	retry := request("subscribe-retry")
	retry.SubscriptionId = sub.SubscriptionId
	retry.Payload = &agentv1.Envelope_Subscribe{Subscribe: &agentv1.Subscribe{Cursor: &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "alternate"}}}
	writeEnvelope(t, c, retry)
	snapshot := readEnvelope(t, c)
	if snapshot.GetSnapshot() == nil || snapshot.StreamSeq != 1 || snapshot.ResourceKey != "alternate" {
		t.Fatalf("failed subscription became active: %v", snapshot)
	}
	unsub := request("unsubscribe")
	unsub.SubscriptionId = sub.SubscriptionId
	unsub.Payload = &agentv1.Envelope_Unsubscribe{Unsubscribe: &agentv1.Unsubscribe{}}
	writeEnvelope(t, c, unsub)
	// Heartbeat is ordered behind Unsubscribe on this connection; wait for it
	// before blocking credentials so the next assertion cannot see its error.
	writeEnvelope(t, c, heartbeat(welcome))
	if got := readEnvelope(t, c); got.GetHeartbeatAck() == nil {
		t.Fatalf("unsubscribe barrier failed: %v", got)
	}
	// Expiry while authorizing a command must cancel storage and keep the session.
	tx = lock("agent_credentials")
	h := heartbeat(welcome)
	h.DeadlineUnixMs = time.Now().Add(300 * time.Millisecond).UnixMilli()
	assertDeadline(h)
	authSub := request("subscribe-auth-deadline")
	authSub.SubscriptionId = "auth-deadline-sub"
	authSub.Payload = &agentv1.Envelope_Subscribe{Subscribe: &agentv1.Subscribe{Cursor: &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing"}}}
	assertDeadline(authSub)
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, c, heartbeat(welcome))
	if got := readEnvelope(t, c); got.GetHeartbeatAck() == nil {
		t.Fatalf("deadline closed or poisoned session: %v", got)
	}
}
