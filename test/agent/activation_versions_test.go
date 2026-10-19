//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

// Reconstruct the real v10 table shape, retaining raw reports and receipts.
func activationV10Fixture(t *testing.T, dsn string) {
	t.Helper()
	activation, err := os.ReadFile("../../internal/agent/activation.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, sources, ok := strings.Cut(string(activation), "CREATE TABLE agent_activation_sources")
	if !ok {
		t.Fatal("v9 source schema missing")
	}
	reports, err := os.ReadFile("../../internal/agent/activation_reports.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, outcomes, ok := strings.Cut(string(reports), "CREATE TABLE agent_activation_reports")
	if !ok {
		t.Fatal("v10 report schema missing")
	}
	pgtest.Exec(t, dsn, `ALTER TABLE agent_activation_reports RENAME TO agent_activation_reports_new;
ALTER TABLE agent_activation_sources RENAME TO agent_activation_sources_new;
CREATE TABLE agent_activation_sources`+sources+`
CREATE TABLE agent_activation_reports`+outcomes+`
INSERT INTO agent_activation_sources SELECT workspace_id,environment_id,service_key,generation,instance_id,command_id,credential_id,prepare_command_id FROM agent_activation_sources_new;
INSERT INTO agent_activation_reports SELECT workspace_id,environment_id,service_key,generation,instance_id,command_id,outcome,revision,report_hash,report,received_at,receipt FROM agent_activation_reports_new;
DROP TABLE agent_activation_reports_new,agent_activation_sources_new,agent_activation_versions;
UPDATE agent_schema SET version=10 WHERE id=true;`)
}

func TestActivationV10MigrationPreservesExactOutcome(t *testing.T) {
	f, v, p := appliedFixture(t)
	report := appliedReport(f, p)
	first, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil {
		t.Fatal(err)
	}
	before := pgtest.Query(t, f.dsn, `SELECT encode(report_hash,'hex'),encode(report,'hex'),encode(receipt,'hex') FROM agent_activation_reports`)
	f.a.Close()
	f.b.Close()
	activationV10Fixture(t, f.dsn)
	f.a, f.b = concurrentMigrationReplicas(t, f.dsn)
	after := pgtest.Query(t, f.dsn, `SELECT encode(report_hash,'hex'),encode(report,'hex'),encode(receipt,'hex') FROM agent_activation_reports`)
	if len(after) != 1 || strings.Join(before[0], "/") != strings.Join(after[0], "/") {
		t.Fatal("migration rewrote immutable evidence", before, after)
	}
	again, err := f.b.RecordApplied(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !proto.Equal(first, again) {
		t.Fatal("migrated receipt replay", again, err)
	}
	exact, err := f.a.ActivationResource(t.Context(), f.session, "activation-1", 1, v)
	if err != nil || !bytes.Equal(exact.Body, p.Resource.Body) {
		t.Fatal("migration changed signed activation", err)
	}
	if got := pgtest.Query(t, f.dsn, `SELECT artifact_revision::text FROM agent_activation_reports`)[0][0]; got != "1" {
		t.Fatal(got)
	}
	if got := pgtest.Query(t, f.dsn, `SELECT version FROM agent_schema`)[0][0]; got != "11" {
		t.Fatal(got)
	}
}

func TestActivationArtifactsIsolateCommandsReportsAndStatus(t *testing.T) {
	f, v, p := appliedFixture(t)
	first, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, appliedReport(f, p), v)
	if err != nil {
		t.Fatal(err)
	}
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(p.Resource.Body, signed); err != nil {
		t.Fatal(err)
	}
	claims, err := v.Verify(signed, f.g.Scope, agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, "activation-1", client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := p.Document
	d.Sources = append([]client.ActivationSource(nil), d.Sources...)
	d.Sources[0].CommandID += "-v2"
	payload, err := client.MarshalActivation(d)
	if err != nil {
		t.Fatal(err)
	}
	claims.Revision = 2
	second, err := control.SignResource(claims, payload, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	conn, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Fixture only: issue a second signed version without extending any deadline.
	// Production renewal authorization and scheduling are separate, unfinished work.
	scope := f.g.Scope
	args := []any{scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, body, hash[:], d.Sources[0].CommandID}
	for _, q := range []string{
		`INSERT INTO agent_resources SELECT workspace_id,environment_id,service_key,resource_key,2,$4::bytea,$5::bytea,expires_at FROM agent_resources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key='activation/1' AND revision=1`,
		`INSERT INTO agent_activation_versions SELECT workspace_id,environment_id,service_key,generation,release_revision,resource_key,2,created_at,expires_at FROM agent_activation_versions WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_revision=1`,
		`INSERT INTO agent_activation_sources SELECT workspace_id,environment_id,service_key,generation,instance_id,$4::text,credential_id,prepare_command_id,2 FROM agent_activation_sources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND artifact_revision=1`,
		`UPDATE agent_resource_heads SET revision=2 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key='activation/1'`,
		`UPDATE agent_activations SET resource_revision=2 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`,
	} {
		// All statements use a common named argument set but only required positions.
		var use []any
		switch {
		case strings.Contains(q, "$4::text"):
			use = append(append([]any(nil), args[:3]...), args[5])
		case strings.Contains(q, "$5"):
			use = args[:5]
		default:
			use = args[:3]
		}
		if _, err := tx.Exec(t.Context(), q, use...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, invalid := conn.Exec(t.Context(), `INSERT INTO agent_activation_reports SELECT workspace_id,environment_id,service_key,generation,instance_id,command_id,outcome,999,report_hash,report,received_at,receipt,2 FROM agent_activation_reports WHERE artifact_revision=1`)
	if detail, ok := errors.AsType[*pgconn.PgError](invalid); !ok || detail.Code != "23503" {
		t.Fatal("database accepted a report spliced across artifact/source/command", invalid)
	}
	pending, err := f.b.PendingActivation(t.Context(), f.session, v)
	if err != nil || pending == nil || pending.Resource.Revision != 2 || pending.Source.CommandID != d.Sources[0].CommandID {
		t.Fatal("old report suppressed new artifact", pending, err)
	}
	status, err := f.a.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || status.ActivationArtifactRevision != 2 || len(status.Applied) != 0 || len(status.PendingSourceIds) != 1 {
		t.Fatal("status mixed artifacts", status, err)
	}
	_, err = f.a.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID, ExpectedActivationArtifactRevision: new(uint64(1))})
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESYNC_REQUIRED)
	replay, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, appliedReport(f, p), v)
	if err != nil || !proto.Equal(first, replay) {
		t.Fatal("historical immutable receipt lost", replay, err)
	}
	if _, err := f.a.RecordApplied(t.Context(), f.session, p.Source.CommandID, appliedReport(f, pending), v); err == nil {
		t.Fatal("old command accepted new artifact")
	}
	next, err := f.b.RecordApplied(t.Context(), f.session, pending.Source.CommandID, appliedReport(f, pending), v)
	if err != nil || next.ReportRevision != 2 {
		t.Fatal("new artifact result failed", next, err)
	}
	status, err = f.a.ActivationStatus(t.Context(), f.session, &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || len(status.Applied) != 1 || status.Applied[0].ActivationArtifactRevision != 2 {
		t.Fatal("latest status mixed outcomes", status, err)
	}
	old, err := f.a.ActivationResource(t.Context(), f.session, "activation-1", 1, v)
	if err != nil || !bytes.Equal(old.Body, p.Resource.Body) {
		t.Fatal("historical artifact substituted", err)
	}
	if got := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activation_reports`)[0][0]; got != "2" {
		t.Fatal(got)
	}
}

func TestActivationV10MigrationPreservesLegacyRejectedReplay(t *testing.T) {
	f, v, p := appliedFixture(t)
	report := activationRejection(f, p)
	ack, err := f.a.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil {
		t.Fatal(err)
	}
	f.a.Close()
	f.b.Close()
	activationV10Fixture(t, f.dsn)
	report.ActivationArtifactRevision = 0
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(append([]byte("grayling.agent.v1.activation-rejected\x00"), body...))
	conn, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE agent_activation_reports SET report=$1,report_hash=$2`, body, hash[:]); err != nil {
		t.Fatal(err)
	}
	conn.Close(t.Context())
	f.a = openSessions(t, f.dsn)
	f.b = openSessions(t, f.dsn)
	replay, err := f.b.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, report, v)
	if err != nil || !proto.Equal(ack, replay) {
		t.Fatal("legacy exact NACK replay lost", replay, err)
	}
	altered := proto.Clone(report).(*agentv1.Rejected)
	altered.Error.Reason = "different"
	if _, err := f.a.RecordActivationRejected(t.Context(), f.session, p.Source.CommandID, altered, v); err == nil {
		t.Fatal("legacy replay altered outcome")
	}
	server, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	wire, err := c.ReplayLegacyActivationRejected(t.Context(), p.Source.CommandID, report)
	if err != nil || !proto.Equal(ack, wire) {
		t.Fatal("legacy WSS replay changed receipt", wire, err)
	}
}

func concurrentMigrationReplicas(t *testing.T, dsn string) (*control.Sessions, *control.Sessions) {
	t.Helper()
	type opened struct {
		sessions *control.Sessions
		err      error
	}
	results := make(chan opened, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { <-start; s, err := control.OpenSessions(t.Context(), dsn); results <- opened{s, err} })
	}
	close(start)
	wg.Wait()
	close(results)
	var stores []*control.Sessions
	for result := range results {
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		t.Cleanup(result.sessions.Close)
		stores = append(stores, result.sessions)
	}
	if len(stores) != 2 {
		t.Fatal("concurrent migration did not open both replicas")
	}
	return stores[0], stores[1]
}
