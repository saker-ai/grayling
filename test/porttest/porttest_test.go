package porttest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLeaseExcludesOtherProcessAndReleases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leases")
	lock, port, err := acquire(dir, 10000, 30000)
	if err != nil {
		t.Fatal(err)
	}
	// The actual child must be able to bind while the reservation is held.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	listener.Close()
	probe := func(available bool) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLeaseContentionProbe$")
		cmd.Env = append(os.Environ(), "GRAYLING_PORT_PROBE_DIR="+dir, "GRAYLING_PORT_PROBE_PORT="+strconv.Itoa(port), fmt.Sprintf("GRAYLING_PORT_PROBE_AVAILABLE=%t", available))
		if raw, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("cross-process lease: %v %s", e, raw)
		}
	}
	probe(false)
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	probe(true)
}
func TestLeaseContentionProbe(t *testing.T) {
	dir := os.Getenv("GRAYLING_PORT_PROBE_DIR")
	if dir == "" {
		return
	}
	port, err := strconv.Atoi(os.Getenv("GRAYLING_PORT_PROBE_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	lock, actual, err := acquire(dir, port, port+1)
	available := os.Getenv("GRAYLING_PORT_PROBE_AVAILABLE") == "true"
	if (err == nil) != available {
		if lock != nil {
			lock.Close()
		}
		t.Fatal("unexpected cross-process reservation outcome")
	}
	if err == nil {
		defer lock.Close()
		if actual != port {
			t.Fatal("wrong port")
		}
	}
}
func TestFreeExcludesEphemeralListeners(t *testing.T) {
	port := Free(t)
	for range 100 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		actual := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		if actual == port {
			t.Fatal("ephemeral server stole child port")
		}
	}
}
