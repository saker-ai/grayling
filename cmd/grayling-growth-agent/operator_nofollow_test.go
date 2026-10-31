package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

type operatorFileCase struct {
	name       string
	command    string
	flag       string
	path       string
	extra      []string
	read       func(string) error
	diagnostic string
}

// Each artifact is valid for its command so a no-follow regression cannot hide
// behind JSON, fingerprint, scope or semantic validation failures.
func operatorFileCases(t *testing.T) []operatorFileCase {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+"-private-content.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	p := fixtureProposal(t)
	proposal := write("proposal", p)
	fp, err := growth.ProposalFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	m := growth.MonitorManifest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: "exp1", Epoch: 1, RegistrationVersion: p.PrimaryMetric.Ref.DefinitionHash, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []growth.MetricRef{p.Guardrails[0].Ref}}
	monitor := write("monitor", m)
	rollout := write("rollout", growth.RolloutManifest{WorkspaceID: m.WorkspaceID, EnvironmentID: m.EnvironmentID, ExperimentID: m.ExperimentID, Epoch: m.Epoch, RegistrationVersion: m.RegistrationVersion})
	review := write("review", growth.ReviewInputs{ExpectedProposalFingerprint: fp, Decision: growth.ReviewRequestChanges, Reviewer: "operator", ReviewedAt: p.AnalysisPlan.Provenance.SuppliedAt, Rationale: secret})
	learning := write("learning", growth.LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: p.AnalysisPlan.Provenance.SuppliedAt, Followups: []string{secret}})
	return []operatorFileCase{
		{"prepare settings", "prepare", "--settings", writeSettings(t, p), []string{"--proposal", proposal}, func(path string) error { _, err := readSettings(path); return err }, "invalid preparation settings file or JSON\n"},
		{"record-review inputs", "record-review", "--review-input", review, []string{"--proposal", proposal}, func(path string) error { _, err := readReviewInputs(path); return err }, "invalid review input file or JSON\n"},
		{"record-learning inputs", "record-learning", "--learning-input", learning, []string{"--proposal", proposal, "--manifest", monitor}, func(path string) error { _, err := readLearningInputs(path); return err }, "invalid learning input file or JSON\n"},
		{"record-learning manifest", "record-learning", "--manifest", monitor, []string{"--proposal", proposal, "--learning-input", learning}, func(path string) error { _, err := readMonitorManifest(path); return err }, "invalid monitor manifest\n"},
		{"monitor manifest", "monitor", "--manifest", monitor, nil, func(path string) error { _, err := readMonitorManifest(path); return err }, "invalid monitor manifest\n"},
		{"rollout manifest", "rollout", "--manifest", rollout, nil, func(path string) error { _, err := readRolloutManifest(path); return err }, "invalid rollout manifest\n"},
		{"bind-check manifest", "bind-check", "--manifest", rollout, nil, func(path string) error { _, err := readRolloutManifest(path); return err }, "invalid bind-check manifest\n"},
	}
}

var operatorPathKinds = []string{"regular", "ancestor symlink", "valid JSON symlink", "dangling symlink", "symlink loop", "fifo", "directory"}

func operatorPath(t *testing.T, target, kind string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "private-path")
	var err error
	switch kind {
	case "regular":
		return target
	case "ancestor symlink":
		// O_NOFOLLOW applies only to the last component. Preserve this boundary.
		err = os.Symlink(filepath.Dir(target), path)
		path = filepath.Join(path, filepath.Base(target))
	case "valid JSON symlink":
		err = os.Symlink(target, path)
	case "dangling symlink":
		err = os.Symlink(filepath.Join(dir, "missing-private-target"), path)
	case "symlink loop":
		other := filepath.Join(dir, "private-loop")
		if err = os.Symlink(path, other); err == nil {
			err = os.Symlink(other, path)
		}
	case "fifo":
		err = syscall.Mkfifo(path, 0600)
	case "directory":
		err = os.Mkdir(path, 0700)
	default:
		t.Fatalf("unknown path kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorReadersNoFollow(t *testing.T) {
	for _, tc := range operatorFileCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range operatorPathKinds {
				t.Run(kind, func(t *testing.T) {
					err := tc.read(operatorPath(t, tc.path, kind))
					allowed := kind == "regular" || kind == "ancestor symlink"
					if allowed && err != nil {
						t.Fatalf("valid regular file rejected: %v", err)
					}
					if !allowed && err == nil {
						t.Fatal("nonregular input accepted")
					}
				})
			}
		})
	}
}

func TestOperatorInputsNoFollowBinary(t *testing.T) {
	// Build once for the entire command/path table, including both shared readers.
	binary := filepath.Join(t.TempDir(), "grayling-growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, data)
	}
	for _, tc := range operatorFileCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range operatorPathKinds {
				t.Run(kind, func(t *testing.T) {
					var calls atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						http.Error(w, secret, http.StatusForbidden)
					}))
					defer srv.Close()
					path := operatorPath(t, tc.path, kind)
					args := []string{tc.command, tc.flag, path, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1"}
					args = append(args, tc.extra...)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, binary, args...)
					cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN="+secret)
					var out, diag bytes.Buffer
					cmd.Stdout, cmd.Stderr = &out, &diag
					err := cmd.Run()
					if ctx.Err() != nil {
						t.Fatal("input processing blocked")
					}
					wantCode, wantDiag := 2, tc.diagnostic
					allowed := kind == "regular" || kind == "ancestor symlink"
					if allowed {
						wantCode, wantDiag = 1, "evidence access denied\n"
					}
					exit, ok := err.(*exec.ExitError)
					if !ok || exit.ExitCode() != wantCode {
						t.Fatalf("exit: %v, want %d; diagnostics: %q", err, wantCode, diag.String())
					}
					// Exact fixed diagnostics also enforce the byte bound and absence of
					// paths, artifact contents, server prose and bearer-token leakage.
					if out.Len() != 0 || diag.String() != wantDiag {
						t.Fatalf("stdout=%q diagnostics=%q", out.String(), diag.String())
					}
					if allowed && calls.Load() == 0 {
						t.Fatal("valid regular-file control did not reach HTTP")
					}
					if !allowed && calls.Load() != 0 {
						t.Fatalf("rejected input made %d HTTP requests", calls.Load())
					}
				})
			}
		})
	}
}
