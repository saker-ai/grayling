package admin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

type recordingBundlePublisher struct {
	requests []control.PublishBundleRequest
	err      error
}

func (p *recordingBundlePublisher) PublishBundle(_ context.Context, r control.PublishBundleRequest) (control.PublishedBundle, error) {
	p.requests = append(p.requests, r)
	return control.PublishedBundle{}, p.err
}

func bundleApprovalFixture(t *testing.T) (*fixture, client.Scope, client.BundleDocument) {
	t.Helper()
	f := newFixture(t)
	flag := testFlag("route")
	flag.Type = model.FlagString
	flag.Variations = map[string]any{"on": "canary", "off": "stable"}
	fullPublishFlow(t, f, f.publisher, f.owner, flag)
	source, err := f.svc.LoadPublishedSnapshotContext(t.Context(), f.publisher, f.ws.ID, f.env.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	scope := client.Scope{WorkspaceID: f.ws.ID, EnvironmentID: f.env.ID, ServiceKey: "gateway"}
	document := client.BundleDocument{SchemaVersion: 1, Purpose: "release-bundle", Scope: scope, Policy: client.BundlePolicy{Key: "routing", ControlPlaneEpoch: 1, Revision: 1, PayloadSHA256: hex.EncodeToString(source.SHA256[:])}, RouteFlag: "route", ValueBuilds: map[string]string{"stable": "stable", "canary": "canary"}, MinimumProtocolVersion: 1, DefaultBehavior: "reject", ValidUntilUnixMs: time.Now().Add(time.Hour).UnixMilli(), Builds: []client.BundleBuild{{ID: "canary", ImageDigest: "sha256:" + strings.Repeat("a", 64), ProtocolVersion: 1, Resources: []client.BundleResource{}}, {ID: "stable", ImageDigest: "sha256:" + strings.Repeat("b", 64), ProtocolVersion: 1, Resources: []client.BundleResource{}}}}
	return f, scope, document
}

func TestBundleApprovalExactReceiptRecoversFailedEffect(t *testing.T) {
	f, scope, doc := bundleApprovalFixture(t)
	a, err := f.svc.RequestBundlePublication(t.Context(), f.publisher, scope, 1, 0, doc, "deploy exact approved source")
	if err != nil {
		t.Fatal(err)
	}
	p := &recordingBundlePublisher{err: errors.New("unknown transaction outcome")}
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher, scope, a.ID, p); !errors.Is(err, platform.ErrInvalidState) {
		t.Fatal("unapproved publication", err)
	}
	if len(p.requests) != 0 {
		t.Fatal("effect before approval")
	}
	approved, err := f.store.ApproveChange(a.ID, f.owner.Name, f.owner.TokenID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher, scope, a.ID, p); !errors.Is(err, p.err) {
		t.Fatal(err)
	}
	consumed, err := f.store.GetApproval(a.ID)
	if err != nil || consumed.State != platform.ApprovalUsed {
		t.Fatal("missing recoverable receipt", err)
	}
	p.err = nil
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher2, scope, a.ID, p); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 {
		t.Fatal("effect not retried")
	}
	first, second := p.requests[0], p.requests[1]
	if first.PrincipalID != second.PrincipalID || first.OperationID != second.OperationID || first.AuthorizationExpiresAt != *approved.ExpiresAt || second.AuthorizationExpiresAt != *approved.ExpiresAt || !bytes.Equal(first.PolicyPayload, second.PolicyPayload) {
		t.Fatal("retry changed approved operation")
	}
	if _, err := client.CompileBundlePolicy(first.Document, first.PolicyPayload); err != nil {
		t.Fatal(err)
	}
	wrong := scope
	wrong.ServiceKey = "foreign"
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher, wrong, a.ID, p); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatal("cross service approval", err)
	}
	if err := f.store.RevokeToken(f.owner.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher, scope, a.ID, p); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatal("revoked reviewer accepted", err)
	}
}

func TestBundleApprovalRejectsChangedSourceAndRevokedCaller(t *testing.T) {
	f, scope, doc := bundleApprovalFixture(t)
	bad := doc
	bad.Policy.PayloadSHA256 = strings.Repeat("a", 64)
	if _, err := f.svc.RequestBundlePublication(t.Context(), f.publisher, scope, 1, 0, bad, "bad"); !errors.Is(err, platform.ErrConflict) {
		t.Fatal("caller digest accepted", err)
	}
	a, err := f.svc.RequestBundlePublication(t.Context(), f.publisher, scope, 1, 0, doc, "approved source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApproveChange(a.ID, f.owner.Name, f.owner.TokenID, ""); err != nil {
		t.Fatal(err)
	}
	// A new approved publication invalidates first consumption of an old basis.
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("additional"))
	p := &recordingBundlePublisher{}
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher, scope, a.ID, p); !errors.Is(err, platform.ErrConflict) {
		t.Fatal("stale source accepted", err)
	}
	if len(p.requests) != 0 {
		t.Fatal("signed stale source")
	}
	if err := f.store.RevokeToken(f.publisher.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublishApprovedBundle(t.Context(), f.publisher, scope, a.ID, p); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatal("revoked caller accepted", err)
	}
}
