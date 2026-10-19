//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
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
	"google.golang.org/protobuf/proto"
)

func activationRequest(f *registrationFixture) control.StartActivationRequest {
	return control.StartActivationRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "activate", Generation: 1, ExpectedRevision: 2, RequiredSourceIDs: []string{f.g.InstanceID}}
}
func activationFixture(t *testing.T) (*registrationFixture, *control.Publisher, *client.Verifier, control.ReleaseRecord) {
	t.Helper()
	f, p, v, r := preparedFixture(t)
	intent := queueGateTarget(t, f, v, "prepare-activation", 0)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	gate, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate-activation", 1), v)
	if err != nil || !gate.Eligible {
		t.Fatal("prepare gate", err)
	}
	return f, p, v, gate.Release
}
func activationBundleHash(t *testing.T, r control.ReleaseRecord) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(r.Definition.BundleHash)
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	copy(hash[:], b)
	return hash
}

func TestActivationAtomicReplicaReplayPinnedViewAndSource(t *testing.T) {
	f, p, v, r := activationFixture(t)
	q := activationRequest(f)
	otherPublisher, _ := publisher(t, f.b)
	out := make(chan control.ActivationReceipt, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			node := p
			if i%2 != 0 {
				node = otherPublisher
			}
			receipt, err := node.StartActivation(t.Context(), q, v)
			if err != nil {
				t.Error(err)
				return
			}
			out <- receipt
		})
	}
	wg.Wait()
	close(out)
	var first control.ActivationReceipt
	for receipt := range out {
		if first.Release.Revision == 0 {
			first = receipt
		}
		if !bytes.Equal(preparedBytes(t, first), preparedBytes(t, receipt)) {
			t.Fatal("activation replay changed")
		}
	}
	if first.Release.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING || first.Release.Revision != 3 || first.PreparedRevision != 1 || first.SourceCount != 1 || first.ViewKey != client.ActivationKey(1) || first.ExpiresAt.After(r.ResourcesExpiresAt) {
		t.Fatal("invalid activation receipt", first)
	}
	resource, err := f.b.ActivationResource(t.Context(), f.session, first.ViewKey, 1, v)
	if err != nil {
		t.Fatal(err)
	}
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(resource.Body, signed); err != nil {
		t.Fatal(err)
	}
	view, claims, err := client.VerifyActivation(v, signed, f.g.Scope, 1, activationBundleHash(t, r), client.Watermark{}, time.Now())
	if err != nil || claims.Revision != 1 || claims.ControlPlaneEpoch != r.ControlPlaneEpoch || view.Document().PreparedRevision != 1 || len(view.Document().Members) != 1 || len(view.Document().Sources) != 1 {
		t.Fatal("frozen view unavailable", err)
	}
	document := view.Document()
	document.Members[0].Instance.Address = "10.20.0.99"
	document.Sources[0].BootID = "mutated"
	if view.Document().Members[0].Instance.Address == "10.20.0.99" || view.Document().Sources[0].BootID == "mutated" {
		t.Fatal("mutable activation view")
	}
	bundle, err := f.a.BundleResource(t.Context(), f.session, activationBundleHash(t, r), r.Definition.BundleArtifactRevision)
	if err != nil {
		t.Fatal(err)
	}
	bs := new(agentv1.SignedResource)
	proto.Unmarshal(bundle.Body, bs)
	if _, _, err := view.VerifyBundle(v, bs, client.Watermark{}, time.Now()); err != nil {
		t.Fatal("exact Bundle join", err)
	}
	if _, err := f.b.ActivationResource(t.Context(), f.session, first.ViewKey, 0, v); err == nil {
		t.Fatal("latest activation alias allowed")
	}
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activations`)[0][0]; n != "1" {
		t.Fatal("duplicate activation rows")
	}
	_, err = f.a.AbortRelease(t.Context(), control.AbortReleaseRequest{Scope: f.g.Scope, PrincipalID: "controller", OperationID: "late-abort", Generation: 1, ExpectedRevision: 3, Reason: "too late"})
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	changed := q
	changed.RequiredSourceIDs = []string{"different"}
	_, err = p.StartActivation(t.Context(), changed, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	// Cold handle can serve exact immutable view; a retained activation pins it.
	cold := openSessions(t, f.dsn)
	again, err := cold.ActivationResource(t.Context(), f.session, first.ViewKey, 1, v)
	if err != nil || !bytes.Equal(again.Body, resource.Body) {
		t.Fatal("restart changed view", err)
	}
	db, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(t.Context(), `DELETE FROM agent_resources WHERE resource_key='activation/1'`); err == nil {
		t.Fatal("retained view deletion allowed")
	}
}

func TestActivationRefusesHistoricalPreparedCapacityAndUnpreparedSource(t *testing.T) {
	f, p, v, r := preparedFixture(t)
	before := activationRequest(f)
	before.ExpectedRevision = 1
	_, err := p.StartActivation(t.Context(), before, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	intent := queueGateTarget(t, f, v, "short", 0)
	report := preparedReport(f, r)
	report.ExpiresUnixMs = time.Now().Add(1500 * time.Millisecond).UnixMilli()
	if _, err := f.a.RecordPrepared(t.Context(), f.session, intent.CommandID, report, v); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate", 1), v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.UnixMilli(report.ExpiresUnixMs)) + 10*time.Millisecond)
	_, err = p.StartActivation(t.Context(), activationRequest(f), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activations`)[0][0]; n != "0" {
		t.Fatal("expired capacity minted activation")
	}
	// Refresh with a new target revision, retaining the immutable release.
	next := queueGateTarget(t, f, v, "fresh", 1)
	if _, err := f.a.RecordPrepared(t.Context(), f.session, next.CommandID, preparedReport(f, r), v); err != nil {
		t.Fatal(err)
	}
	missing := activationRequest(f)
	missing.RequiredSourceIDs = []string{"not-prepared-source"}
	_, err = p.StartActivation(t.Context(), missing, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal("fresh current capacity could not recover", err)
	}
}

func TestActivationWriteRollbackDeadlineAndSignerTrust(t *testing.T) {
	f, p, v, _ := activationFixture(t)
	assertClean := func() {
		t.Helper()
		r, err := f.b.Release(t.Context(), f.g.Scope, 1)
		if err != nil || r.Revision != 2 || r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARED {
			t.Fatal("partial activation", r, err)
		}
		if n := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_activations)+(SELECT count(*) FROM agent_activation_versions)+(SELECT count(*) FROM agent_activation_sources)+(SELECT count(*) FROM agent_resources WHERE resource_key='activation/1')+(SELECT count(*) FROM agent_resource_heads WHERE resource_key='activation/1')`)[0][0]; n != "0" {
			t.Fatal("partial artifacts", n)
		}
	}
	denied := activationRequest(f)
	denied.Authorize = func(context.Context, pgx.Tx) error { return errors.New("approval rejected") }
	if _, err := p.StartActivation(t.Context(), denied, v); err == nil {
		t.Fatal("guard ignored")
	}
	assertClean()
	for _, table := range []string{"agent_resource_heads", "agent_resources", "agent_release_events", "agent_activations", "agent_activation_versions", "agent_activation_sources"} {
		pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_activation() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; CREATE TRIGGER suppress_activation BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION suppress_activation()`)
		if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err == nil {
			t.Fatal("suppressed write committed", table)
		}
		assertClean()
		pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_activation ON `+table+`; DROP FUNCTION suppress_activation()`)
	}
	deadline := activationRequest(f)
	deadline.AuthorizationExpiresAt = time.Now().Add(150 * time.Millisecond)
	deadline.Authorize = func(context.Context, pgx.Tx) error { time.Sleep(180 * time.Millisecond); return nil }
	_, err := p.StartActivation(t.Context(), deadline, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	assertClean()
	bad, err := control.NewPublisher(f.a, "untrusted", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.StartActivation(t.Context(), activationRequest(f), v); err == nil {
		t.Fatal("untrusted signer minted view")
	}
	assertClean()
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
}

func TestActivationSourceWSSExactDownloadAndOtherInstanceDenied(t *testing.T) {
	f, p, v, r := activationFixture(t)
	other := secondGateInstance(t, f, "az-a")
	receipt, err := p.StartActivation(t.Context(), activationRequest(f), v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.ActivationResource(t.Context(), other.session, receipt.ViewKey, 1, v); err == nil {
		t.Fatal("non-source downloaded activation")
	}
	srv, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activation.read"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ref, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, Key: receipt.ViewKey, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	view, claims, err := c.DownloadActivation(t.Context(), ref, v, f.g.Scope, 1, activationBundleHash(t, r), client.Watermark{}, time.Now())
	if err != nil || claims.Revision != 1 || len(view.Document().Sources) != 1 {
		t.Fatal("real activation download", err)
	}
	if _, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, Key: receipt.ViewKey}); err == nil {
		t.Fatal("network latest alias accepted")
	}
	// An old connection cannot keep fetching after the current boot is replaced.
	takeover := f.g
	takeover.BootID = "activation-successor"
	takeover.TakeoverFromBootID = f.g.BootID
	_, secret := issue(t, f.a, takeover)
	if _, err := f.a.Connect(t.Context(), secret, helloFor(takeover)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.DownloadActivation(t.Context(), ref, v, f.g.Scope, 1, activationBundleHash(t, r), client.Watermark{}, time.Now()); err == nil {
		t.Fatal("old boot downloaded view")
	}
}

func TestActivationV8MigrationSourceIntentAndPermission(t *testing.T) {
	f, _, v, _ := activationFixture(t)
	f.a.Close()
	f.b.Close()
	pgtest.Exec(t, f.dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations; UPDATE agent_schema SET version=8 WHERE id=true`)
	f.a = openSessions(t, f.dsn)
	f.b = openSessions(t, f.dsn)
	p, _ := publisher(t, f.b)
	// Preparing/reporting is not permission to consume the membership view.
	pgtest.Exec(t, f.dsn, `UPDATE agent_credentials SET permissions=permissions & ~$1::bigint WHERE id=$2`, int64(control.DiscoveryRead), f.session.Grant.ID)
	_, err := p.StartActivation(t.Context(), activationRequest(f), v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	pgtest.Exec(t, f.dsn, `UPDATE agent_credentials SET permissions=permissions | $1::bigint WHERE id=$2`, int64(control.DiscoveryRead), f.session.Grant.ID)
	q := activationRequest(f)
	for _, sources := range [][]string{nil, {f.g.InstanceID, f.g.InstanceID}, {"z", "a"}} {
		bad := q
		bad.RequiredSourceIDs = sources
		if _, err := p.StartActivation(t.Context(), bad, v); err == nil {
			t.Fatal("invalid source set accepted")
		}
	}
	receipt, err := p.StartActivation(t.Context(), q, v)
	if err != nil {
		t.Fatal("v8 migration lost Prepared state", err)
	}
	if _, err := f.a.ActivationResource(t.Context(), f.session, receipt.ViewKey, 1, v); err != nil {
		t.Fatal("migration lost current lease/session", err)
	}
	if n := pgtest.Query(t, f.dsn, `SELECT version FROM agent_schema WHERE id=true`)[0][0]; n != "11" {
		t.Fatal("migration version", n)
	}
}
