package agent_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/saker-ai/grayling/pkg/agent"
)

// The child exits without Close or deferred cleanup. This tests committed WAL
// recovery after process death, not storage-controller durability on power loss.
func TestPolicyCacheCrashHelper(t *testing.T) {
	directory := os.Getenv("GRAYLING_TEST_CACHE_CRASH_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	f := newCacheFixture(t)
	f.opts.Directory = directory
	private := recoveryRoots(t, f)
	cache, err := agent.InitializePolicyCache(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), f.resource(t, 10, "before-crash")); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GRAYLING_TEST_CACHE_CRASH_RECOVERY") == "1" {
		from, err := cache.Watermark(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		target := recoveryTarget(t, f, 2, 1, "after-recovery")
		if err := cache.InstallRecovery(t.Context(), target, recoveryGrant(t, f, from, target, private)); err != nil {
			t.Fatal(err)
		}
	}
	os.Exit(0)
}
func TestPolicyCacheCommittedWALSurvivesProcessExit(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "policy", true: "recovery"}[recovery], func(t *testing.T) {
			f := newCacheFixture(t)
			recoveryRoots(t, f)
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPolicyCacheCrashHelper$", "-test.count=1")
			command.Env = append(os.Environ(), "GRAYLING_TEST_CACHE_CRASH_DIRECTORY="+f.opts.Directory)
			if recovery {
				command.Env = append(command.Env, "GRAYLING_TEST_CACHE_CRASH_RECOVERY=1")
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("child: %v %s", err, output)
			}
			cache, err := agent.OpenPolicyCache(t.Context(), f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			view, err := cache.Capture()
			if err != nil {
				t.Fatal(err)
			}
			wantEpoch, wantRevision := uint64(1), uint64(10)
			if recovery {
				wantEpoch, wantRevision = 2, 1
			}
			claims := view.Claims()
			mark, err := cache.Watermark(t.Context())
			if err != nil || claims.ControlPlaneEpoch != wantEpoch || claims.Revision != wantRevision || mark != claims.Watermark() {
				t.Fatalf("crash restored mismatched state: %+v %+v %v", claims, mark, err)
			}
			if err := cache.Install(t.Context(), f.resource(t, 1, "old")); err == nil {
				t.Fatal("crash cleared durable watermark")
			}
		})
	}
}
