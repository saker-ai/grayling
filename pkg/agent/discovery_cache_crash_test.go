package agent_test

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	client "github.com/saker-ai/grayling/pkg/agent"
)

func TestDiscoveryCacheCrashHelper(t *testing.T) {
	directory := os.Getenv("GRAYLING_TEST_DISCOVERY_CRASH_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	f, o := discoveryCacheFixture(t)
	o.Directory = directory
	cache, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 100, 100, false)); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // intentionally bypass Close and SQLite checkpoint cleanup
}
func TestDiscoveryCacheCommittedWALKeepsMemberFloorAfterProcessExit(t *testing.T) {
	f, o := discoveryCacheFixture(t)
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDiscoveryCacheCrashHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "GRAYLING_TEST_DISCOVERY_CRASH_DIRECTORY="+o.Directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	cache, err := client.OpenDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	view, err := cache.Capture()
	if err != nil || view.Claims().Revision != 100 || view.MembershipRevision() != 100 {
		t.Fatal("process exit tore the discovery baseline", err)
	}
	if err := cache.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 101, 99, false)); !errors.Is(err, client.ErrRollback) {
		t.Fatal("process exit cleared member floor", err)
	}
}
