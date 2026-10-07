package cluster

import (
	"crypto/tls"
	"crypto/x509"
	json "encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/test/pgtest"
	"github.com/godeps/golab/test/redistest"
	grpcsrv "github.com/godeps/gonacos/pkg/protocol/grpc"
	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
	"golang.org/x/net/http2"
)

func replicaOption(t *testing.T, r *replica, name string) string {
	t.Helper()
	for i := 0; i+1 < len(r.args); i += 2 {
		if r.args[i] == name {
			return r.args[i+1]
		}
	}
	t.Fatalf("replica %s has no %s option", r.name, name)
	return ""
}

func nacosServerConfig(t *testing.T, r *replica) constant.ServerConfig {
	t.Helper()
	_, httpPort, err := net.SplitHostPort(replicaOption(t, r, "-gonacos-addr"))
	if err != nil {
		t.Fatal(err)
	}
	_, grpcPort, err := net.SplitHostPort(replicaOption(t, r, "-gonacos-grpc-addr"))
	if err != nil {
		t.Fatal(err)
	}
	hp, err := strconv.Atoi(httpPort)
	if err != nil {
		t.Fatal(err)
	}
	gp, err := strconv.Atoi(grpcPort)
	if err != nil {
		t.Fatal(err)
	}
	return *constant.NewServerConfig("127.0.0.1", uint64(hp), constant.WithGrpcPort(uint64(gp)))
}

func nativeClusterClient(t *testing.T, namespace, sdkToken string, servers ...constant.ServerConfig) config_client.IConfigClient {
	return nativeClusterClientWithCA(t, namespace, sdkToken, "", servers...)
}

func nativeClusterClientWithCA(t *testing.T, namespace, sdkToken, caFile string, servers ...constant.ServerConfig) config_client.IConfigClient {
	t.Helper()
	opts := []constant.ClientOption{
		constant.WithNamespaceId(namespace), constant.WithTimeoutMs(3000),
		constant.WithUsername("golab-sdk"), constant.WithPassword(sdkToken),
		constant.WithNotLoadCacheAtStart(true), constant.WithCacheDir(t.TempDir()), constant.WithLogDir(t.TempDir()),
	}
	if caFile != "" {
		opts = append(opts, constant.WithTLS(*constant.NewTLSConfig(constant.WithCA(caFile, ""))))
	}
	c, err := clients.NewConfigClient(vo.NacosClientParam{
		ClientConfig:  constant.NewClientConfig(opts...),
		ServerConfigs: servers,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseClient)
	return c
}

func nativeClusterRevision(c config_client.IConfigClient) (int64, error) {
	content, err := c.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group})
	if err != nil {
		return 0, err
	}
	var manifest model.Manifest
	if err := json.Unmarshal([]byte(content), &manifest); err != nil {
		return 0, err
	}
	if len(manifest.Flags) != 1 {
		return 0, fmt.Errorf("manifest has %d flags, want 1", len(manifest.Flags))
	}
	entry := manifest.Flags[0]
	doc, err := c.GetConfig(vo.ConfigParam{DataId: entry.DataID, Group: configstore.Group})
	if err != nil {
		return 0, err
	}
	if model.HashBytes([]byte(doc)) != entry.Hash {
		return 0, fmt.Errorf("document %s hash mismatch", entry.DataID)
	}
	var flag model.Flag
	if err := json.Unmarshal([]byte(doc), &flag); err != nil {
		return 0, err
	}
	if flag.Key != entry.Key {
		return 0, fmt.Errorf("document key %q, manifest key %q", flag.Key, entry.Key)
	}
	return manifest.Revision, nil
}

// A native client starts while B is offline, so its initial connection must
// use A. After B catches up and A exits, the same client's original server
// list must reach B and read a complete newly published snapshot.
func TestOfficialNacosClientFailsOverAcrossGolabNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires two GoLab processes, PostgreSQL, and Redis")
	}
	dsn := pgtest.DSN(t)
	redis := redistest.Start(t)
	bin := buildGolab(t, repoRoot(t))
	const authSecret = "native-failover-shared-secret-0123456789abcdef"
	a := startReplica(t, "native-A", bin, dsn, redis.Addr, authSecret, time.Second, "-nacos-native-auth=true")
	owner := scrapeOwnerToken(t, a.proc, 20*time.Second)
	adminA := &client{t: t, base: a.base, token: owner}
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces",
		map[string]any{"slug": "acme", "name": "Acme"}, nil); code != http.StatusCreated {
		t.Fatalf("create workspace: HTTP %d", code)
	}
	var environment struct{ NamespaceID string }
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces/acme/environments",
		map[string]any{"slug": "prod", "name": "Production"}, &environment); code != http.StatusCreated {
		t.Fatalf("create environment: HTTP %d", code)
	}
	if environment.NamespaceID == "" {
		t.Fatal("environment has no gonacos namespace ID")
	}
	var reviewer struct{ Secret string }
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "reviewer", "kind": "admin", "role": "publisher"}, &reviewer); code != http.StatusCreated {
		t.Fatalf("create reviewer token: HTTP %d", code)
	}
	adminA.reviewer = reviewer.Secret
	r1, code := adminA.publishFlag(splitFlag("feature", 50))
	if code != http.StatusOK {
		t.Fatalf("publish r1: HTTP %d", code)
	}
	var sdkToken struct {
		Secret string
		Token  struct{ ID string }
	}
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "sdk", "kind": "sdk", "environment": "prod"}, &sdkToken); code != http.StatusCreated {
		t.Fatalf("create SDK token: HTTP %d", code)
	}
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces",
		map[string]any{"slug": "other", "name": "Other"}, nil); code != http.StatusCreated {
		t.Fatalf("create other workspace: HTTP %d", code)
	}
	var otherEnvironment struct{ NamespaceID string }
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces/other/environments",
		map[string]any{"slug": "prod", "name": "Production"}, &otherEnvironment); code != http.StatusCreated {
		t.Fatalf("create other environment: HTTP %d", code)
	}
	if otherEnvironment.NamespaceID == "" {
		t.Fatal("other environment has no gonacos namespace ID")
	}
	var otherReviewer struct{ Secret string }
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces/other/tokens",
		map[string]any{"name": "other-reviewer", "kind": "admin", "role": "publisher"}, &otherReviewer); code != http.StatusCreated {
		t.Fatalf("create other reviewer token: HTTP %d", code)
	}
	var otherToken struct{ Secret string }
	if code := adminA.do(http.MethodPost, "/api/v1/admin/workspaces/other/tokens",
		map[string]any{"name": "other-sdk", "kind": "sdk", "environment": "prod"}, &otherToken); code != http.StatusCreated {
		t.Fatalf("create other SDK token: HTTP %d", code)
	}
	otherFlag := splitFlag("other-feature", 25)
	otherPath := "/api/v1/admin/workspaces/other/environments/prod/flags/" + otherFlag.Key
	for _, step := range []struct {
		method, path string
		body         map[string]any
		token        string
	}{
		{http.MethodPut, otherPath + "/draft", map[string]any{"content": otherFlag, "risk": "normal"}, owner},
		{http.MethodPost, otherPath + "/submit", map[string]any{"reason": "native tenant isolation"}, owner},
		{http.MethodPost, otherPath + "/approve", map[string]any{"note": "ok"}, otherReviewer.Secret},
		{http.MethodPost, otherPath + "/publish", map[string]any{"note": "ship"}, owner},
	} {
		actor := &client{t: t, base: a.base, token: step.token}
		if code := actor.do(step.method, step.path, step.body, nil); code != http.StatusOK {
			t.Fatalf("publish other tenant %s: HTTP %d", step.path, code)
		}
	}

	b := startReplica(t, "native-B", bin, dsn, redis.Addr, authSecret, time.Second, "-nacos-native-auth=true")
	cert, caFile := tlsL4Fixture(t)
	server := func(node *replica) constant.ServerConfig {
		httpAddr := startNativeTLSL4(t, replicaOption(t, node, "-gonacos-addr"), cert, false)
		grpcAddr := startNativeTLSL4(t, replicaOption(t, node, "-gonacos-grpc-addr"), cert, true)
		_, hp, _ := net.SplitHostPort(httpAddr)
		_, gp, _ := net.SplitHostPort(grpcAddr)
		httpPort, _ := strconv.Atoi(hp)
		grpcPort, _ := strconv.Atoi(gp)
		return *constant.NewServerConfig("127.0.0.1", uint64(httpPort), constant.WithScheme("https"), constant.WithGrpcPort(uint64(grpcPort)))
	}
	serverA, serverB := server(a), server(b)
	b.proc.terminate(t)
	// The SDK's next-server rotation can choose either address first. With B
	// offline, an initial successful query proves it connected through A.
	native := nativeClusterClientWithCA(t, environment.NamespaceID, sdkToken.Secret, caFile, serverB, serverA)
	initial, err := nativeClusterRevision(native)
	if err != nil || initial != r1 {
		t.Fatalf("initial native revision=%d want=%d error=%v", initial, r1, err)
	}
	changes := make(chan int64, 8)
	if err := native.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, content string) {
			var manifest model.Manifest
			if json.Unmarshal([]byte(content), &manifest) != nil {
				return
			}
			select {
			case changes <- manifest.Revision:
			default:
			}
		}}); err != nil {
		t.Fatal(err)
	}
	b.proc = startProc(t, "golab-native-B(restarted)", b.bin, b.args...)
	waitHealth(t, b.base, 60*time.Second)
	sdkA := &client{t: t, base: a.base, token: sdkToken.Secret}
	sdkB := &client{t: t, base: b.base, token: sdkToken.Secret}
	waitRevision(t, r1, 15*time.Second, sdkA, sdkB)
	otherNative := nativeClusterClientWithCA(t, otherEnvironment.NamespaceID, otherToken.Secret, caFile, serverA)
	if revision, err := nativeClusterRevision(otherNative); err != nil || revision == 0 {
		t.Fatalf("other tenant cannot read its own manifest: revision=%d error=%v", revision, err)
	}
	checkNativeSharedBackendConnection(t, replicaOption(t, a, "-gonacos-grpc-addr"),
		environment.NamespaceID, otherEnvironment.NamespaceID, sdkToken.Secret, otherToken.Secret, cert, caFile)
	foreignPushes := make(chan string, 2)
	for _, node := range []struct {
		name   string
		server constant.ServerConfig
	}{{a.name, serverA}, {b.name, serverB}} {
		other := nativeClusterClientWithCA(t, environment.NamespaceID, otherToken.Secret, caFile, node.server)
		if _, err := other.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group}); err == nil {
			t.Fatalf("other tenant read acme manifest through node %s", node.name)
		}
		_ = other.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
			OnChange: func(_, _, _, _ string) { foreignPushes <- node.name }})
	}
	r2, code := adminA.publishFlag(splitFlag("feature", 60))
	if code != http.StatusOK {
		t.Fatalf("publish r2: HTTP %d", code)
	}
	waitRevision(t, r2, 15*time.Second, sdkA, sdkB)
	pushDeadline := time.After(10 * time.Second)
waitPush:
	for {
		select {
		case revision := <-changes:
			if revision == r2 {
				break waitPush
			}
		case <-pushDeadline:
			t.Fatal("authorized TLS subscription did not receive r2 manifest push")
		}
	}
	select {
	case node := <-foreignPushes:
		t.Fatalf("other tenant received acme push through %s", node)
	case <-time.After(time.Second):
	}
	a.proc.terminate(t)
	adminB := &client{t: t, base: b.base, token: owner, reviewer: reviewer.Secret}
	r3, code := adminB.publishFlag(splitFlag("feature", 70))
	if code != http.StatusOK {
		t.Fatalf("publish r3 on surviving node: HTTP %d", code)
	}

	deadline := time.Now().Add(45 * time.Second)
	lastVerified := initial
	var lastErr error
	for time.Now().Before(deadline) {
		revision, err := nativeClusterRevision(native)
		if err == nil && revision > lastVerified {
			lastVerified = revision
		}
		lastErr = err
		if lastVerified == r3 {
			break
		}
		select {
		case <-changes: // only an invalidation hint; always re-read the manifest
		case <-time.After(200 * time.Millisecond):
		}
	}
	if lastVerified != r3 {
		t.Fatalf("native client did not fail over to B: revision=%d want=%d last error=%v", lastVerified, r3, lastErr)
	}
	// The TLS HTTP listener must expose only login; it cannot bypass gRPC
	// authorization, including for callers with no credential at all.
	httpAddr := net.JoinHostPort(serverB.IpAddr, strconv.FormatUint(serverB.Port, 10))
	pool := x509.NewCertPool()
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("invalid test CA")
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	baseURL := "https://" + httpAddr
	resp, err := httpClient.Get(baseURL + "/nacos/v1/cs/configs?tenant=" + environment.NamespaceID + "&group=GOLAB_FLAGS&dataId=golab.manifest")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HTTP config bypass: HTTP %d, want 404", resp.StatusCode)
	}
	resp, err = httpClient.PostForm(baseURL+"/nacos/v1/auth/users/login", url.Values{
		"username": {"golab-sdk"}, "password": {"invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized TLS login: HTTP %d, want 401", resp.StatusCode)
	}
	resp, err = httpClient.PostForm(baseURL+"/nacos/v1/auth/users/login", url.Values{
		"username": {"golab-sdk"}, "password": {sdkToken.Secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized TLS login: HTTP %d, want 200", resp.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet,
		baseURL+"/nacos/v1/cs/configs?tenant="+environment.NamespaceID+"&group=GOLAB_FLAGS&dataId=golab.manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("accessToken", sdkToken.Secret)
	resp, err = httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("authenticated HTTP config bypass: HTTP %d, want 404", resp.StatusCode)
	}
	var rawConnections atomic.Int64
	grpcAddr := startNativeTLSL4Count(t, replicaOption(t, b, "-gonacos-grpc-addr"), cert, true, &rawConnections)
	probeAddr := startNativeTLSL4(t, replicaOption(t, b, "-gonacos-grpc-addr"), cert, true)
	probeTokens := []string{sdkToken.Secret, otherToken.Secret}
	probeFiles := make([]string, len(probeTokens))
	for i, secret := range probeTokens {
		probeFiles[i] = filepath.Join(t.TempDir(), fmt.Sprintf("tenant-%d-token", i))
		if err := os.WriteFile(probeFiles[i], []byte(secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	probeCmd := exec.CommandContext(t.Context(), "go", "run", "./cmd/nacos-native-grpc-probe",
		"-address", probeAddr, "-server-name", "127.0.0.1", "-ca-file", caFile,
		"-tenant-a-namespace", environment.NamespaceID, "-tenant-b-namespace", otherEnvironment.NamespaceID,
		"-tenant-a-token-file", probeFiles[0], "-tenant-b-token-file", probeFiles[1])
	probeCmd.Dir = repoRoot(t)
	probeCmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := probeCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; cross-tenant reads and subscriptions denied") {
		t.Fatalf("two-tenant native deployment probe failed: %v\n%s", err, out)
	}
	grpcTransport := &http2.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(grpcTransport.CloseIdleConnections)
	grpcClient := &http.Client{Transport: grpcTransport}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, environment.NamespaceID, sdkToken.Secret); status != grpcsrv.StatusOK {
		t.Fatalf("authorized direct TLS gRPC read: status %d, want 0", status)
	}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, otherEnvironment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusOK {
		t.Fatalf("other tenant direct TLS gRPC read: status %d, want 0", status)
	}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, environment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("cross-tenant direct TLS gRPC read: status %d, want 7", status)
	}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, environment.NamespaceID, ""); status != grpcsrv.StatusUnauthenticated {
		t.Fatalf("anonymous direct TLS gRPC read: status %d, want 16", status)
	}
	if status := nativeRawTLSListenStatus(t, grpcClient, grpcAddr, environment.NamespaceID, sdkToken.Secret); status != grpcsrv.StatusOK {
		t.Fatalf("authorized direct TLS gRPC subscription: status %d, want 0", status)
	}
	if status := nativeRawTLSListenStatus(t, grpcClient, grpcAddr, environment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("cross-tenant direct TLS gRPC subscription: status %d, want 7", status)
	}
	if code := adminB.do(http.MethodDelete, "/api/v1/admin/tokens/"+sdkToken.Token.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("revoke SDK token: HTTP %d", code)
	}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, environment.NamespaceID, sdkToken.Secret); status != grpcsrv.StatusUnauthenticated {
		t.Fatalf("revoked direct TLS gRPC read on existing connection: status %d, want 16", status)
	}
	if status := nativeRawTLSListenStatus(t, grpcClient, grpcAddr, environment.NamespaceID, sdkToken.Secret); status != grpcsrv.StatusUnauthenticated {
		t.Fatalf("revoked direct TLS gRPC subscription on existing connection: status %d, want 16", status)
	}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, otherEnvironment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusOK {
		t.Fatalf("other tenant read after revocation on existing connection: status %d, want 0", status)
	}
	if status := nativeRawTLSListenStatus(t, grpcClient, grpcAddr, otherEnvironment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("other tenant subscription on revoked tenant's connection: status %d, want 7", status)
	}
	if status := nativeRawTLSQueryStatus(t, grpcClient, grpcAddr, environment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("cross-tenant read after revocation on existing connection: status %d, want 7", status)
	}
	if status := nativeRawTLSListenStatus(t, grpcClient, grpcAddr, environment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("cross-tenant subscription after revocation on existing connection: status %d, want 7", status)
	}
	if count := rawConnections.Load(); count != 1 {
		t.Fatalf("direct reads used %d TLS connections, want one persistent connection", count)
	}
	otherTransport := &http2.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(otherTransport.CloseIdleConnections)
	otherGRPCClient := &http.Client{Transport: otherTransport}
	if status := nativeRawTLSListenStatus(t, otherGRPCClient, grpcAddr, otherEnvironment.NamespaceID, otherToken.Secret); status != grpcsrv.StatusOK {
		t.Fatalf("other tenant subscription on separate connection after revocation: status %d, want 0", status)
	}
	if count := rawConnections.Load(); count != 2 {
		t.Fatalf("separate tenant subscription used %d TLS connections, want two", count)
	}
	resp, err = httpClient.PostForm(baseURL+"/nacos/v1/auth/users/login", url.Values{
		"username": {"golab-sdk"}, "password": {sdkToken.Secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked SDK credential login through B: HTTP %d, want 401", resp.StatusCode)
	}
	r4, code := adminB.publishFlag(splitFlag("feature", 80))
	if code != http.StatusOK {
		t.Fatalf("publish after revoke: HTTP %d", code)
	}
	quiet := time.After(time.Second)
	for {
		select {
		case revision := <-changes:
			if revision == r4 {
				t.Fatal("revoked SDK credential received r4 manifest push")
			}
		case <-quiet:
			return
		}
	}
}
