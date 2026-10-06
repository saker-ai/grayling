package native

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/pkg/evaluation"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/gonacos/pkg/server"
	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

func nativeServer(t *testing.T) *server.Server {
	t.Helper()
	// The official SDK uses the configured HTTP port and its explicit gRPC port.
	srv, err := server.New(server.WithAddr("127.0.0.1:0"), server.WithGRPCAddr("127.0.0.1:0"), server.WithDataDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = srv.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })
	return srv
}

func nativeClient(t *testing.T, srv *server.Server, namespace string) config_client.IConfigClient {
	t.Helper()
	_, httpPort, err := net.SplitHostPort(srv.HTTPAddr())
	if err != nil {
		t.Fatal(err)
	}
	_, grpcPort, err := net.SplitHostPort(srv.GRPCAddr())
	if err != nil {
		t.Fatal(err)
	}
	hp, _ := strconv.Atoi(httpPort)
	gp, _ := strconv.Atoi(grpcPort)
	c, err := clients.NewConfigClient(vo.NacosClientParam{
		ClientConfig: constant.NewClientConfig(
			constant.WithNamespaceId(namespace), constant.WithTimeoutMs(3000),
			constant.WithNotLoadCacheAtStart(true), constant.WithCacheDir(t.TempDir()), constant.WithLogDir(t.TempDir())),
		ServerConfigs: []constant.ServerConfig{*constant.NewServerConfig("127.0.0.1", uint64(hp), constant.WithGrpcPort(uint64(gp)))},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseClient)
	return c
}

func publish(t *testing.T, store configstore.Store, namespace string, revision int64, value bool) {
	t.Helper()
	flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: "checkout", Type: model.FlagBool,
		Variations: map[string]any{"on": true, "off": false}, DefaultRule: &model.Rule{Variation: "off"}}
	if value {
		flag.DefaultRule.Variation = "on"
	}
	b, err := model.CanonicalJSON(flag)
	if err != nil {
		t.Fatal(err)
	}
	dataID := configstore.FlagDataID(flag.Key, revision)
	if _, err := store.PublishDoc(namespace, dataID, b); err != nil {
		t.Fatal(err)
	}
	m := &model.Manifest{SchemaVersion: model.SchemaVersion, Workspace: "ws", Environment: "env", Revision: revision,
		Flags: []model.ManifestEntry{{Key: flag.Key, Revision: revision, DataID: dataID, Hash: model.HashBytes(b)}}}
	if err := store.PublishManifest(namespace, m); err != nil {
		t.Fatal(err)
	}
}

// nativeSnapshot fetches exactly the manifest's referenced documents through
// the official SDK. A caller may swap the returned evaluator only on success.
func nativeSnapshot(client config_client.IConfigClient) (*evaluation.Evaluator, int64, error) {
	manifestContent, err := client.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group})
	if err != nil {
		return nil, 0, err
	}
	var m model.Manifest
	if err := json.Unmarshal([]byte(manifestContent), &m); err != nil {
		return nil, 0, err
	}
	if m.SchemaVersion != model.SchemaVersion {
		return nil, 0, fmt.Errorf("manifest schema version %d", m.SchemaVersion)
	}
	snap := &model.Snapshot{SchemaVersion: m.SchemaVersion, Workspace: m.Workspace, Environment: m.Environment,
		ManifestRevision: m.Revision, Flags: map[string]*model.Flag{}, Segments: map[string]*model.Segment{}}
	for _, entry := range m.Flags {
		if entry.Deleted {
			continue
		}
		content, err := client.GetConfig(vo.ConfigParam{DataId: entry.DataID, Group: configstore.Group})
		if err != nil {
			return nil, 0, fmt.Errorf("fetch %s: %w", entry.DataID, err)
		}
		if model.HashBytes([]byte(content)) != entry.Hash {
			return nil, 0, fmt.Errorf("hash mismatch for %s", entry.DataID)
		}
		var flag model.Flag
		if err := json.Unmarshal([]byte(content), &flag); err != nil {
			return nil, 0, err
		}
		if flag.Key != entry.Key {
			return nil, 0, fmt.Errorf("key mismatch for %s", entry.DataID)
		}
		snap.Flags[flag.Key] = &flag
	}
	for _, entry := range m.Segments {
		if entry.Deleted {
			continue
		}
		content, err := client.GetConfig(vo.ConfigParam{DataId: entry.DataID, Group: configstore.Group})
		if err != nil {
			return nil, 0, fmt.Errorf("fetch %s: %w", entry.DataID, err)
		}
		if model.HashBytes([]byte(content)) != entry.Hash {
			return nil, 0, fmt.Errorf("hash mismatch for %s", entry.DataID)
		}
		var segment model.Segment
		if err := json.Unmarshal([]byte(content), &segment); err != nil {
			return nil, 0, err
		}
		if segment.Key != entry.Key {
			return nil, 0, fmt.Errorf("key mismatch for %s", entry.DataID)
		}
		snap.Segments[segment.Key] = &segment
	}
	e, err := evaluation.NewEvaluator(snap)
	return e, m.Revision, err
}

// nativeConsumer models the minimum state a native client integration must
// expose. A failed refresh never publishes an incomplete candidate.
type nativeConsumer struct {
	evaluator *evaluation.Evaluator
	revision  int64
	lastErr   error
}

func (c *nativeConsumer) refresh(client config_client.IConfigClient) {
	candidate, revision, err := nativeSnapshot(client)
	c.apply(candidate, revision, err)
}

// apply also handles a completed fetch that arrives after a newer revision.
// Only a fully verified, strictly newer candidate may replace the live view.
func (c *nativeConsumer) apply(candidate *evaluation.Evaluator, revision int64, err error) {
	c.lastErr = err
	if err == nil && revision > c.revision {
		c.evaluator, c.revision = candidate, revision
	}
}

func (c *nativeConsumer) value(key string) any {
	return c.evaluator.Evaluate(key, model.EvaluationContext{}).Value
}

func publishNativeFlag(t *testing.T, store configstore.Store, namespace, key string, revision int64, value bool) model.ManifestEntry {
	t.Helper()
	variation := "off"
	if value {
		variation = "on"
	}
	flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: key, Type: model.FlagBool,
		Variations: map[string]any{"on": true, "off": false}, DefaultRule: &model.Rule{Variation: variation}}
	body, err := model.CanonicalJSON(flag)
	if err != nil {
		t.Fatal(err)
	}
	dataID := configstore.FlagDataID(key, revision)
	if _, err := store.PublishDoc(namespace, dataID, body); err != nil {
		t.Fatal(err)
	}
	return model.ManifestEntry{Key: key, Revision: revision, DataID: dataID, Hash: model.HashBytes(body)}
}

func TestNativeSnapshotAtomicityAndStatus(t *testing.T) {
	srv := nativeServer(t)
	store := configstore.NewGonacosStore(srv.Services().Config)
	const namespace = "native-atomic"
	client := nativeClient(t, srv, namespace)
	first := publishNativeFlag(t, store, namespace, "first", 1, false)
	second := publishNativeFlag(t, store, namespace, "second", 1, false)
	manifest := &model.Manifest{SchemaVersion: model.SchemaVersion, Workspace: "ws", Environment: "env",
		Revision: 1, Flags: []model.ManifestEntry{first, second}}
	if err := store.PublishManifest(namespace, manifest); err != nil {
		t.Fatal(err)
	}
	var consumer nativeConsumer
	consumer.refresh(client)
	if consumer.lastErr != nil || consumer.revision != 1 || consumer.value("first") != false || consumer.value("second") != false {
		t.Fatalf("initial state: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
	staleEvaluator, staleRevision, err := nativeSnapshot(client)
	if err != nil {
		t.Fatal(err)
	}

	// One valid new document and one missing document must not expose a mix.
	first = publishNativeFlag(t, store, namespace, "first", 2, true)
	manifest.Revision = 2
	manifest.Flags = []model.ManifestEntry{first, {Key: "second", Revision: 2,
		DataID: configstore.FlagDataID("second", 2), Hash: model.HashBytes([]byte("missing"))}}
	if err := store.PublishManifest(namespace, manifest); err != nil {
		t.Fatal(err)
	}
	consumer.refresh(client)
	if consumer.lastErr == nil || consumer.revision != 1 || consumer.value("first") != false || consumer.value("second") != false {
		t.Fatalf("partial snapshot visible: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}

	// A present document with a bad hash is equally ineligible.
	second = publishNativeFlag(t, store, namespace, "second", 3, true)
	second.Hash = model.HashBytes([]byte("wrong"))
	manifest.Revision = 3
	manifest.Flags = []model.ManifestEntry{first, second}
	if err := store.PublishManifest(namespace, manifest); err != nil {
		t.Fatal(err)
	}
	consumer.refresh(client)
	if consumer.lastErr == nil || consumer.revision != 1 || consumer.value("first") != false || consumer.value("second") != false {
		t.Fatalf("invalid hash replaced last known good: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}

	// Repair with a newer complete manifest.
	second = publishNativeFlag(t, store, namespace, "second", 4, true)
	first = publishNativeFlag(t, store, namespace, "first", 4, true)
	manifest.Revision = 4
	manifest.Flags = []model.ManifestEntry{first, second}
	if err := store.PublishManifest(namespace, manifest); err != nil {
		t.Fatal(err)
	}
	consumer.refresh(client)
	if consumer.lastErr != nil || consumer.revision != 4 || consumer.value("first") != true || consumer.value("second") != true {
		t.Fatalf("repair did not converge: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
	// Finish an older in-flight fetch after revision 4 was installed.
	consumer.apply(staleEvaluator, staleRevision, nil)
	if consumer.revision != 4 || consumer.value("first") != true || consumer.value("second") != true {
		t.Fatalf("delayed candidate replaced revision 4: revision=%d", consumer.revision)
	}
}

func TestOfficialNacosClientPublishSubscribeEvaluate(t *testing.T) {
	srv := nativeServer(t)
	store := configstore.NewGonacosStore(srv.Services().Config)
	const namespace = "native-test-a"
	client := nativeClient(t, srv, namespace)
	publish(t, store, namespace, 1, false)
	e, revision, err := nativeSnapshot(client)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 1 || e.Evaluate("checkout", model.EvaluationContext{}).Value != false {
		t.Fatalf("initial revision=%d result=%+v", revision, e.Evaluate("checkout", model.EvaluationContext{}))
	}
	changes := make(chan int64, 8)
	err = client.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, content string) {
			var changed model.Manifest
			if json.Unmarshal([]byte(content), &changed) != nil {
				return
			}
			select {
			case changes <- changed.Revision:
			default:
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	publish(t, store, namespace, 2, true)
	deadline := time.After(10 * time.Second)
	observedPush := false
	for revision < 2 || !observedPush {
		select {
		case notified := <-changes:
			if notified == 2 {
				observedPush = true
			}
		case <-time.After(200 * time.Millisecond): // missed notification reconciliation
		case <-deadline:
			t.Fatal("native subscription did not converge to r2")
		}
		candidate, next, err := nativeSnapshot(client)
		if err == nil && next > revision {
			e, revision = candidate, next
		}
	}
	if e.Evaluate("checkout", model.EvaluationContext{}).Value != true {
		t.Fatalf("r2 result=%+v", e.Evaluate("checkout", model.EvaluationContext{}))
	}
	// A newer manifest with a missing document cannot replace last known good.
	bad := &model.Manifest{SchemaVersion: model.SchemaVersion, Workspace: "ws", Environment: "env", Revision: 3,
		Flags: []model.ManifestEntry{{Key: "checkout", Revision: 3, DataID: configstore.FlagDataID("checkout", 3), Hash: "missing"}}}
	if err := store.PublishManifest(namespace, bad); err != nil {
		t.Fatal(err)
	}
	if _, _, err := nativeSnapshot(client); err == nil {
		t.Fatal("missing document accepted")
	}
	if e.Evaluate("checkout", model.EvaluationContext{}).Value != true {
		t.Fatal("last known good changed")
	}
	// Repair the manifest and show a new official client converges without any
	// earlier push notification (the reconnect / lost-notification path).
	publish(t, store, namespace, 4, false)
	reconnected := nativeClient(t, srv, namespace)
	var newEval *evaluation.Evaluator
	var next int64
	deadline = time.After(10 * time.Second)
	for next != 4 {
		newEval, next, err = nativeSnapshot(reconnected)
		if err == nil && next == 4 {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("reconnect did not converge: revision=%d err=%v", next, err)
		}
	}
	if err != nil || next != 4 || newEval.Evaluate("checkout", model.EvaluationContext{}).Value != false {
		t.Fatalf("reconnect revision=%d evaluator=%v err=%v", next, newEval, err)
	}
}

// Polling the manifest must converge even when this same client never receives
// a notification. In particular, intermediate revisions need not be observed.
func TestOfficialNacosClientConvergesAfterMissedNotifications(t *testing.T) {
	srv := nativeServer(t)
	store := configstore.NewGonacosStore(srv.Services().Config)
	const namespace = "native-missed-notifications"
	client := nativeClient(t, srv, namespace)
	publish(t, store, namespace, 1, false)
	var consumer nativeConsumer
	consumer.refresh(client)
	if consumer.lastErr != nil || consumer.revision != 1 || consumer.value("checkout") != false {
		t.Fatalf("initial state: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}

	// The client is kept alive, but has no listener for either publication.
	publish(t, store, namespace, 2, true)
	publish(t, store, namespace, 3, false)
	if consumer.revision != 1 || consumer.value("checkout") != false {
		t.Fatal("unverified revision became visible before reconciliation")
	}
	consumer.refresh(client)
	if consumer.lastErr != nil || consumer.revision != 3 || consumer.value("checkout") != false {
		t.Fatalf("manifest reconciliation: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
}

// Restart the real service on the same ports while retaining the official
// client's subscription. The client must reconcile from the current manifest
// after its established gRPC connection is severed.
func TestOfficialNacosClientReconnectsAfterServerRestart(t *testing.T) {
	dataDir := t.TempDir()
	srv, err := server.New(server.WithAddr("127.0.0.1:0"), server.WithGRPCAddr("127.0.0.1:0"),
		server.WithDataDir(dataDir), server.WithShutdownTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	httpAddr, grpcAddr := srv.HTTPAddr(), srv.GRPCAddr()
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Start(ctx) }()
	firstStopped := false
	t.Cleanup(func() {
		if !firstStopped {
			cancel()
			<-stopped
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = srv.Shutdown(cleanupCtx)
	})
	const namespace = "native-restart"
	client := nativeClient(t, srv, namespace)
	store := configstore.NewGonacosStore(srv.Services().Config)
	publish(t, store, namespace, 1, false)
	var consumer nativeConsumer
	consumer.refresh(client)
	if consumer.lastErr != nil || consumer.revision != 1 || consumer.value("checkout") != false {
		t.Fatalf("initial state: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
	changes := make(chan int64, 8)
	if err := client.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, content string) {
			var m model.Manifest
			if json.Unmarshal([]byte(content), &m) == nil {
				select {
				case changes <- m.Revision:
				default:
				}
			}
		}}); err != nil {
		t.Fatal(err)
	}

	envelope, err := srv.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-stopped // active subscription may force gonacos' graceful shutdown deadline
	firstStopped = true
	// The same client remains alive. Start a new service on the original ports
	// and publish while its old subscription connection is gone.
	restarted, err := server.New(server.WithAddr(httpAddr), server.WithGRPCAddr(grpcAddr), server.WithDataDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Restore(envelope); err != nil {
		t.Fatal(err)
	}
	restartCtx, stopRestart := context.WithCancel(t.Context())
	restartDone := make(chan error, 1)
	go func() { restartDone <- restarted.Start(restartCtx) }()
	t.Cleanup(func() { stopRestart(); <-restartDone })
	publish(t, configstore.NewGonacosStore(restarted.Services().Config), namespace, 2, true)

	deadline := time.After(20 * time.Second)
	for consumer.revision != 2 {
		consumer.refresh(client) // periodic manifest reconciliation after a lost push
		if consumer.revision == 2 {
			break
		}
		if consumer.value("checkout") != false {
			t.Fatal("partial snapshot visible during reconnect")
		}
		select {
		case <-changes: // notification is only an invalidation hint
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatalf("same client did not reconnect: revision=%d error=%v", consumer.revision, consumer.lastErr)
		}
	}
	if consumer.lastErr != nil || consumer.value("checkout") != true {
		t.Fatalf("reconnected state: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
}

// This characterizes the current security gap: gonacos's gRPC config query
// and subscription accept a caller-selected namespace without a tenant
// credential. Until the upstream endpoint enforces ACLs, native clients must
// remain on loopback and cannot be a public multi-tenant GoLab interface.
func TestNativeClientCanReadOtherTenantWithoutAuthorization(t *testing.T) {
	srv := nativeServer(t)
	store := configstore.NewGonacosStore(srv.Services().Config)
	publish(t, store, "tenant-a", 1, true)
	clientB := nativeClient(t, srv, "tenant-b")
	if _, err := clientB.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group}); err == nil {
		t.Fatal("tenant B unexpectedly sees tenant A in its own namespace")
	}
	// No auth is required to construct another native client with tenant A's
	// namespace ID. This MUST stop succeeding before native access is supported.
	impersonated := nativeClient(t, srv, "tenant-a")
	if _, _, err := nativeSnapshot(impersonated); err != nil {
		t.Fatalf("security characterization changed; recheck ACL contract: %v", err)
	}
	if _, err := impersonated.GetConfig(vo.ConfigParam{DataId: configstore.FlagDataID("checkout", 1), Group: configstore.Group}); err != nil {
		t.Fatalf("immutable document query behavior changed; recheck ACL contract: %v", err)
	}
	changes := make(chan int64, 8)
	if err := impersonated.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, content string) {
			var manifest model.Manifest
			if json.Unmarshal([]byte(content), &manifest) == nil {
				select {
				case changes <- manifest.Revision:
				default:
				}
			}
		}}); err != nil {
		t.Fatal(err)
	}
	publish(t, store, "tenant-a", 2, false)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case revision := <-changes:
			if revision == 2 {
				return
			}
		case <-deadline:
			t.Fatal("unauthorized native subscription did not observe tenant A update; recheck ACL contract")
		}
	}
}
