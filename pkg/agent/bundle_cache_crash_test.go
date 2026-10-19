package agent_test

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	client "github.com/saker-ai/grayling/pkg/agent"
)

func TestBundleCacheCrashHelper(t *testing.T) {
	directory := os.Getenv("GRAYLING_TEST_BUNDLE_CRASH_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	f, o, d, _ := bundleCacheFixture(t)
	o.Directory = directory
	cache, err := client.InitializeBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), cachedBundleResource(t, f, d, 100, "key", f.private)); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // bypass Close and SQLite checkpoint cleanup deliberately
}
func TestBundleCacheCommittedWALKeepsWatermarkAfterProcessExit(t *testing.T) {
	f, o, d, _ := bundleCacheFixture(t)
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBundleCacheCrashHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "GRAYLING_TEST_BUNDLE_CRASH_DIRECTORY="+o.Directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	cache, err := client.OpenBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	view, err := cache.Capture()
	if err != nil || view.Claims().Revision != 100 {
		t.Fatal("process exit tore Bundle/watermark", err)
	}
	if err := cache.Install(t.Context(), cachedBundleResource(t, f, d, 99, "key", f.private)); !errors.Is(err, client.ErrRollback) {
		t.Fatal("process exit cleared Bundle floor", err)
	}
}
