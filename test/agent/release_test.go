//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func releaseRequest(pair control.PublishedBundle) control.StartReleaseRequest {
	return control.StartReleaseRequest{Scope: grant().Scope, PrincipalID: "approved-release-owner", OperationID: "release-start", BundleHash: pair.BundleHash, BundleArtifactRevision: 1, Capacity: control.ReleaseCapacity{Profile: "production-v1", RequiredZones: []string{"az-a", "az-b"}, MinimumPerBuildZone: 2, MaximumFailureBasisPoints: 0}}
}
func releaseBytes(t *testing.T, r control.ReleaseRecord) []byte {
	t.Helper()
	b, e := json.Marshal(r, json.Deterministic(true))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func noReleaseRows(t *testing.T, dsn string) {
	t.Helper()
	for _, table := range []string{"agent_releases", "agent_release_heads", "agent_release_operations", "agent_release_events", "agent_release_resource_refs"} {
		if n := pgtest.Query(t, dsn, "SELECT count(*) FROM "+table)[0][0]; n != "0" {
			t.Fatal("failed operation left release state", table, n)
		}
	}
}

func TestReleaseReplicaGenerationDedupAbortAndHistoricalReceipts(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	pa, v := publisher(t, a)
	pb, _ := publisher(t, b)
	pair, err := pa.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(pair)
	results := make(chan control.ReleaseRecord, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			p := pa
			if i%2 != 0 {
				p = pb
			}
			r, e := p.StartRelease(t.Context(), request, v)
			if e != nil {
				t.Error(e)
				return
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	var first control.ReleaseRecord
	for r := range results {
		if first.Generation == 0 {
			first = r
		}
		if !bytes.Equal(releaseBytes(t, r), releaseBytes(t, first)) {
			t.Fatal("duplicate allocated a different release")
		}
	}
	if first.Generation != 1 || first.Revision != 1 || first.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING || first.Definition.BundleArtifactRevision != 1 || first.Definition.Capacity.MinimumPerBuildZone != 2 || !first.PrepareDeadline.Equal(first.CreatedAt.Add(120*time.Second)) {
		t.Fatal("invalid initial state", first)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_release_events`)[0][0]; n != "1" {
		t.Fatal("duplicate state events", n)
	}
	current, err := b.Release(t.Context(), request.Scope, 1)
	if err != nil || !bytes.Equal(releaseBytes(t, current), releaseBytes(t, first)) {
		t.Fatal("replica read diverged", err)
	}
	changed := request
	changed.Capacity.MinimumPerBuildZone = 3
	_, err = pb.StartRelease(t.Context(), changed, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	competing := request
	competing.OperationID = "competing"
	_, err = pb.StartRelease(t.Context(), competing, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	competing.ExpectedGeneration = 1
	_, err = pb.StartRelease(t.Context(), competing, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	abort := control.AbortReleaseRequest{Scope: request.Scope, PrincipalID: request.PrincipalID, OperationID: "release-abort", Generation: 1, ExpectedRevision: 1, Reason: "operator stopped before activation"}
	for i := range 8 {
		wg.Go(func() {
			s := a
			if i%2 != 0 {
				s = b
			}
			r, e := s.AbortRelease(t.Context(), abort)
			if e != nil {
				t.Error(e)
				return
			}
			if r.State != agentv1.ReleaseState_RELEASE_STATE_ABORTED || r.Revision != 2 {
				t.Error("wrong abort result", r)
			}
		})
	}
	wg.Wait()
	current, err = b.Release(t.Context(), request.Scope, 1)
	if err != nil || current.State != agentv1.ReleaseState_RELEASE_STATE_ABORTED || current.Revision != 2 {
		t.Fatal("abort not durable", err)
	}
	historical, err := pb.StartRelease(t.Context(), request, v)
	if err != nil || !bytes.Equal(releaseBytes(t, historical), releaseBytes(t, first)) {
		t.Fatal("creation receipt changed with current state", err)
	}
	next, err := pb.StartRelease(t.Context(), competing, v)
	if err != nil || next.Generation != 2 || next.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("new generation not monotonic", err)
	}
	// Even the aborted predecessor pins exact policy and Bundle bytes until its
	// retained release/reference lifecycle is explicitly retired.
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, deleteErr := conn.Exec(t.Context(), `DELETE FROM agent_resources WHERE resource_key='routing' AND revision=1`)
	if err := conn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if deleteErr == nil {
		t.Fatal("retained release lost its exact policy dependency")
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_release_resource_refs`)[0][0]; n != "4" {
		t.Fatal("missing exact release resource pins", n)
	}
	cold := openSessions(t, dsn)
	afterRestart, err := cold.Release(t.Context(), request.Scope, 2)
	if err != nil || afterRestart.Generation != next.Generation || afterRestart.State != next.State {
		t.Fatal("cold session handle lost release state", err)
	}
	// Receipt retention is not the generation high-water mark.
	pgtest.Exec(t, dsn, `UPDATE agent_release_operations SET expires_at=clock_timestamp()-interval '1 second'`)
	_, err = pa.StartRelease(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	_, err = a.AbortRelease(t.Context(), abort)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_release_events`)[0][0]; n != "3" {
		t.Fatal("unexpected transition history", n)
	}
}

func TestReleaseAuthorizationAndAuditFailureRollBackAllState(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(pair)
	rejected := errors.New("revoked approval")
	request.AuthorizeNewRelease = func(context.Context, pgx.Tx) error { return rejected }
	_, err = p.StartRelease(t.Context(), request, v)
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	noReleaseRows(t, dsn)
	request.AuthorizeNewRelease = nil
	request.AuthorizationExpiresAt = time.Now().Add(-time.Second)
	_, err = p.StartRelease(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	noReleaseRows(t, dsn)
	request.AuthorizationExpiresAt = time.Now().Add(2 * time.Second)
	request.AuthorizeNewRelease = func(ctx context.Context, _ pgx.Tx) error {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err = p.StartRelease(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	noReleaseRows(t, dsn)
	request.AuthorizationExpiresAt = time.Time{}
	request.AuthorizeNewRelease = nil
	pgtest.Exec(t, dsn, `CREATE FUNCTION suppress_release_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; CREATE TRIGGER suppress_release_event BEFORE INSERT ON agent_release_events FOR EACH ROW EXECUTE FUNCTION suppress_release_event()`)
	if _, err := p.StartRelease(t.Context(), request, v); err == nil {
		t.Fatal("suppressed durable state audit accepted")
	}
	noReleaseRows(t, dsn)
	pgtest.Exec(t, dsn, `DROP TRIGGER suppress_release_event ON agent_release_events`)
	r, err := p.StartRelease(t.Context(), request, v)
	if err != nil {
		t.Fatal("rolled-back operation could not retry", err)
	}
	// Abort has the same atomic state/event/receipt invariant.
	pgtest.Exec(t, dsn, `CREATE TRIGGER suppress_release_event BEFORE INSERT ON agent_release_events FOR EACH ROW EXECUTE FUNCTION suppress_release_event()`)
	abort := control.AbortReleaseRequest{Scope: request.Scope, PrincipalID: request.PrincipalID, OperationID: "abort", Generation: r.Generation, ExpectedRevision: 1, Reason: "stop"}
	if _, err := s.AbortRelease(t.Context(), abort); err == nil {
		t.Fatal("unaudited abort committed")
	}
	current, err := s.Release(t.Context(), request.Scope, r.Generation)
	if err != nil || current.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING || current.Revision != 1 {
		t.Fatal("failed abort changed state", err)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_release_operations`)[0][0]; n != "1" {
		t.Fatal("failed abort persisted operation", n)
	}
	pgtest.Exec(t, dsn, `DROP TRIGGER suppress_release_event ON agent_release_events`)
	if _, err := s.AbortRelease(t.Context(), abort); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRejectsUntrustedWrongEpochAndExactArtifactMismatch(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(pair)
	wrong := request
	wrong.BundleArtifactRevision = 2
	if _, err := p.StartRelease(t.Context(), wrong, v); err == nil {
		t.Fatal("latest fallback accepted missing artifact")
	}
	noReleaseRows(t, dsn)
	wrong = request
	wrong.ExpectedGeneration = math.MaxUint64
	_, err = p.StartRelease(t.Context(), wrong, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	noReleaseRows(t, dsn)
	wrong = request
	wrong.Scope.EnvironmentID = "foreign"
	if _, err := p.StartRelease(t.Context(), wrong, v); err == nil {
		t.Fatal("foreign scope accepted")
	}
	noReleaseRows(t, dsn)
	_, untrusted := publisher(t, s)
	// A key with the right ID and wrong public bytes is not an authority.
	roots := map[string]client.TrustKey{"policy-test-key": {PublicKey: bytes.Repeat([]byte{7}, 32), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}}
	untrusted, err = client.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartRelease(t.Context(), request, untrusted); err == nil {
		t.Fatal("untrusted Bundle accepted")
	}
	noReleaseRows(t, dsn)
	pgtest.Exec(t, dsn, `UPDATE agent_schema SET control_plane_epoch=2 WHERE id=true`)
	if _, err := p.StartRelease(t.Context(), request, v); err == nil {
		t.Fatal("old epoch Bundle accepted")
	}
	noReleaseRows(t, dsn)
}

func TestReleaseV5MigrationPreservesExactPublishedPair(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	pgtest.Exec(t, dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations,agent_preparation_decisions,agent_prepare_rejections,agent_prepared_evidence,agent_prepared_heads,agent_prepare_heads,agent_prepare_intents,agent_release_resource_refs,agent_release_events,agent_release_operations,agent_releases,agent_release_heads; UPDATE agent_schema SET version=5 WHERE id=true`)
	opened := make(chan *control.Sessions, 2)
	failures := make(chan error, 2)
	var migrators sync.WaitGroup
	for range 2 {
		migrators.Go(func() {
			s, err := control.OpenSessions(t.Context(), dsn)
			if err != nil {
				failures <- err
				return
			}
			opened <- s
		})
	}
	migrators.Wait()
	close(opened)
	close(failures)
	handles := make([]*control.Sessions, 0, 2)
	for handle := range opened {
		handles = append(handles, handle)
		t.Cleanup(handle.Close)
	}
	for err := range failures {
		t.Fatal(err)
	}
	if len(handles) != 2 {
		t.Fatal("concurrent migration lost a replica")
	}
	a, b := handles[0], handles[1]
	if n := pgtest.Query(t, dsn, `SELECT version FROM agent_schema WHERE id=true`)[0][0]; n != "11" {
		t.Fatal("migration did not advance", n)
	}
	pa, _ := publisher(t, a)
	pb, _ := publisher(t, b)
	r, err := pa.StartRelease(t.Context(), releaseRequest(pair), v)
	if err != nil || r.Generation != 1 {
		t.Fatal("migration lost exact signed dependency", err)
	}
	replay, err := pb.StartRelease(t.Context(), releaseRequest(pair), v)
	if err != nil || !bytes.Equal(releaseBytes(t, r), releaseBytes(t, replay)) {
		t.Fatal("migration replica diverged", err)
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; n != "2" {
		t.Fatal("migration rewrote publication", n)
	}
}

func TestReleaseExpiredPredecessorEpochFenceAndNoAbortAfterActivation(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(pair)
	if _, err := p.StartRelease(t.Context(), request, v); err != nil {
		t.Fatal(err)
	}
	// Move only test timestamps to exercise the expiry barrier without a 120s wait.
	pgtest.Exec(t, dsn, `UPDATE agent_releases SET created_at=clock_timestamp()-interval '4 minutes',prepare_deadline=clock_timestamp()-interval '3 minutes' WHERE generation=1`)
	request.ExpectedGeneration = 1
	request.OperationID = "after-expiry"
	second, err := p.StartRelease(t.Context(), request, v)
	if err != nil || second.Generation != 2 {
		t.Fatal("expired prepare prevented recovery", err)
	}
	old, err := s.Release(t.Context(), request.Scope, 1)
	if err != nil || old.State != agentv1.ReleaseState_RELEASE_STATE_FAILED || old.Revision != 2 {
		t.Fatal("expiry not durably failed", err)
	}
	abort := control.AbortReleaseRequest{Scope: request.Scope, PrincipalID: request.PrincipalID, OperationID: "cannot-undo", Generation: 2, ExpectedRevision: 1, Reason: "attempt to undo activation"}
	for _, state := range []agentv1.ReleaseState{agentv1.ReleaseState_RELEASE_STATE_ACTIVATING, agentv1.ReleaseState_RELEASE_STATE_VERIFYING, agentv1.ReleaseState_RELEASE_STATE_ACTIVE} {
		pgtest.Exec(t, dsn, `UPDATE agent_releases SET state=$1 WHERE generation=2`, int32(state))
		_, err := s.AbortRelease(t.Context(), abort)
		expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	}
	pgtest.Exec(t, dsn, `UPDATE agent_releases SET state=$1 WHERE generation=2`, int32(agentv1.ReleaseState_RELEASE_STATE_PREPARING))
	pgtest.Exec(t, dsn, `UPDATE agent_schema SET control_plane_epoch=2 WHERE id=true`)
	_, err = s.AbortRelease(t.Context(), abort)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	// Old epoch cannot clean up its predecessor by trying to start another old
	// Bundle. The whole attempted FAILED transition must roll back as well.
	thirdRequest := request
	thirdRequest.ExpectedGeneration = 2
	thirdRequest.OperationID = "new-epoch"
	if _, err := p.StartRelease(t.Context(), thirdRequest, v); err == nil {
		t.Fatal("old epoch started release")
	}
	current, err := s.Release(t.Context(), request.Scope, 2)
	if err != nil || current.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("failed epoch attempt mutated predecessor", err)
	}
	nextPair := bundlePairRequest(t)
	nextPair.ExpectedPolicyRevision = 1
	nextPair.Document.Policy.Revision = 2
	nextPair.Document.Policy.ControlPlaneEpoch = 2
	nextPair.OperationID = "epoch2-pair"
	fresh, err := p.PublishBundle(t.Context(), nextPair)
	if err != nil {
		t.Fatal(err)
	}
	thirdRequest.BundleHash = fresh.BundleHash
	third, err := p.StartRelease(t.Context(), thirdRequest, v)
	if err != nil || third.Generation != 3 || third.ControlPlaneEpoch != 2 {
		t.Fatal("new epoch failed recovery", err)
	}
	current, err = s.Release(t.Context(), request.Scope, 2)
	if err != nil || current.State != agentv1.ReleaseState_RELEASE_STATE_FAILED || current.Revision != 2 {
		t.Fatal("old epoch prepare not fenced", err)
	}
}

func TestReleaseDeadlineIdentityAndResourceExpiryCeiling(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pairRequest := bundlePairRequest(t)
	pairRequest.Document.ValidUntilUnixMs = time.Now().Add(30 * time.Second).UnixMilli()
	pair, err := p.PublishBundle(t.Context(), pairRequest)
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(pair)
	first, err := p.StartRelease(t.Context(), request, v)
	if err != nil {
		t.Fatal(err)
	}
	if first.PrepareDeadline.Sub(first.CreatedAt) >= 120*time.Second || first.PrepareDeadline.UnixMilli() != pair.Policy.ExpiresAt.UnixMilli() || !first.ResourcesExpiresAt.Equal(first.PrepareDeadline) {
		t.Fatal("prepare exceeded exact dependency expiry", first.PrepareDeadline, pair.Policy.ExpiresAt)
	}
	// No-deadline and an explicit Unix epoch are different intents despite both
	// having Unix milliseconds zero in an untagged representation.
	changed := request
	changed.AuthorizationExpiresAt = time.UnixMilli(0)
	_, err = p.StartRelease(t.Context(), changed, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	abort := control.AbortReleaseRequest{Scope: request.Scope, PrincipalID: request.PrincipalID, OperationID: "abort-deadline", Generation: 1, ExpectedRevision: 1, Reason: "end preparation"}
	result, err := s.AbortRelease(t.Context(), abort)
	if err != nil {
		t.Fatal(err)
	}
	abort.AuthorizationExpiresAt = time.UnixMilli(0)
	_, err = s.AbortRelease(t.Context(), abort)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	current, err := s.Release(t.Context(), request.Scope, 1)
	if err != nil || current.State != result.State {
		t.Fatal("deadline conflict changed state", err)
	}
}

func TestReleaseGenerationPreservesUint64Boundary(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(pair)
	if _, err := p.StartRelease(t.Context(), request, v); err != nil {
		t.Fatal(err)
	}
	// Construct a retained terminal predecessor at the uint64 boundary. This is
	// only a numeric-storage fixture, not evidence of an actual completed rollout.
	pgtest.Exec(t, dsn, `INSERT INTO agent_releases SELECT workspace_id,environment_id,service_key,18446744073709551614,'release-18446744073709551614',control_plane_epoch,definition,definition_sha256,9,2,created_at,prepare_deadline,resources_expires_at FROM agent_releases WHERE generation=1`)
	pgtest.Exec(t, dsn, `UPDATE agent_release_heads SET generation=18446744073709551614`)
	request.OperationID = "last-generation"
	request.ExpectedGeneration = math.MaxUint64 - 1
	last, err := p.StartRelease(t.Context(), request, v)
	if err != nil || last.Generation != math.MaxUint64 {
		t.Fatal("uint64 generation truncated", err, last.Generation)
	}
	current, err := s.Release(t.Context(), request.Scope, math.MaxUint64)
	if err != nil || current.Generation != math.MaxUint64 || current.ReleaseID != "release-18446744073709551615" {
		t.Fatal("uint64 generation readback truncated", err, current.Generation)
	}
	if n := pgtest.Query(t, dsn, `SELECT generation::text FROM agent_release_heads`)[0][0]; n != "18446744073709551615" {
		t.Fatal("generation did not persist exactly", n)
	}
	request.OperationID = "overflow"
	request.ExpectedGeneration = math.MaxUint64
	_, err = p.StartRelease(t.Context(), request, v)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
}
