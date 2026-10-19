//go:build integration

package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"math"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func verifyStoredDiscovery(t *testing.T, v *client.Verifier, r control.StoredResource, scope client.Scope, bundle [32]byte, mark client.Watermark) (*client.Discovery, client.Claims) {
	t.Helper()
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(r.Body, signed); err != nil {
		t.Fatal(err)
	}
	discovery, claims, err := client.VerifyDiscovery(v, signed, scope, bundle, mark, time.Now())
	if err != nil || claims.Revision != r.Revision {
		t.Fatal("persisted discovery failed verification", err)
	}
	return discovery, claims
}
func TestInstanceDiscoveryDurableConcurrentCaptureAndMembership(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	a, v := publisher(t, f.a)
	b, _ := publisher(t, f.b)
	bundle := sha256.Sum256([]byte("bundle-a"))
	var wg sync.WaitGroup
	results := make(chan control.StoredResource, 12)
	failures := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			p := a
			if i%2 != 0 {
				p = b
			}
			r, err := p.Discovery(t.Context(), f.session, bundle)
			if err != nil {
				failures <- err
			} else {
				results <- r
			}
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first control.StoredResource
	for r := range results {
		if first.Body == nil {
			first = r
		}
		if r.Revision != 1 || !bytes.Equal(r.Body, first.Body) {
			t.Fatal("concurrent captures changed immutable identity")
		}
	}
	empty, claims := verifyStoredDiscovery(t, v, first, f.g.Scope, bundle, client.Watermark{})
	if len(empty.Candidates()) != 0 {
		t.Fatal("empty capture not empty")
	}
	registered := registerInstance(t, f, "discovery-register")
	pending, err := a.Discovery(t.Context(), f.session, bundle)
	if err != nil {
		t.Fatal(err)
	}
	unconfirmed, pendingClaims := verifyStoredDiscovery(t, v, pending, f.g.Scope, bundle, claims.Watermark())
	if len(unconfirmed.Candidates()) != 0 || pending.Revision <= first.Revision {
		t.Fatal("unconfirmed registration became a published member")
	}
	intents, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(intents) != 1 {
		t.Fatal(err)
	}
	if err := f.b.ConfirmNamingIntent(t.Context(), intents[0]); err != nil {
		t.Fatal(err)
	}
	confirmed, err := b.Discovery(t.Context(), f.session, bundle)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := verifyStoredDiscovery(t, v, confirmed, f.g.Scope, bundle, pendingClaims.Watermark())
	if len(full.Candidates()) != 1 || full.Candidates()[0].Lease.Id != registered.Instance.Lease.Id {
		t.Fatal("confirmed member not captured")
	}
	replay, err := a.Discovery(t.Context(), f.session, bundle)
	if err != nil || !bytes.Equal(replay.Body, confirmed.Body) {
		t.Fatal("cross replica retry re-signed artifact", err)
	}
	other := sha256.Sum256([]byte("bundle-b"))
	independent, err := b.Discovery(t.Context(), f.session, other)
	if err != nil || independent.Revision != 1 {
		t.Fatal("bundle resources shared revision counter", err)
	}
	verifyStoredDiscovery(t, v, independent, f.g.Scope, other, client.Watermark{})
	rows := pgtest.Query(t, f.dsn, `SELECT encode(body,'hex') FROM agent_discovery_resources WHERE revision=1 AND bundle_hash=$1`, bundle[:])
	if len(rows) != 1 || rows[0][0] != hex.EncodeToString(first.Body) {
		t.Fatal("later captures rewrote immutable history")
	}
}
func TestInstanceDiscoveryRefreshAndFailureAtomicity(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	p, v := publisher(t, f.a)
	bundle := sha256.Sum256([]byte("refresh"))
	pgtest.Exec(t, f.dsn, `CREATE FUNCTION reject_discovery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER reject_discovery BEFORE INSERT ON agent_discovery_resources FOR EACH ROW EXECUTE FUNCTION reject_discovery()`)
	if _, err := p.Discovery(t.Context(), f.session, bundle); err == nil {
		t.Fatal("artifact failure committed")
	}
	rows := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_discovery_heads),(SELECT count(*) FROM agent_discovery_resources)`)
	if rows[0][0] != "0" || rows[0][1] != "0" {
		t.Fatal("failed signing transaction left a head or artifact")
	}
	pgtest.Exec(t, f.dsn, `DROP TRIGGER reject_discovery ON agent_discovery_resources; DROP FUNCTION reject_discovery()`)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32))
	short, err := control.NewPublisher(f.a, "policy-test-key", key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, err := short.Discovery(t.Context(), f.session, bundle)
	if err != nil || first.Revision != 1 {
		t.Fatal(err)
	}
	initial, claims := verifyStoredDiscovery(t, v, first, f.g.Scope, bundle, client.Watermark{})
	timer := time.NewTimer(time.Until(first.ExpiresAt.Add(-250 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("test cancelled")
	}
	second, err := short.Discovery(t.Context(), f.session, bundle)
	if err != nil || second.Revision != 2 || !second.ExpiresAt.After(first.ExpiresAt) {
		t.Fatal("refresh reused old immutable identity", err)
	}
	refreshed, _ := verifyStoredDiscovery(t, v, second, f.g.Scope, bundle, claims.Watermark())
	if refreshed.MembershipRevision() != initial.MembershipRevision() {
		t.Fatal("empty set freshness changed membership revision")
	}
}

func TestInstanceDiscoveryV4MigrationPreservesGovernance(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	registered := registerInstance(t, f, "before-v5")
	pgtest.Exec(t, f.dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations,agent_preparation_decisions,agent_prepare_rejections,agent_prepared_evidence,agent_prepared_heads,agent_prepare_heads,agent_prepare_intents,agent_release_resource_refs,agent_release_events,agent_release_operations,agent_releases,agent_release_heads,agent_discovery_resources,agent_discovery_heads; UPDATE agent_schema SET version=4 WHERE id=true`)
	upgraded := openSessions(t, f.dsn)
	if err := upgraded.CheckSession(t.Context(), f.session); err != nil {
		t.Fatal("v5 reset session", err)
	}
	snapshot, err := upgraded.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(snapshot.Members) != 1 || snapshot.Members[0].Instance.Lease.Id != registered.Instance.Lease.Id {
		t.Fatal("v5 lost lease", err)
	}
	p, v := publisher(t, upgraded)
	bundle := sha256.Sum256([]byte("migrated"))
	artifact, err := p.Discovery(t.Context(), f.session, bundle)
	if err != nil || artifact.Revision != 1 {
		t.Fatal("v5 failed to publish first resource", err)
	}
	verifyStoredDiscovery(t, v, artifact, f.g.Scope, bundle, client.Watermark{})
}

func TestInstanceDiscoveryUint64ExhaustionAndFutureArtifact(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	p, v := publisher(t, f.a)
	bundle := sha256.Sum256([]byte("uint64"))
	original, err := p.Discovery(t.Context(), f.session, bundle)
	if err != nil {
		t.Fatal(err)
	}
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(original.Body, signed); err != nil {
		t.Fatal(err)
	}
	claims, err := client.ParseClaims(signed.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.Revision = math.MaxUint64
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32))
	high, err := control.SignResource(claims, signed.Payload, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(high)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	pgtest.Exec(t, f.dsn, `INSERT INTO agent_discovery_resources SELECT workspace_id,environment_id,service_key,bundle_hash,18446744073709551615,membership_revision,control_plane_epoch,$1,$2,expires_at FROM agent_discovery_resources WHERE revision=1`, body, digest[:])
	pgtest.Exec(t, f.dsn, `UPDATE agent_discovery_heads SET revision=18446744073709551615`)
	current, err := p.Discovery(t.Context(), f.session, bundle)
	if err != nil || current.Revision != math.MaxUint64 {
		t.Fatal("full uint64 artifact not reusable", err)
	}
	verifyStoredDiscovery(t, v, current, f.g.Scope, bundle, client.Watermark{})
	short, err := control.NewPublisher(f.a, "policy-test-key", key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = short.Discovery(t.Context(), f.session, bundle)
	detail, ok := errors.AsType[*agentv1.ProtocolError](err)
	if !ok || detail.Code != agentv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Fatal("exhausted counter wrapped", err)
	}
	claims.IssuedUnixMs = time.Now().Add(3 * time.Second).UnixMilli()
	future, err := control.SignResource(claims, signed.Payload, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err = proto.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(body)
	pgtest.Exec(t, f.dsn, `UPDATE agent_discovery_resources SET body=$1,sha256=$2 WHERE revision=18446744073709551615`, body, digest[:])
	_, err = p.Discovery(t.Context(), f.session, bundle)
	detail, ok = errors.AsType[*agentv1.ProtocolError](err)
	if !ok || detail.Code != agentv1.ErrorCode_ERROR_CODE_NOT_READY {
		t.Fatal("future-issued artifact returned after clock rollback", err)
	}
}

func TestInstanceDiscoveryRejectsSessionExpiringDuringHeadLock(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	p, _ := publisher(t, f.a)
	bundle := sha256.Sum256([]byte("head-lock"))
	if _, err := p.Discovery(t.Context(), f.session, bundle); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT revision FROM agent_discovery_heads FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_sessions SET expires_at=clock_timestamp()+interval '3 seconds'`)
	finished := make(chan error, 1)
	go func() { _, err := p.Discovery(t.Context(), f.session, bundle); finished <- err }()
	awaitNamingState(t, f.dsn, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT revision::text FROM agent_discovery_heads%'`, "1")
	awaitNamingState(t, f.dsn, `SELECT count(*) FROM agent_sessions WHERE expires_at<=clock_timestamp()`, "1")
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		detail, ok := errors.AsType[*agentv1.ProtocolError](err)
		if !ok || detail.Code != agentv1.ErrorCode_ERROR_CODE_FENCED {
			t.Fatal("expired session received artifact after lock wait", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not finish after lock release")
	}
	rows := pgtest.Query(t, f.dsn, `SELECT revision::text FROM agent_discovery_heads`)
	if rows[0][0] != "1" {
		t.Fatal("expired session advanced artifact head")
	}
}
