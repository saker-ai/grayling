package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRenderReportDOMContract runs the Node-based DOM/contract test that loads
// the real web/app.js in a vm and asserts renderReport() produces the expected
// DOM for every P5 report field (ratio/quantile/CUPED detail lines, the
// always-valid sequential view and the Bayesian view — each labelled separate
// from the frequentist decision gate, per docs/技术方案.md §10.3).
//
// node is an external toolchain dependency (already used by sdk/typescript). As
// with the PostgreSQL integration tests, which skip when docker is absent, this
// skips with a clear message when node is unavailable — it is NOT a stubbed or
// faked pass: when node is present (as in CI and this repo's dev env) the test
// executes the real renderer and fails on any contract drift.
func TestRenderReportDOMContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; console DOM/contract test requires node (same external-tool gating as the PG/docker tests)")
	}
	script := filepath.Join("render_contract.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("contract script missing: %v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = wd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("renderReport DOM contract failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// wd returns the directory of this test file (the web/ package dir), so the
// contract runs regardless of the caller's working directory.
func wd(t *testing.T) string {
	t.Helper()
	// go test sets the working directory to the package dir already; resolve it
	// explicitly for robustness when invoked from the repo root.
	if dir, err := os.Getwd(); err == nil && fileExists(filepath.Join(dir, "app.js")) {
		return dir
	}
	return "."
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
