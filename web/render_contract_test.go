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

func TestFlagPublishReviewContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; Flag publish review contract requires node")
	}
	cmd := exec.Command(node, "flag_publish_contract.mjs")
	cmd.Dir = wd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Flag publish review contract failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// TestRenderRolloutDOMContract runs the Node-based DOM/contract test for the
// staged-rollout (release plan) UI of the experiment-detail view (§5.3 P4
// 自动推进). It loads the same real web/app.js in a vm and drives
// renderRollout()/bindRollout() with mock plans covering every plan state
// (no plan / armed / running / manual takeover / paused — manual and
// SRM-blocked / rolled_back / completed), asserting the key DOM nodes and
// button IDs per state, that every bound ID exists in the rendered markup
// (bindRollout runs against a root that answers only rendered IDs), and — via
// click simulation with recording confirm/prompt/fetch stubs — the two-step
// confirmation of danger actions plus the exact path/body of pause, resume,
// advance and takeover requests. Same node gating as the report contract.
func TestRenderRolloutDOMContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; console DOM/contract test requires node (same external-tool gating as the PG/docker tests)")
	}
	script := filepath.Join("rollout_contract.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("contract script missing: %v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = wd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rollout UI DOM contract failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// TestI18NDOMContract runs the Node-based DOM/contract test for the console
// i18n layer (web/i18n.js + web/app.js). It loads the REAL sources in a vm and
// asserts: the language resolution order (localStorage golab_lang >
// navigator.language zh*→zh > en), every key view (flags list/detail, segments
// list/detail, experiments list/create/detail, report, rollout block, tokens,
// audit, ai, debug) rendered in BOTH en and zh with sampled strings hitting
// each language and identical layout node IDs, the top-bar language switch
// under click simulation (persist + in-place re-render, no reload), dictionary
// completeness (zero t() misses collected during all renders, en/zh catalog
// key parity, every statically extracted t('key')/data-i18n key present) and a
// static scan proving app.js/index.html carry no CJK literals outside the
// catalogs (comments stripped as the whitelisted exception). Same node gating
// as the other console contract tests.
func TestI18NDOMContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; console DOM/contract test requires node (same external-tool gating as the PG/docker tests)")
	}
	script := filepath.Join("i18n_contract.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("contract script missing: %v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = wd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("i18n DOM contract failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func TestCapabilityDOMContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; capability DOM contract requires node")
	}
	cmd := exec.Command(node, "capability_contract.mjs")
	cmd.Dir = wd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capability DOM contract failed: %v\n%s", err, out)
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
