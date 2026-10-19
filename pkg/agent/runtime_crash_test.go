package agent_test

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/saker-ai/grayling/pkg/agent"
)

// Exercises committed SQLite WAL recovery after process death. It does not
// simulate storage-controller power loss or implicitly restore process readiness.
func TestRuntimeCrashHelper(t *testing.T) {
	directory := os.Getenv("GRAYLING_TEST_RUNTIME_CRASH_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	f := newRuntimeFixture(t)
	f.options.Directory = directory
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.generation(t, 2)); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
func TestRuntimeCommittedWALRestoresOneInputSetAndFloors(t *testing.T) {
	f := newRuntimeFixture(t)
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRuntimeCrashHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "GRAYLING_TEST_RUNTIME_CRASH_DIRECTORY="+f.options.Directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	m, err := agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Applied(); !errors.Is(err, agent.ErrRuntimeNotReady) {
		t.Fatal("WAL recovery falsely ready", err)
	}
	mark, err := m.Watermark(t.Context())
	if err != nil || mark.Generation != 2 {
		t.Fatal(mark, err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("process death cleared floor", err)
	}
	report, err := m.Restore(t.Context())
	if err != nil || report.Release.Generation != mark.Generation || report.PolicyRevision != mark.PolicyRevision || report.PreparedRevision != mark.PreparedRevision || report.DiscoveryRevision != mark.DiscoveryRevision {
		t.Fatal("restored mixed inputs", report, mark, err)
	}
	if f.builds.Load() != 1 {
		t.Fatal("restore skipped actual process resources")
	}
}
