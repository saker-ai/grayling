// Package redistest provides a REAL Redis server for tests (no mocks, no
// in-process substitutes — gonacos cluster mode requires the real RESP
// protocol, pub/sub and persistence semantics). Resolution order:
//
//  1. GOLAB_TEST_REDIS_ADDR — an existing reachable Redis; lifecycle
//     operations (Pause/Resume/Stop/StartAgain) are unavailable for it and
//     tests needing them skip with an explicit reason.
//  2. Docker — a throwaway redis:7.4-alpine container bound to loopback
//     (image override: GOLAB_TEST_REDIS_IMAGE).
//  3. Neither available — tests skip with an explicit reason.
package redistest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var redisCounter atomic.Int64

// Redis is a running Redis instance available to the test.
type Redis struct {
	Addr string
	// container is the docker container name ("" for externally provided
	// instances — lifecycle operations then skip).
	container string
}

// Start returns a ready Redis and registers teardown.
func Start(t *testing.T) *Redis {
	t.Helper()
	if addr := os.Getenv("GOLAB_TEST_REDIS_ADDR"); addr != "" {
		waitPing(t, addr)
		return &Redis{Addr: addr}
	}
	return dockerRedis(t)
}

// managed reports whether lifecycle operations are available.
func (r *Redis) managed(t *testing.T) bool {
	t.Helper()
	if r.container == "" {
		t.Skip("redistest: GOLAB_TEST_REDIS_ADDR is externally managed — lifecycle operation unavailable")
		return false
	}
	return true
}

// Pause freezes the Redis process (docker pause): TCP connections stay open
// but nothing is served — simulates a network partition where clients block
// rather than get refused.
func (r *Redis) Pause(t *testing.T) {
	t.Helper()
	if !r.managed(t) {
		return
	}
	r.docker(t, "pause", r.container)
}

// Resume un-freezes a paused Redis (docker unpause).
func (r *Redis) Resume(t *testing.T) {
	t.Helper()
	if !r.managed(t) {
		return
	}
	r.docker(t, "unpause", r.container)
	waitPing(t, r.Addr)
}

// Stop hard-stops the Redis process (docker stop): connections are refused —
// simulates a full Redis outage.
func (r *Redis) Stop(t *testing.T) {
	t.Helper()
	if !r.managed(t) {
		return
	}
	r.docker(t, "stop", r.container)
}

// StartAgain restarts a stopped container and waits for readiness.
func (r *Redis) StartAgain(t *testing.T) {
	t.Helper()
	if !r.managed(t) {
		return
	}
	r.docker(t, "start", r.container)
	waitPing(t, r.Addr)
}

func (r *Redis) docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("redistest: docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// Set writes one string key (raw RESP over TCP — no client dependency).
func (r *Redis) Set(t *testing.T, key, value string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", r.Addr, 5*time.Second)
	if err != nil {
		t.Fatalf("redistest: dial %s: %v", r.Addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	cmd := fmt.Sprintf("*3\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
		len(key), key, len(value), value)
	if _, err := conn.Write([]byte(cmd)); err != nil {
		t.Fatalf("redistest: SET %s: %v", key, err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("redistest: SET reply %s: %v", key, err)
	}
	if !strings.HasPrefix(reply, "+OK") {
		t.Fatalf("redistest: SET %s → %q", key, reply)
	}
}

// KeysCount returns DBSIZE (evidence logging for KV writes).
func (r *Redis) KeysCount(t *testing.T) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", r.Addr, 5*time.Second)
	if err != nil {
		t.Fatalf("redistest: dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("*1\r\n$6\r\nDBSIZE\r\n")); err != nil {
		t.Fatalf("redistest: DBSIZE: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("redistest: DBSIZE reply: %v", err)
	}
	return strings.TrimSpace(reply)
}

func dockerRedis(t *testing.T) *Redis {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("redistest: no docker and no GOLAB_TEST_REDIS_ADDR — real-Redis tests skipped: %v", err)
	}
	probe := exec.Command("docker", "info")
	probe.Stdout, probe.Stderr = nil, nil
	if err := probe.Run(); err != nil {
		t.Skipf("redistest: docker daemon unavailable and no GOLAB_TEST_REDIS_ADDR — real-Redis tests skipped: %v", err)
	}
	image := os.Getenv("GOLAB_TEST_REDIS_IMAGE")
	if image == "" {
		image = "redis:7.4-alpine"
	}
	port := freePort(t)
	name := fmt.Sprintf("golab-redis-%d-%d", os.Getpid(), redisCounter.Add(1))
	// Give each test its own writable persistence directory. Docker's overlay
	// store may be on a different (or full) filesystem from the test workspace.
	dataDir := t.TempDir()
	run := exec.Command("docker", "run", "-d",
		"--name", name,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", dataDir+":/data",
		"-p", fmt.Sprintf("127.0.0.1:%d:6379", port),
		image)
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("redistest: cannot run %s (docker pull/start failed) and no GOLAB_TEST_REDIS_ADDR — real-Redis tests skipped: %v\n%s", image, err, out)
	}
	t.Cleanup(func() {
		c := exec.Command("docker", "rm", "-f", name)
		_, _ = c.CombinedOutput()
	})
	r := &Redis{Addr: fmt.Sprintf("127.0.0.1:%d", port), container: name}
	waitPing(t, r.Addr)
	return r
}

// waitPing blocks until Redis answers the RESP PING with +PONG.
func waitPing(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ping(addr); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("redistest: redis at %s never became ready: %v", addr, lastErr)
}

func ping(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(reply, "+") {
		return fmt.Errorf("PING → %q", reply)
	}
	return nil
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("redistest: free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
