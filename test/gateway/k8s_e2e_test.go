//go:build integration && k8s

package gatewaye2e

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gw "github.com/saker-ai/grayling/internal/gateway"
)

const k8sNamespace = "golab-issue12"

func kube(t *testing.T, input []byte, args ...string) string {
	t.Helper()
	ctx := os.Getenv("GOLAB_K8S_CONTEXT")
	if ctx == "" {
		t.Fatal("GOLAB_K8S_CONTEXT must name the disposable cluster")
	}
	cmd := exec.CommandContext(t.Context(), "kubectl", append([]string{"--context", ctx}, args...)...)
	cmd.Stdin = strings.NewReader(string(input))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func command(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func applyRoutes(t *testing.T, spec *gw.RouteSpec) string {
	t.Helper()
	routes, err := gw.IngressRoutes(spec)
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(routes))
	kube(t, routes, "apply", "-f", "-")
	t.Logf("IngressRoute config sha256=%s parent=%s rule=%s", hash, spec.ParentRouter(), spec.Rule)
	return hash
}

func waitRoute(t *testing.T, base, path, key, backend string) *backendReply {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		status, reply, _ := gatewayRequest(t, base, path, key, nil, "GET")
		if status == 200 && (reply.Backend == backend ||
			(backend == "" && (reply.Backend == "service-stable" || reply.Backend == "service-canary"))) {
			return reply
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("route %s did not converge to %s; traefik logs:\n%s", path, backend,
		kube(t, nil, "-n", k8sNamespace, "logs", "deployment/traefik", "--tail=80"))
	return nil
}

func TestKubernetesIngressRouteE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a disposable kind cluster")
	}
	if os.Getenv("GOLAB_K8S_CONTEXT") == "" || os.Getenv("GOLAB_KIND_NAME") == "" {
		t.Skip("run test/gateway/k8s/run.sh for the pinned disposable cluster")
	}
	hostGateway := os.Getenv("GOLAB_K8S_HOST_GATEWAY")
	if hostGateway == "" {
		t.Fatal("GOLAB_K8S_HOST_GATEWAY is required")
	}
	t.Logf("cluster version: %s", kube(t, nil, "version"))
	t.Logf("traefik image: %s", kube(t, nil, "-n", k8sNamespace, "get", "deployment/traefik", "-o", "jsonpath={.spec.template.spec.containers[0].image}"))

	fx := setupFixtureOnHost(t, "0.0.0.0")
	port := strings.TrimPrefix(fx.golabBase, "http://127.0.0.1:")
	upstream := "http://" + net.JoinHostPort(hostGateway, port)

	proxyBin := buildBinary(t, fx.root, "./cmd/golab-proxy", "golab-proxy-k8s")
	imageDir := t.TempDir()
	imageBin := filepath.Join(imageDir, "golab-proxy")
	data, err := os.ReadFile(proxyBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageBin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte("FROM alpine:3.20\nCOPY golab-proxy /golab-proxy\nENTRYPOINT [\"/golab-proxy\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := "golab-proxy:issue12"
	command(t, imageDir, "docker", "build", "-q", "-t", image, ".")
	t.Log(command(t, fx.root, "kind", "load", "docker-image", image, "--name", os.Getenv("GOLAB_KIND_NAME")))

	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: gateway-token\n  namespace: %s\ntype: Opaque\ndata:\n  token: %s\n", k8sNamespace,
		base64.StdEncoding.EncodeToString([]byte(fx.sdkToken)))
	kube(t, []byte(secret), "apply", "-f", "-")
	proxyManifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: proxy, namespace: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: proxy}}
  template:
    metadata: {labels: {app: proxy}}
    spec:
      automountServiceAccountToken: false
      containers:
        - name: proxy
          image: %s
          imagePullPolicy: IfNotPresent
          args: ["-addr=0.0.0.0:8689", "-upstream=$(GOLAB_UPSTREAM)", "-gateway-token=$(GOLAB_TOKEN)", "-gateway-secret=%s", "-gateway-eval-timeout=200ms"]
          env:
            - {name: GOLAB_UPSTREAM, value: %q}
            - name: GOLAB_TOKEN
              valueFrom: {secretKeyRef: {name: gateway-token, key: token}}
          ports: [{name: http, containerPort: 8689}]
          readinessProbe: {httpGet: {path: /healthz, port: http}}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
---
apiVersion: v1
kind: Service
metadata: {name: proxy, namespace: %s}
spec:
  selector: {app: proxy}
  ports: [{name: http, port: 8689, targetPort: http}]
`, k8sNamespace, image, gatewaySecret, upstream, k8sNamespace)
	kube(t, []byte(proxyManifest), "apply", "-f", "-")
	for _, deployment := range []string{"traefik", "stable", "canary", "proxy"} {
		t.Log(kube(t, nil, "-n", k8sNamespace, "rollout", "status", "deployment/"+deployment, "--timeout=120s"))
	}

	entryPort := freePort(t)
	pf := exec.CommandContext(t.Context(), "kubectl", "--context", os.Getenv("GOLAB_K8S_CONTEXT"), "-n", k8sNamespace,
		"port-forward", "service/traefik", fmt.Sprintf("%d:80", entryPort))
	var pfLog strings.Builder
	pf.Stdout, pf.Stderr = &pfLog, &pfLog
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Process.Kill(); _, _ = pf.Process.Wait() })
	entry := fmt.Sprintf("http://127.0.0.1:%d", entryPort)
	for i := range 50 {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(entryPort)), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if i == 49 {
			t.Fatalf("port-forward: %v: %s", err, pfLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	spec := *fx.spec
	spec.Namespace = k8sNamespace
	spec.Rule = "PathPrefix(`/api`)"
	spec.EvalAddress = "http://proxy.golab-issue12.svc.cluster.local:8689"
	for i := range spec.Backends {
		spec.Backends[i].K8sService = "svc-" + spec.Backends[i].Variant
		spec.Backends[i].K8sPort = 80
	}
	initialHash := applyRoutes(t, &spec)
	waitRoute(t, entry, "/api/hello", "probe-user", "")

	// Same assertions as the file/KV test: sticky key, both arms, method,
	// and the evaluated headers win over forged client headers.
	counts := map[string]int{}
	stableKey := ""
	for i := range 100 {
		key := fmt.Sprintf("user-%d", i)
		status, reply, _ := gatewayRequest(t, entry, "/api/orders", key, nil, "POST")
		if status != 200 || reply.Method != "POST" || reply.FlagKeyHeader != flagKey ||
			(reply.Backend == "service-canary") != (reply.VariantHeader == "canary") {
			t.Fatalf("route key=%s status=%d reply=%+v", key, status, reply)
		}
		counts[reply.Backend]++
		if reply.Backend == "service-stable" && stableKey == "" {
			stableKey = key
		}
		status2, reply2, _ := gatewayRequest(t, entry, "/api/orders", key, nil, "POST")
		if status2 != 200 || reply2.Backend != reply.Backend {
			t.Fatalf("non-sticky key=%s: %s then %s", key, reply.Backend, reply2.Backend)
		}
	}
	if counts["service-canary"] < 30 || counts["service-canary"] > 70 || stableKey == "" {
		t.Fatalf("50/50 distribution out of tolerance: %v", counts)
	}
	t.Logf("50/50 distribution: %v", counts)
	status, forged, _ := gatewayRequest(t, entry, "/api/forged", stableKey,
		map[string]string{gw.VariantHeader: "canary", gw.FlagKeyHeader: "fake", gw.FailOpenHeader: "fake", "X-Golab-Forged": "fake"}, "GET")
	if status != 200 || forged.Backend != "service-stable" || forged.VariantHeader != "stable" ||
		forged.FlagKeyHeader != flagKey || forged.FailOpen != "" || forged.ForgedExtra != "" {
		t.Fatalf("forged headers survived: status=%d reply=%+v", status, forged)
	}
	t.Log("forged X-Golab-* headers stripped; stable key remains stable")

	fx.admin.publishFlag(gatewayFlag(0, 100))
	for i := range 30 {
		status, reply, _ := gatewayRequest(t, entry, "/api/orders", fmt.Sprintf("changed-%d", i), nil, "GET")
		if status != 200 || reply.Backend != "service-canary" {
			t.Fatalf("ratio change request %d: status=%d reply=%+v", i, status, reply)
		}
	}
	if got := applyRoutes(t, &spec); got != initialHash {
		t.Fatalf("ratio update changed route config: %s → %s", initialHash, got)
	}
	t.Log("0/100 ratio: 30/30 canary, IngressRoute SHA unchanged")

	// A selected backend with zero endpoints must return an error, not
	// silently substitute the default backend.
	kube(t, nil, "-n", k8sNamespace, "scale", "deployment/canary", "--replicas=0")
	kube(t, nil, "-n", k8sNamespace, "rollout", "status", "deployment/canary", "--timeout=60s")
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, _, _ := gatewayRequest(t, entry, "/api/orders", "no-canary", nil, "GET")
		if status == 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scaled backend returned %d, want 503", status)
		}
		time.Sleep(300 * time.Millisecond)
	}
	kube(t, nil, "-n", k8sNamespace, "scale", "deployment/canary", "--replicas=1")
	kube(t, nil, "-n", k8sNamespace, "rollout", "status", "deployment/canary", "--timeout=90s")
	waitRoute(t, entry, "/api/orders", "restored-canary", "service-canary")
	t.Log("backend scale 1→0→1: 503 while unavailable, recovered after rollout")

	spec.Rule = "PathPrefix(`/v2`)"
	updatedHash := applyRoutes(t, &spec)
	waitRoute(t, entry, "/v2/orders", "rolled", "service-canary")
	status, _, _ = gatewayRequest(t, entry, "/api/orders", "rolled", nil, "GET")
	if status != 404 {
		t.Fatalf("old path after CRD update: %d, want 404", status)
	}
	spec.Rule = "PathPrefix(`/api`)"
	if got := applyRoutes(t, &spec); got != initialHash || got == updatedHash {
		t.Fatalf("CRD rollback hash: got=%s initial=%s updated=%s", got, initialHash, updatedHash)
	}
	waitRoute(t, entry, "/api/orders", "rolled", "service-canary")
	t.Log("IngressRoute CRD update and rollback converged without Traefik restart")

	// The auth process remains available while its GoLab upstream stalls.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	slow := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	slow.Listener = listener
	slow.Start()
	defer slow.Close()
	slowUpstream := "http://" + net.JoinHostPort(hostGateway, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
	kube(t, nil, "-n", k8sNamespace, "set", "env", "deployment/proxy", "GOLAB_UPSTREAM="+slowUpstream)
	kube(t, nil, "-n", k8sNamespace, "rollout", "status", "deployment/proxy", "--timeout=90s")
	var reply *backendReply
	deadline = time.Now().Add(25 * time.Second)
	for {
		status, reply, _ = gatewayRequest(t, entry, "/api/timeout", "timeout-key", nil, "GET")
		if status == 200 && reply.Backend == "service-stable" && reply.FailOpen == "upstream_timeout" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status != 200 || reply.Backend != "service-stable" || reply.FailOpen != "upstream_timeout" {
		t.Fatalf("proxy timeout: status=%d reply=%+v", status, reply)
	}
	kube(t, nil, "-n", k8sNamespace, "set", "env", "deployment/proxy", "GOLAB_UPSTREAM="+upstream)
	kube(t, nil, "-n", k8sNamespace, "rollout", "status", "deployment/proxy", "--timeout=90s")
	waitRoute(t, entry, "/api/orders", "healthy-again", "service-canary")
	t.Log("proxy evaluation timeout: 200 default stable with upstream_timeout")

	fx.golab.stop()
	status, reply, _ = gatewayRequest(t, entry, "/api/outage", "outage-key", nil, "GET")
	if status != 200 || reply.Backend != "service-stable" || reply.FailOpen != "upstream_unreachable" {
		t.Fatalf("GoLab outage: status=%d reply=%+v", status, reply)
	}
	t.Log("GoLab outage: 200 default stable with upstream_unreachable")

	kube(t, nil, "-n", k8sNamespace, "scale", "deployment/proxy", "--replicas=0")
	kube(t, nil, "-n", k8sNamespace, "rollout", "status", "deployment/proxy", "--timeout=60s")
	deadline = time.Now().Add(25 * time.Second)
	for {
		status, reply, _ = gatewayRequest(t, entry, "/api/outage", "outage-key", nil, "GET")
		if status >= 500 && reply.Backend == "" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status < 500 || reply.Backend != "" {
		t.Fatalf("proxy process outage should reject without backend: status=%d reply=%+v", status, reply)
	}
	t.Logf("proxy process outage: gateway status %d, no backend reached", status)
}
