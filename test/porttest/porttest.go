// Package porttest leases loopback ports for child-process test fixtures.
// Kernel-assigned :0 listeners remain preferable for in-process servers.
package porttest

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Free returns a currently available unprivileged TCP port outside Linux's
// ephemeral range. A file lock reserves it against other cooperating fixture
// processes through test cleanup, including child shutdown/restart. Keeping it
// outside the ephemeral range prevents httptest/:0 listeners stealing the port
// between allocation and the child's listen, or receiving its health probes.
func Free(t testing.TB) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Fatal("read fixture ephemeral range:", err)
	}
	var low, high int
	if n, err := fmt.Sscanf(string(raw), "%d %d", &low, &high); err != nil || n != 2 || low < 1 || high < low || high > 65535 {
		t.Fatal("invalid fixture ephemeral range")
	}
	first, last := 10000, min(low, 30000)
	if last-first < 1024 {
		first, last = max(high+1, 10000), 65536
	}
	if last-first < 1024 {
		t.Fatal("no unprivileged non-ephemeral fixture port range")
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("grayling-port-leases-%d", os.Getuid()))
	lock, port, err := acquire(dir, first, last)
	if err != nil {
		t.Fatal("reserve child fixture port:", err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error("release fixture port:", err)
		}
	})
	return port
}

func acquire(dir string, first, last int) (*os.File, int, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, 0, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return nil, 0, fmt.Errorf("unsafe fixture lease directory")
	}
	start := rand.IntN(last - first)
	for offset := range last - first {
		port := first + (start+offset)%(last-first)
		lock, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%d.lock", port)), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return nil, 0, err
		}
		info, err = lock.Stat()
		if err != nil || !info.Mode().IsRegular() {
			lock.Close()
			return nil, 0, fmt.Errorf("invalid fixture lease file")
		}
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			lock.Close()
			continue
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			lock.Close()
			continue
		}
		if err = listener.Close(); err != nil {
			lock.Close()
			return nil, 0, err
		}
		return lock, port, nil
	}
	return nil, 0, fmt.Errorf("fixture port range unavailable")
}
