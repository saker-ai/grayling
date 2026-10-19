//go:build integration

package agent_test

import (
	"bytes"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/test/pgtest"
	"testing"
	"time"
)

func TestBundleAuthorizationDeadlineAndReplay(t *testing.T) {
	dsn := pgtest.DSN(t)
	sessions := openSessions(t, dsn)
	p, _ := publisher(t, sessions)
	req := bundlePairRequest(t)
	req.AuthorizationExpiresAt = time.Now().Add(-time.Second)
	_, err := p.PublishBundle(t.Context(), req)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; n != "0" {
		t.Fatal("expired grant wrote resource", n)
	}
	req.AuthorizationExpiresAt = time.Now().Add(time.Second).Truncate(time.Millisecond)
	result, err := p.PublishBundle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the exact original deadline, then recover only the original result.
	timer := time.NewTimer(time.Until(req.AuthorizationExpiresAt) + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	replay, err := p.PublishBundle(t.Context(), req)
	if err != nil || !bytes.Equal(result.Bundle.Body, replay.Bundle.Body) || !bytes.Equal(result.Policy.Body, replay.Policy.Body) {
		t.Fatal("receipt replay changed signed pair", err)
	}
	req.AuthorizationExpiresAt = req.AuthorizationExpiresAt.In(time.FixedZone("other", 3600)).Add(time.Microsecond)
	if _, err := p.PublishBundle(t.Context(), req); err != nil {
		t.Fatal("same millisecond deadline changed operation digest", err)
	}
	req.AuthorizationExpiresAt = req.AuthorizationExpiresAt.Add(time.Millisecond)
	_, err = p.PublishBundle(t.Context(), req)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
}

func TestBundleAuthorizationExpiresBeforeCommitRollsBack(t *testing.T) {
	dsn := pgtest.DSN(t)
	sessions := openSessions(t, dsn)
	p, _ := publisher(t, sessions)
	pgtest.Exec(t, dsn, `CREATE FUNCTION delay_bundle_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(2); RETURN NEW; END $$`)
	pgtest.Exec(t, dsn, `CREATE TRIGGER delay_bundle_insert BEFORE INSERT ON agent_resources FOR EACH ROW EXECUTE FUNCTION delay_bundle_insert()`)
	req := bundlePairRequest(t)
	req.AuthorizationExpiresAt = time.Now().Add(time.Second)
	_, err := p.PublishBundle(t.Context(), req)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	for _, table := range []string{"agent_resources", "agent_resource_heads", "agent_resource_operations"} {
		if n := pgtest.Query(t, dsn, "SELECT count(*) FROM "+table)[0][0]; n != "0" {
			t.Fatal("expired publication partially committed", table, n)
		}
	}
}
