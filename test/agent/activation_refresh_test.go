//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestActivationRefreshReplicaReplayAndNewApplied(t *testing.T) {
	f, _, v, _ := activationFixture(t)
	makePublisher := func(s *control.Sessions) *control.Publisher {
		p, err := control.NewPublisher(s, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := makePublisher(f.a), makePublisher(f.b)
	first, err := a.StartActivation(t.Context(), activationRequest(f), v)
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.a.PendingActivation(t.Context(), f.session, v)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	if _, err := f.a.RecordApplied(t.Context(), f.session, old.Source.CommandID, appliedReport(f, old), v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(first.ExpiresAt) + 10*time.Millisecond)
	q := activationRequest(f)
	q.OperationID = "refresh"
	q.ExpectedRevision = 4
	results := make(chan control.ActivationReceipt, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			p := a
			if i%2 != 0 {
				p = b
			}
			r, err := p.RefreshActivation(t.Context(), q, 1, v)
			if err != nil {
				t.Error(err)
				return
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	var receipt control.ActivationReceipt
	for r := range results {
		if receipt.ArtifactRevision == 0 {
			receipt = r
		}
		if !bytes.Equal(preparedBytes(t, receipt), preparedBytes(t, r)) {
			t.Fatal("replica replay changed refresh")
		}
	}
	if receipt.ArtifactRevision != 2 || receipt.Release.Revision != 5 || receipt.Release.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING || !receipt.ExpiresAt.After(first.ExpiresAt) || receipt.ExpiresAt.After(time.UnixMilli(old.Document.Members[0].ExpiresUnixMs)) {
		t.Fatal("invalid renewal", receipt)
	}
	next, err := f.b.PendingActivation(t.Context(), f.session, v)
	if err != nil || next == nil || next.Resource.Revision != 2 || next.Source.CommandID == old.Source.CommandID {
		t.Fatal("new command missing", next, err)
	}
	ack, err := f.b.RecordApplied(t.Context(), f.session, next.Source.CommandID, appliedReport(f, next), v)
	if err != nil || ack.ReportRevision != 2 || ack.ReleaseRevision != 6 {
		t.Fatal("fresh Applied missing", ack, err)
	}
	replay, err := a.RefreshActivation(t.Context(), q, 1, v)
	if err != nil || !bytes.Equal(preparedBytes(t, receipt), preparedBytes(t, replay)) {
		t.Fatal("later replay changed artifact", err)
	}
	q.OperationID = "stale-artifact"
	q.ExpectedRevision = 6
	_, err = a.RefreshActivation(t.Context(), q, 1, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	q.OperationID = "change-source"
	q.RequiredSourceIDs = append(q.RequiredSourceIDs, "other-source")
	slices.Sort(q.RequiredSourceIDs)
	if _, err := a.RefreshActivation(t.Context(), q, 2, v); err == nil {
		t.Fatal("refresh changed frozen source set")
	}
	if got := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activation_reports`)[0][0]; got != "2" {
		t.Fatal(got)
	}
}

func TestActivationRefreshRollbackAndAuthorityGuards(t *testing.T) {
	f, p, v, _ := activationFixture(t)
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	q := activationRequest(f)
	q.ExpectedRevision = 3
	q.OperationID = "refresh"
	clean := func() {
		t.Helper()
		if got := pgtest.Query(t, f.dsn, `SELECT resource_revision::text FROM agent_activations`)[0][0]; got != "1" {
			t.Fatal("partial head", got)
		}
		if got := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activation_versions`)[0][0]; got != "1" {
			t.Fatal("partial version", got)
		}
		r, err := f.b.Release(t.Context(), f.g.Scope, 1)
		if err != nil || r.Revision != 3 || r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING {
			t.Fatal("partial release state", r, err)
		}
		if got := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_release_operations WHERE operation_id='refresh'`)[0][0]; got != "0" {
			t.Fatal("partial operation receipt", got)
		}
	}
	q.Authorize = func(context.Context, pgx.Tx) error { return errors.New("approval denied") }
	if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
		t.Fatal("ignored authorization")
	}
	clean()
	q.Authorize = nil
	for _, table := range []string{"agent_resources", "agent_activation_versions", "agent_activation_sources", "agent_release_resource_refs", "agent_release_events"} {
		pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_refresh() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; CREATE TRIGGER suppress_refresh BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION suppress_refresh()`)
		if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
			t.Fatal("suppressed write committed", table)
		}
		clean()
		pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_refresh ON `+table+`; DROP FUNCTION suppress_refresh()`)
	}
	for _, table := range []string{"agent_resource_heads", "agent_activations"} {
		pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_refresh_update() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; CREATE TRIGGER suppress_refresh_update BEFORE UPDATE ON `+table+` FOR EACH ROW EXECUTE FUNCTION suppress_refresh_update()`)
		if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
			t.Fatal("suppressed head update committed", table)
		}
		clean()
		pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_refresh_update ON `+table+`; DROP FUNCTION suppress_refresh_update()`)
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
	if _, err := tx.Exec(t.Context(), `LOCK TABLE agent_activation_versions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	started := time.Now()
	_, err = p.RefreshActivation(deadline, q, 1, v)
	cancel()
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatal("refresh ignored caller deadline", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	clean()
	q.AuthorizationExpiresAt = time.Now().Add(50 * time.Millisecond)
	q.Authorize = func(context.Context, pgx.Tx) error { time.Sleep(80 * time.Millisecond); return nil }
	if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
		t.Fatal("expired approval committed")
	}
	clean()
	q.Authorize = nil
	q.AuthorizationExpiresAt = time.Time{}
	pgtest.Exec(t, f.dsn, `UPDATE agent_instances SET expires_at=clock_timestamp()-interval '1 second'`)
	if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
		t.Fatal("expired lease renewed activation")
	}
	clean()
}

func TestActivationRefreshWSSInstallsActualNewView(t *testing.T) {
	f, _, v, _ := activationFixture(t)
	p, err := control.NewPublisher(f.a, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var builds atomic.Int64
	runtime, err := client.InitializeRuntime(t.Context(), client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{49}, 32), Verifier: v, Now: time.Now, Builder: func(_ context.Context, s client.RuntimeSpec) (client.RuntimeResources, error) {
		builds.Add(1)
		return client.RuntimeResources{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meta, ok := client.RuntimeFromContext(r.Context())
			if !ok || meta != s.Metadata {
				t.Error("request mixed refreshed view")
			}
			_, _ = w.Write([]byte(strconv.FormatUint(meta.Watermark.ActivationRevision, 10)))
		}), Close: func() error { return nil }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	initial := clientEvent(t, c)
	one, err := c.InstallActivation(t.Context(), initial, runtime)
	if err != nil {
		t.Fatal(err)
	}
	old, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	q := activationRequest(f)
	q.OperationID = "refresh-installed"
	q.ExpectedRevision = one.Receipt.ReleaseRevision
	receipt, err := p.RefreshActivation(t.Context(), q, 1, v)
	if err != nil {
		t.Fatal(err)
	}
	event := clientEvent(t, c)
	if event.GetActivate() == nil || event.GetActivate().View.Revision != 2 || event.OperationId == initial.OperationId {
		t.Fatal("refreshed command not delivered", event)
	}
	two, err := c.InstallActivation(t.Context(), event, runtime)
	if err != nil || two.Applied.ActivationArtifactRevision != 2 || two.Receipt.ReportRevision != 2 || builds.Load() != 2 {
		t.Fatal("refreshed view not installed and applied", two, err)
	}
	current, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	oldResponse, newResponse := httptest.NewRecorder(), httptest.NewRecorder()
	if err := old.ServeHTTP(oldResponse, httptest.NewRequest("GET", "https://service/business", nil)); err != nil {
		t.Fatal(err)
	}
	if err := current.ServeHTTP(newResponse, httptest.NewRequest("GET", "https://service/business", nil)); err != nil {
		t.Fatal(err)
	}
	if oldResponse.Body.String() != "1" || newResponse.Body.String() != "2" {
		t.Fatal("old/new requests mixed runtime versions", oldResponse.Body.String(), newResponse.Body.String())
	}
	replay, err := c.InstallActivation(t.Context(), event, runtime)
	if err != nil || replay.Receipt.ReportRevision != two.Receipt.ReportRevision || builds.Load() != 2 {
		t.Fatal("refresh retry rebuilt view", replay, err)
	}
	status, err := c.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: receipt.Release.ReleaseID})
	if err != nil || status.ActivationArtifactRevision != 2 || status.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || len(status.Applied) != 1 {
		t.Fatal("refresh inferred ACTIVE or mixed outcomes", status, err)
	}
}

func TestActivationRefreshRejectsTamperedProofSignerAndRevokedBundle(t *testing.T) {
	f, p, v, _ := activationFixture(t)
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	current, err := f.a.PendingActivation(t.Context(), f.session, v)
	if err != nil || current == nil {
		t.Fatal(err)
	}
	q := activationRequest(f)
	q.ExpectedRevision = 3
	q.OperationID = "refresh-proof-guards"
	clean := func() {
		t.Helper()
		if got := pgtest.Query(t, f.dsn, `SELECT resource_revision::text FROM agent_activations`)[0][0]; got != "1" {
			t.Fatal("partial refreshed head", got)
		}
	}
	bad, err := control.NewPublisher(f.a, "untrusted", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.RefreshActivation(t.Context(), q, 1, v); err == nil {
		t.Fatal("untrusted issuer refreshed")
	}
	clean()
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(current.Resource.Body, signed); err != nil {
		t.Fatal(err)
	}
	signed.Signature[0] ^= 1
	tampered, err := proto.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(tampered)
	conn, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `UPDATE agent_resources SET body=$1,sha256=$2 WHERE resource_key='activation/1'`, tampered, hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
		t.Fatal("tampered proof was laundered through refresh")
	}
	clean()
	if _, err := conn.Exec(t.Context(), `UPDATE agent_resources SET body=$1,sha256=$2 WHERE resource_key='activation/1'`, current.Resource.Body, current.Resource.Hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("deny-refresh"), control.RevokeBundlesRequest{BundleHashes: []string{current.Release.Definition.BundleHash}, Reason: "deny release"}, v); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RefreshActivation(t.Context(), q, 1, v); err == nil {
		t.Fatal("revoked Bundle refreshed activation")
	}
	clean()
}
