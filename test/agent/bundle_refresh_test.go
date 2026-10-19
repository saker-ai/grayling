//go:build integration

package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func TestBundleRefreshReplicaDedupCASAndAuthorizationCeiling(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	p, v := publisher(t, a)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	// A longer issuer lifetime cannot enlarge the original policy authorization.
	long, err := control.NewPublisher(b, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := control.RefreshBundleRequest{Scope: grant().Scope, PrincipalID: "approved-owner", OperationID: "refresh-2", BundleHash: pair.BundleHash, ExpectedRevision: 1}
	results := make(chan control.StoredResource, 8)
	failures := make(chan error, 8)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			pub := p
			if i%2 != 0 {
				pub = long
			}
			r, err := pub.RefreshBundle(t.Context(), request, v)
			if err != nil {
				failures <- err
			} else {
				results <- r
			}
		})
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first control.StoredResource
	for result := range results {
		if first.Revision == 0 {
			first = result
		}
		if result.Revision != 2 || !bytes.Equal(first.Body, result.Body) || result.ExpiresAt.After(pair.Policy.ExpiresAt) {
			t.Fatal("refresh allocated new results or extended policy authorization")
		}
	}
	if !first.ExpiresAt.Equal(pair.Policy.ExpiresAt) {
		t.Fatal("refresh ceiling not preserved")
	}
	before := pgtest.Query(t, dsn, `SELECT encode(sha256,'hex') FROM agent_resources WHERE resource_key=$1 AND revision=1`, "bundle/"+hex.EncodeToString(pair.BundleHash[:]))[0][0]
	if before != hex.EncodeToString(pair.Bundle.Hash[:]) {
		t.Fatal("original Bundle overwritten")
	}
	next := request
	next.OperationID = "refresh-3"
	next.ExpectedRevision = 2
	third, err := long.RefreshBundle(t.Context(), next, v)
	if err != nil || third.Revision != 3 {
		t.Fatal("next artifact not allocated", err)
	}
	replay, err := p.RefreshBundle(t.Context(), request, v)
	if err != nil || !bytes.Equal(replay.Body, first.Body) {
		t.Fatal("replay returned latest instead of exact result", err)
	}
	changed := request
	changed.ExpectedRevision = 2
	_, err = p.RefreshBundle(t.Context(), changed, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	stale := request
	stale.OperationID = "stale"
	_, err = p.RefreshBundle(t.Context(), stale, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	mixed := request
	mixed.OperationID = "bundle-pair"
	_, err = p.RefreshBundle(t.Context(), mixed, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	pgtest.Exec(t, dsn, `UPDATE agent_resource_operations SET expires_at=clock_timestamp()-interval '1 second'`)
	_, err = p.RefreshBundle(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	if count := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; count != "4" {
		t.Fatal("old request allocated more resources", count)
	}
}

func TestBundleRefreshVerifiesSourceAndSignerRoots(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := control.RefreshBundleRequest{Scope: grant().Scope, PrincipalID: "approved-owner", OperationID: "refresh", BundleHash: pair.BundleHash, ExpectedRevision: 1}
	now := time.Now()
	rotated := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x67}, 32))
	next, err := control.NewPublisher(s, "rotated", rotated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.RefreshBundle(t.Context(), request, v); err == nil {
		t.Fatal("untrusted new signing root accepted")
	}
	// Changing both stored bytes and their wire digest cannot forge source authority.
	forged := signed(t, pair.Bundle.Body)
	claims, err := client.ParseClaims(forged.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.ExpiresUnixMs += 1000
	forged.Claims, err = client.MarshalClaims(claims)
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	storageKey := "bundle/" + hex.EncodeToString(pair.BundleHash[:])
	pgtest.Exec(t, dsn, `UPDATE agent_resources SET body=$2,sha256=$3,expires_at=$4 WHERE resource_key=$1 AND revision=1`, storageKey, body, hash[:], time.UnixMilli(claims.ExpiresUnixMs))
	if _, err := p.RefreshBundle(t.Context(), request, v); err == nil {
		t.Fatal("invalid historical signature was re-authorized")
	}
	pgtest.Exec(t, dsn, `UPDATE agent_resources SET body=$2,sha256=$3,expires_at=$4 WHERE resource_key=$1 AND revision=1`, storageKey, pair.Bundle.Body, pair.Bundle.Hash[:], pair.Bundle.ExpiresAt)
	if count := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; count != "2" {
		t.Fatal("invalid roots/source left new artifact", count)
	}
	if count := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resource_operations`)[0][0]; count != "1" {
		t.Fatal("failed refresh left operation result", count)
	}
	roots := map[string]client.TrustKey{"policy-test-key": {PublicKey: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)).Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}, "rotated": {PublicKey: rotated.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}}
	overlap, err := client.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := next.RefreshBundle(t.Context(), request, overlap)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := client.VerifyBundle(overlap, signed(t, fresh.Body), request.Scope, pair.BundleHash, client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bundle.VerifyPolicy(overlap, signed(t, pair.Policy.Body), client.Watermark{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	delete(roots, "policy-test-key")
	retired, err := client.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = 2
	request.OperationID = "retired-source"
	if _, err := next.RefreshBundle(t.Context(), request, retired); err == nil {
		t.Fatal("retired source root authorized refresh")
	}
}

func TestBundleRefreshWSSAdvancesCacheWithoutRenewingOldView(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	g := grant()
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	srv, _ := channelServer(t, s)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cache, err := client.InitializeBundleCache(t.Context(), client.BundleCacheOptions{Directory: filepath.Join(t.TempDir(), "bundle"), Identity: g.AgentID, Scope: g.Scope, AuthenticationKey: bytes.Repeat([]byte{0x44}, 32), Verifier: v, BundleHash: pair.BundleHash, MaxAge: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	fetch := &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: client.BundleKey(pair.BundleHash), Revision: 1}
	ref, err := c.FetchResource(t.Context(), fetch)
	if err != nil {
		t.Fatal(err)
	}
	original, err := c.Download(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	old, err := cache.Capture()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := cache.Install(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Capture(); !errors.Is(err, client.ErrBundleStale) {
		t.Fatal("old artifact replay refreshed cache", err)
	}
	refreshed, err := p.RefreshBundle(t.Context(), control.RefreshBundleRequest{Scope: g.Scope, PrincipalID: "approved-owner", OperationID: "network-refresh", BundleHash: pair.BundleHash, ExpectedRevision: 1}, v)
	if err != nil {
		t.Fatal(err)
	}
	fetch.Revision = 2
	nextRef, err := c.FetchResource(t.Context(), fetch)
	if err != nil || nextRef.Sha256 != hex.EncodeToString(refreshed.Hash[:]) {
		t.Fatal("new immutable artifact not fetchable", err)
	}
	fresh, err := c.Download(t.Context(), nextRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	current, err := cache.Capture()
	if err != nil || current.Claims().Revision != 2 {
		t.Fatal("new signed artifact did not refresh cache", err)
	}
	if _, err := old.Document(); !errors.Is(err, client.ErrBundleStale) {
		t.Fatal("refresh renewed old captured view", err)
	}
	if _, err := current.Document(); err != nil {
		t.Fatal(err)
	}
}

func TestBundleRefreshFailureLeavesNoNewArtifactOrOperation(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := control.RefreshBundleRequest{Scope: grant().Scope, PrincipalID: "approved-owner", OperationID: "refresh", BundleHash: pair.BundleHash, ExpectedRevision: 1}
	foreign := request
	foreign.Scope.EnvironmentID += "-foreign"
	_, err = p.RefreshBundle(t.Context(), foreign, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	pgtest.Exec(t, dsn, `UPDATE agent_schema SET control_plane_epoch=control_plane_epoch+1 WHERE id=true`)
	_, err = p.RefreshBundle(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	// Restore test fixtures only; production epoch recovery never decrements it.
	pgtest.Exec(t, dsn, `UPDATE agent_schema SET control_plane_epoch=control_plane_epoch-1 WHERE id=true`)
	pgtest.Exec(t, dsn, `UPDATE agent_resources SET expires_at=clock_timestamp()-interval '1 second' WHERE resource_key='routing' AND revision=1`)
	_, err = p.RefreshBundle(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	pgtest.Exec(t, dsn, `UPDATE agent_resources SET expires_at=$1 WHERE resource_key='routing' AND revision=1`, pair.Policy.ExpiresAt)
	pgtest.Exec(t, dsn, `ALTER TABLE agent_resources ADD CONSTRAINT test_deny_refresh CHECK(resource_key NOT LIKE 'bundle/%' OR revision=1)`)
	if _, err := p.RefreshBundle(t.Context(), request, v); err == nil {
		t.Fatal("injected artifact INSERT failure accepted")
	}
	for table, want := range map[string]string{"agent_resources": "2", "agent_resource_heads": "2", "agent_resource_operations": "1"} {
		if count := pgtest.Query(t, dsn, `SELECT count(*) FROM `+table)[0][0]; count != want {
			t.Fatal("failed refresh left durable state", table, count)
		}
	}
	if revision := pgtest.Query(t, dsn, `SELECT revision::text FROM agent_resource_heads WHERE resource_key=$1`, "bundle/"+hex.EncodeToString(pair.BundleHash[:]))[0][0]; revision != "1" {
		t.Fatal("failed refresh advanced head", revision)
	}
	pgtest.Exec(t, dsn, `ALTER TABLE agent_resources DROP CONSTRAINT test_deny_refresh`)
	if result, err := p.RefreshBundle(t.Context(), request, v); err != nil || result.Revision != 2 {
		t.Fatal("rolled-back refresh could not retry", err)
	}
}
