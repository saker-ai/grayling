//go:build integration

package agent_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godeps/gonacos/pkg/server"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

type rejectNamingReadback struct{ control.NamingBackend }

func (rejectNamingReadback) Verify(context.Context, control.NamingIntent) error {
	return control.ErrNamingProjection
}
func TestInstanceRealEmbeddedNamingReconcileAndStaleCleanup(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	substrate, err := server.New(server.WithAddr("127.0.0.1:0"), server.WithGRPCAddr("127.0.0.1:0"), server.WithDataDir(filepath.Join(t.TempDir(), "gonacos")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := substrate.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	native := substrate.Services().Naming
	var namespace atomic.Value
	native.SetPushFunc(func(value, _, _ string) { namespace.Store(value) })
	backend, err := control.NewEmbeddedNaming(native)
	if err != nil {
		t.Fatal(err)
	}
	first := registerInstance(t, f, "initial")
	result, err := f.a.ReconcileNaming(t.Context(), f.g.Scope, rejectNamingReadback{backend}, 2)
	if !errors.Is(err, control.ErrNamingProjection) || result.Failed != 1 || result.Confirmed != 0 || native.CountAllInstances() != 1 {
		t.Fatalf("projection failure outcome: %+v %v", result, err)
	}
	snapshot, err := f.b.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || snapshot.Members[0].NamingConfirmed {
		t.Fatal("Naming write ACK was treated as verified publication")
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_naming_intents SET next_attempt_at=clock_timestamp()`)
	oldWrites, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(oldWrites) != 1 {
		t.Fatal("missing initial write")
	}
	draining := proto.Clone(first.Instance).(*agentv1.Instance)
	draining.Draining = true
	if _, err := f.b.UpdateRegisteredInstance(t.Context(), f.session, "drain", &agentv1.UpdateInstance{Instance: draining}); err != nil {
		t.Fatal(err)
	}
	result, err = f.a.ReconcileNaming(t.Context(), f.g.Scope, backend, 2)
	if err != nil || result.Confirmed != 2 {
		t.Fatalf("real readback failed: %+v %v", result, err)
	}
	snapshot, err = f.b.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || !snapshot.Members[0].NamingConfirmed || !snapshot.Members[0].Instance.Draining {
		t.Fatal("reconciled descriptor/readiness diverged")
	}
	instances, err := native.ListInstances(namespace.Load().(string), "GRAYLING_AGENT", f.g.Scope.ServiceKey, "", false)
	if err != nil || len(instances) != 1 || instances[0].Healthy || instances[0].Enabled || instances[0].Metadata["bootId"] != f.g.BootID {
		t.Fatal("native projection metadata/health mismatch")
	}
	// An old same-lease write completes after the newer revision is confirmed.
	if err := backend.Upsert(t.Context(), oldWrites[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.a.ConfirmNamingIntent(t.Context(), oldWrites[0]); err == nil {
		t.Fatal("old same-lease confirmation accepted")
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_naming_intents SET next_attempt_at=clock_timestamp()`)
	current, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range current {
		if intent.Present {
			if err := backend.Verify(t.Context(), intent); err != nil {
				t.Fatal("late old write corrupted current projection", err)
			}
		}
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_naming_intents SET next_attempt_at=clock_timestamp()`)
	if _, err := f.a.ReconcileNaming(t.Context(), f.g.Scope, backend, 2); err != nil {
		t.Fatal(err)
	}
	if native.CountAllInstances() != 1 {
		t.Fatal("late old projection was not cleaned")
	}
	// Native metadata tampering cannot satisfy exact readback, even if a caller
	// leaves the stored descriptor hash untouched.
	tampered := instances[0]
	tampered.Metadata = maps.Clone(tampered.Metadata)
	tampered.Metadata["buildId"] = "forged-build"
	if err := native.UpdateInstance(tampered); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_naming_intents SET next_attempt_at=clock_timestamp()`)
	intents, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(intents) != 2 {
		t.Fatal("missing refresh intent")
	}
	for _, intent := range intents {
		if intent.Present {
			intents[0] = intent
			break
		}
	}
	if err := backend.Verify(t.Context(), intents[0]); !errors.Is(err, control.ErrNamingProjection) {
		t.Fatal("modified native build passed readback")
	}
	replacement := registerInstance(t, f, "replacement")
	// Two intents share IP/port but use different incarnation/cluster identities.
	result, err = f.b.ReconcileNaming(t.Context(), f.g.Scope, backend, 2)
	if err != nil || result.Confirmed != 3 {
		t.Fatalf("replacement and cleanup: %+v %v", result, err)
	}
	if native.CountAllInstances() != 1 {
		t.Fatal("old cleanup deleted successor or retained old member")
	}
	instances, err = native.ListInstances(namespace.Load().(string), "GRAYLING_AGENT", f.g.Scope.ServiceKey, "", false)
	if err != nil || len(instances) != 1 || !strings.HasPrefix(instances[0].InstanceID, replacement.Instance.Lease.Id+"-r") || instances[0].Metadata["buildId"] != "build-a" {
		t.Fatal("successor projection did not survive old cleanup")
	}
	old := intents[0]
	old.Present = false
	if err := backend.Remove(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if native.CountAllInstances() != 1 {
		t.Fatal("delayed duplicate old delete removed current address reuse")
	}
	if err := f.a.ConfirmNamingIntent(t.Context(), intents[0]); err == nil {
		t.Fatal("old projection confirmation overrode successor")
	}
	snapshot, err = f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(snapshot.Members) != 1 || !snapshot.Members[0].NamingConfirmed || snapshot.Members[0].Instance.Lease.Id != replacement.Instance.Lease.Id {
		t.Fatal("governance and successor projection diverged")
	}
}
