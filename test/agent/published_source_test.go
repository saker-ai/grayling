//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	gconfig "github.com/godeps/gonacos/pkg/config"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/admin"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
)

type approvedBundlePublisherFunc func(context.Context, control.PublishBundleRequest) (control.PublishedBundle, error)

func (f approvedBundlePublisherFunc) PublishBundle(ctx context.Context, r control.PublishBundleRequest) (control.PublishedBundle, error) {
	return f(ctx, r)
}

func TestPublishedSourceReplicaReadToSignedWSSPolicy(t *testing.T) {
	dsn := pgtest.DSN(t)
	store, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ws, err := store.CreateWorkspace("source", "Source")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	actor := func(name string, role platform.Role) admin.Actor {
		t.Helper()
		token, err := store.CreateToken(platform.TokenAdmin, name, role, ws.ID, "", "source test")
		if err != nil {
			t.Fatal(err)
		}
		return admin.Actor{TokenID: token.Token.ID, Name: name, Kind: platform.TokenAdmin, Role: role, WorkspaceID: ws.ID}
	}
	writer := actor("writer", platform.RolePublisher)
	owner := actor("approver", platform.RoleOwner)
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	source := admin.NewService(store, cfg, nil)
	flag := &model.Flag{SchemaVersion: 1, Key: "route", Type: model.FlagString, Variations: map[string]any{"stable": "backend"}, DefaultRule: &model.Rule{Variation: "stable"}}
	if _, err := source.SaveFlagDraft(writer, ws.ID, env.ID, flag, platform.RiskNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := source.SubmitFlagDraft(writer, ws.ID, env.ID, flag.Key); err != nil {
		t.Fatal(err)
	}
	if err := source.ApproveFlagDraft(owner, ws.ID, env.ID, flag.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.PublishFlagContext(t.Context(), owner, ws.ID, env.ID, flag.Key, ""); err != nil {
		t.Fatal(err)
	}
	replica, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	reader := admin.NewService(replica, cfg, nil)
	published, err := reader.LoadPublishedSnapshotContext(t.Context(), owner, ws.ID, env.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sessions := openSessions(t, dsn)
	epoch, now, err := sessions.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	document := client.BundleDocument{SchemaVersion: 1, Purpose: "release-bundle", Scope: client.Scope{WorkspaceID: ws.ID, EnvironmentID: env.ID, ServiceKey: "checkout"}, Policy: client.BundlePolicy{Key: "routing", ControlPlaneEpoch: epoch, Revision: 1, PayloadSHA256: hex.EncodeToString(published.SHA256[:])}, RouteFlag: "route", ValueBuilds: map[string]string{"backend": "stable"}, MinimumProtocolVersion: 1, DefaultBehavior: "reject", ValidUntilUnixMs: now.Add(time.Minute).UnixMilli(), Builds: []client.BundleBuild{{ID: "stable", ImageDigest: "sha256:" + strings.Repeat("a", 64), ProtocolVersion: 1, Resources: []client.BundleResource{}}}}
	bundleBody, err := client.MarshalBundle(document)
	if err != nil {
		t.Fatal(err)
	}
	bundleHash := sha256.Sum256(bundleBody)
	p, v := publisher(t, sessions)
	approval, err := reader.RequestBundlePublication(t.Context(), writer, document.Scope, 1, 0, document, "publish exact source Bundle")
	if err != nil {
		t.Fatal(err)
	}
	// Approval on another replica becomes visible only after scope reload.
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveChange(approval.ID, owner.Name, owner.TokenID, "reviewed source and build mapping"); err != nil {
		t.Fatal(err)
	}
	lostResponse := errors.New("committed effect response lost")
	pair, err := reader.PublishApprovedBundle(t.Context(), writer, document.Scope, approval.ID, approvedBundlePublisherFunc(func(ctx context.Context, request control.PublishBundleRequest) (control.PublishedBundle, error) {
		result, err := p.PublishBundle(ctx, request)
		if err != nil {
			return result, err
		}
		return result, lostResponse
	}))
	if !errors.Is(err, lostResponse) {
		t.Fatal("failed to simulate unknown outcome", err)
	}
	replay, err := source.PublishApprovedBundle(t.Context(), owner, document.Scope, approval.ID, p)
	if err != nil || !bytes.Equal(replay.Bundle.Body, pair.Bundle.Body) || !bytes.Equal(replay.Policy.Body, pair.Policy.Body) {
		t.Fatal("cross-replica approval replay changed signed pair", err)
	}
	bundle, _, err := client.VerifyBundle(v, signed(t, pair.Bundle.Body), document.Scope, bundleHash, client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	g := grant()
	g.Scope = document.Scope
	if err := sessions.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, sessions, g)
	srv, _ := channelServer(t, sessions)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bundleRef, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: client.BundleKey(pair.BundleHash), Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	networkBundle, _, err := c.DownloadBundle(t.Context(), bundleRef, v, g.Scope, pair.BundleHash, client.Watermark{}, time.Now())
	if err != nil || networkBundle.Hash() != bundle.Hash() {
		t.Fatal("published source Bundle delivery changed identity", err)
	}
	bundle = networkBundle
	ref, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: document.Policy.Key, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := c.Download(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	policy, claims, err := bundle.VerifyPolicy(v, delivered, client.Watermark{}, time.Now())
	if err != nil || claims.PayloadHash != published.SHA256 || policy.Evaluate("route", model.EvaluationContext{}).Value != "backend" {
		t.Fatal("approved source did not reach bound policy evaluator", err)
	}

	// Revoke the reviewer after the service working-set check and before the
	// actual effect transaction. The transaction guard must read authoritative rows.
	nextDocument := document
	nextDocument.Policy.Revision = 2
	nextApproval, err := reader.RequestBundlePublication(t.Context(), writer, document.Scope, 1, 1, nextDocument, "next exact revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveChange(nextApproval.ID, owner.Name, owner.TokenID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = reader.PublishApprovedBundle(t.Context(), writer, document.Scope, nextApproval.ID, approvedBundlePublisherFunc(func(ctx context.Context, request control.PublishBundleRequest) (control.PublishedBundle, error) {
		if err := store.ReloadFromBackend(); err != nil {
			return control.PublishedBundle{}, err
		}
		if err := store.RevokeToken(owner.TokenID); err != nil {
			return control.PublishedBundle{}, err
		}
		return p.PublishBundle(ctx, request)
	}))
	if !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatal("effect minted after reviewer revocation", err)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; n != "2" {
		t.Fatal("revoked effect left resources", n)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resource_operations`)[0][0]; n != "1" {
		t.Fatal("revoked effect left operation", n)
	}
}
