//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling"
	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestBundleHTTPApprovalPublicationAndReplay(t *testing.T) {
	dsn := pgtest.DSN(t)
	server := httptest.NewUnstartedServer(nil)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x49}, 32))
	app, err := grayling.Start(t.Context(), grayling.Options{PostgresDSN: dsn, GonacosData: filepath.Join(t.TempDir(), "gonacos"), AuthSecret: "bundle-http-test-stable-secret", AgentEnabled: true, AgentAllowedHosts: []string{server.Listener.Addr().String()}, AgentMaxConnections: 4, AgentSigningKeyID: "bundle-http", AgentSigningKey: key})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	server.Config.Handler = app.Handler()
	server.StartTLS()
	defer server.Close()
	root := app.BootstrapOwnerToken()
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces", map[string]string{"slug": "bundles", "name": "Bundles"}, 201)
	env := decodeAdmin[platform.Environment](t, adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/bundles/environments", map[string]string{"slug": "prod", "name": "Production"}, 201))
	base := "/api/v1/admin/workspaces/bundles/environments/prod"
	flag := model.Flag{SchemaVersion: 1, Key: "route", Type: model.FlagString, Variations: map[string]any{"stable": "backend"}, DefaultRule: &model.Rule{Variation: "stable"}}
	adminHTTP(t, server, root, "PUT", base+"/flags/route/draft", map[string]any{"content": flag, "baseRevision": 0, "expectedDraftHash": "", "risk": "normal"}, 200)
	adminHTTP(t, server, root, "POST", base+"/flags/route/submit", map[string]any{"reason": "publish route source"}, 200)
	adminHTTP(t, server, root, "POST", base+"/flags/route/approve", map[string]any{}, 200)
	adminHTTP(t, server, root, "POST", base+"/flags/route/publish", map[string]any{"note": "approved source publication"}, 200)
	mint := func(role string) platform.CreatedToken {
		return decodeAdmin[platform.CreatedToken](t, adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/bundles/tokens", map[string]string{"name": role, "kind": "admin", "role": role}, 201))
	}
	publisher := mint("publisher")
	viewer := mint("viewer")
	path := base + "/agent/services/checkout/bundles"
	request := map[string]any{"sourceRevision": 1, "expectedPolicyRevision": "0", "policyKey": "routing", "routeFlag": "route", "valueBuilds": map[string]string{"backend": "stable"}, "minimumProtocolVersion": 1, "defaultBehavior": "reject", "validUntilUnixMs": time.Now().Add(time.Hour).UnixMilli(), "builds": []client.BundleBuild{{ID: "stable", ImageDigest: "sha256:" + strings.Repeat("a", 64), ProtocolVersion: 1, Resources: []client.BundleResource{}}}, "reason": "release exact snapshot"}
	request["validUntilUnixMs"] = strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	adminHTTP(t, server, viewer.Secret, "POST", path+"/approvals", request, 403)
	invalid := map[string]any{}
	for k, v := range request {
		invalid[k] = v
	}
	invalid["controlPlaneEpoch"] = "999"
	adminHTTP(t, server, publisher.Secret, "POST", path+"/approvals", invalid, 400)
	missingCAS := map[string]any{}
	for k, v := range request {
		missingCAS[k] = v
	}
	delete(missingCAS, "expectedPolicyRevision")
	adminHTTP(t, server, publisher.Secret, "POST", path+"/approvals", missingCAS, 400)
	invalidSource := map[string]any{}
	for k, v := range request {
		invalidSource[k] = v
	}
	invalidSource["sourceRevision"] = 2
	adminHTTP(t, server, publisher.Secret, "POST", path+"/approvals", invalidSource, 409)
	approval := decodeAdmin[platform.Approval](t, adminHTTP(t, server, publisher.Secret, "POST", path+"/approvals", request, 201))
	if approval.Action != admin.BundlePublishAction || approval.Object != "checkout" || approval.EnvironmentID != env.ID || approval.BaseRevision != 1 {
		t.Fatal("wrong approval binding", approval)
	}
	var proposal admin.BundlePublicationProposal
	if err := json.Unmarshal([]byte(approval.Payload), &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.Document.Scope.EnvironmentID != env.ID || proposal.Document.Policy.ControlPlaneEpoch != 1 || proposal.Document.Policy.Revision != 1 || len(proposal.PolicyPayload) == 0 || proposal.Document.Policy.PayloadSHA256 == "" {
		t.Fatal("missing server-owned publication basis")
	}
	execute := map[string]string{"approvalId": approval.ID}
	adminHTTP(t, server, publisher.Secret, "POST", path+"/publications", execute, 409)
	adminHTTP(t, server, viewer.Secret, "POST", path+"/publications", execute, 403)
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/bundles/approvals/"+approval.ID+"/approve", map[string]any{}, 200)
	// Force the post-effect audit to fail after the Agent pair has committed.
	pgtest.Exec(t, dsn, `CREATE FUNCTION fail_bundle_publication_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF position('agent.bundle.published' in convert_from(NEW.payload,'UTF8'))>0 THEN RAISE EXCEPTION 'injected publication audit failure'; END IF; RETURN NEW; END $$`)
	pgtest.Exec(t, dsn, `CREATE TRIGGER fail_bundle_publication_audit BEFORE UPDATE ON database_states FOR EACH ROW EXECUTE FUNCTION fail_bundle_publication_audit()`)
	uncertain := adminHTTP(t, server, publisher.Secret, "POST", path+"/publications", execute, 503)
	if !bytes.Contains(uncertain, []byte("bundle_publication_unavailable")) {
		t.Fatal("lost unknown-outcome marker", string(uncertain))
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; n != "2" {
		t.Fatal("did not commit effect before audit failure", n)
	}
	pgtest.Exec(t, dsn, `DROP TRIGGER fail_bundle_publication_audit ON database_states`)
	first := adminHTTP(t, server, publisher.Secret, "POST", path+"/publications", execute, 200)
	replay := adminHTTP(t, server, root, "POST", path+"/publications", execute, 200)
	if !bytes.Equal(first, replay) {
		t.Fatal("HTTP result replay changed metadata", string(first), string(replay))
	}
	metadata := decodeAdmin[map[string]any](t, first)
	if metadata["artifactRevision"] != "1" || metadata["policyRevision"] != "1" || len(metadata["bundleHash"].(string)) != 64 {
		t.Fatal("invalid publication metadata", metadata)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; n != "2" {
		t.Fatal("HTTP replay minted another pair", n)
	}
	adminHTTP(t, server, publisher.Secret, "POST", base+"/agent/services/foreign/bundles/publications", execute, 403)
	adminRawHTTP(t, server, publisher.Secret, "POST", path+"/publications", []byte(`{"approvalId":"`+approval.ID+`","operationId":"new"}`), 400)
	adminRawHTTP(t, server, publisher.Secret, "POST", path+"/approvals", bytes.Repeat([]byte(" "), 33<<10), 400)
	stale := decodeAdmin[platform.Approval](t, adminHTTP(t, server, publisher.Secret, "POST", path+"/approvals", request, 201))
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/bundles/approvals/"+stale.ID+"/approve", map[string]any{}, 200)
	adminHTTP(t, server, publisher.Secret, "POST", path+"/publications", map[string]string{"approvalId": stale.ID}, 409)
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; n != "2" {
		t.Fatal("stale approval minted another pair", n)
	}
	// A confirmed approval ID survives failure of its subsequent request audit.
	pgtest.Exec(t, dsn, `CREATE FUNCTION fail_bundle_request_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF length(convert_from(NEW.payload,'UTF8'))-length(replace(convert_from(NEW.payload,'UTF8'),'agent.bundle.requested','')) > length(convert_from(OLD.payload,'UTF8'))-length(replace(convert_from(OLD.payload,'UTF8'),'agent.bundle.requested','')) THEN RAISE EXCEPTION 'injected request audit failure'; END IF; RETURN NEW; END $$`)
	pgtest.Exec(t, dsn, `CREATE TRIGGER fail_bundle_request_audit BEFORE UPDATE ON database_states FOR EACH ROW EXECUTE FUNCTION fail_bundle_request_audit()`)
	requestUncertain := decodeAdmin[map[string]any](t, adminHTTP(t, server, publisher.Secret, "POST", path+"/approvals", request, 503))
	knownID, ok := requestUncertain["approvalId"].(string)
	if !ok || knownID == "" {
		t.Fatal("lost committed approval identity", requestUncertain)
	}
	pgtest.Exec(t, dsn, `DROP TRIGGER fail_bundle_request_audit ON database_states`)
	approvals := decodeAdmin[[]platform.Approval](t, adminHTTP(t, server, root, "GET", "/api/v1/admin/workspaces/bundles/approvals", nil, 200))
	found := false
	for _, candidate := range approvals {
		if candidate.ID == knownID && candidate.State == platform.DraftSubmitted {
			found = true
		}
	}
	if !found {
		t.Fatal("created approval not discoverable", knownID)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM database_states WHERE position($1 in convert_from(payload,'UTF8'))>0`, `"id":"`+knownID+`"`)[0][0]; n != "1" {
		t.Fatal("approval creation not durable", n)
	}

	plain := httptest.NewServer(app.Handler())
	defer plain.Close()
	adminHTTP(t, plain, publisher.Secret, "POST", path+"/publications", execute, 403)
}

func TestBundleHTTPRoutesDisabledWithoutSigner(t *testing.T) {
	dsn := pgtest.DSN(t)
	server := httptest.NewUnstartedServer(nil)
	app, err := grayling.Start(t.Context(), grayling.Options{PostgresDSN: dsn, GonacosData: filepath.Join(t.TempDir(), "gonacos"), AuthSecret: "bundle-keyless-test-stable-secret", AgentEnabled: true, AgentAllowedHosts: []string{server.Listener.Addr().String()}, AgentMaxConnections: 1})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	server.Config.Handler = app.Handler()
	server.StartTLS()
	defer server.Close()
	for _, suffix := range []string{"approvals", "publications"} {
		adminHTTP(t, server, app.BootstrapOwnerToken(), "POST", "/api/v1/admin/workspaces/unknown/environments/prod/agent/services/gateway/bundles/"+suffix, map[string]any{}, 404)
	}
}
