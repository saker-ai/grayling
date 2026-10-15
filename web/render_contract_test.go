package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/pkg/model"
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
// asserts: the language resolution order (localStorage grayling_lang >
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

// TestPresetContract runs the Node-based contract for the console's built-in
// gray-release presets (web/presets.js) and then re-validates every document it
// prints with the REAL server validators.
//
// The Node half proves the console side: each preset builds a fresh document for
// the requested key, the console's own lintFlag/lintSegment accept it, every
// preset i18n key resolves in both catalogs, and the pickers are wired (create
// dialog seeds S.flagPreset/S.segPreset, the detail view renders the preset into
// #flag-json/#seg-json, the toolbar picker replaces the source, repaints the
// structured form and clears the select). The Go half closes the loop the
// console cannot: model.ValidateFlag/model.ValidateSegment are a separate code
// path from the browser linter, so a preset that would 400 on save fails here
// instead of in front of an operator, and every time operand is re-read with
// model.ParseTimeOperand with a generated window required to open in the future
// (an elapsed window validates cleanly while quietly pinning every unit to the
// control value). Same node gating as the other console contract tests (skip
// only when node is genuinely unavailable).
func TestPresetContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; preset contract requires node (same external-tool gating as the PG/docker tests)")
	}
	script := filepath.Join("preset_contract.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("contract script missing: %v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = wd(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preset contract failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)

	payload, err := parsePresetPayload(out)
	if err != nil {
		t.Fatalf("preset contract output: %v", err)
	}
	if len(payload.Flags) != 6 || len(payload.Segments) != 5 {
		t.Fatalf("preset catalog changed: got %d flag and %d segment presets, want 6 and 5 — update this test (and docs) with the new catalog",
			len(payload.Flags), len(payload.Segments))
	}
	// Segments first: a flag preset may reference them, and standalone segment
	// validation is what registers their constraint fields.
	now := time.Now()
	timedFlags, timedSegments := 0, 0
	segments := make(map[string]*model.Segment, len(payload.Segments))
	for _, doc := range payload.Segments {
		var segment model.Segment
		decodePresetDocument(t, "segment preset "+doc.ID, doc.Content, &segment)
		if err := model.ValidateSegment(&segment, segments); err != nil {
			t.Errorf("segment preset %q must pass the server validator: %v", doc.ID, err)
		}
		if assertPresetTimeOperands(t, "segment preset "+doc.ID, segment.Constraints, now) {
			timedSegments++
		}
		segments[segment.Key] = &segment
	}
	for _, doc := range payload.Flags {
		var flag model.Flag
		decodePresetDocument(t, "flag preset "+doc.ID, doc.Content, &flag)
		if err := model.ValidateFlag(&flag, segments); err != nil {
			t.Errorf("flag preset %q must pass the server validator: %v", doc.ID, err)
		}
		var constraints []model.Constraint
		for _, rule := range flag.Targeting {
			constraints = append(constraints, rule.Constraints...)
		}
		if assertPresetTimeOperands(t, "flag preset "+doc.ID, constraints, now) {
			timedFlags++
		}
	}
	// Coverage guard: the window presets are the reason these time assertions
	// exist, so a catalog that dropped them must fail here instead of letting
	// the checks above pass over an empty set.
	if timedFlags == 0 || timedSegments == 0 {
		t.Fatalf("time coverage lost: %d flag and %d segment presets carry a time constraint, want at least one of each (the time-window and active-window presets)",
			timedFlags, timedSegments)
	}
}

// assertPresetTimeOperands checks the time constraints of one preset document
// with the server's own operand reader (model.ParseTimeOperand) and, for a
// generated window, that it still lies ahead and stays ordered. It reports
// whether the document carries any time constraint at all.
//
// This is the one check format validation cannot make: an already-elapsed
// window is a perfectly legal pair of RFC3339 strings, so model.ValidateFlag,
// model.ValidateSegment and the console linter all accept it — while every unit
// then falls through to the control value and the preset merely looks broken.
// The console builds its window timestamps at apply time (web/presets.js), so
// these assertions never pin an absolute instant; they pin the two properties
// that keep the preset useful: the window opens in the future and stays ordered.
func assertPresetTimeOperands(t *testing.T, label string, constraints []model.Constraint, now time.Time) bool {
	t.Helper()
	var after, before time.Time
	var hasAfter, hasBefore bool
	for _, constraint := range constraints {
		if constraint.Op != model.OpTimeAfter && constraint.Op != model.OpTimeBefore {
			continue
		}
		instant, ok := model.ParseTimeOperand(constraint.Value)
		if !ok {
			t.Errorf("%s: %s on %s carries an operand the server cannot parse: %v — presets must use RFC3339 strings or epoch seconds",
				label, constraint.Op, constraint.Field, constraint.Value)
			continue
		}
		if constraint.Op == model.OpTimeAfter {
			after, hasAfter = instant, true
		} else {
			before, hasBefore = instant, true
		}
	}
	if !hasAfter && !hasBefore {
		return false
	}
	// A window is a pair: half of it silently widens the rule to "always" (a
	// lone time_after) or "until then" (a lone time_before).
	if hasAfter != hasBefore {
		t.Errorf("%s: a time constraint must come in pairs, got time_after=%t time_before=%t", label, hasAfter, hasBefore)
	}
	if hasAfter && !after.After(now) {
		t.Errorf("%s: the generated window opens at %s, already in the past — an expired preset pins every unit to the control value",
			label, after.UTC().Format(time.RFC3339))
	}
	if hasAfter && hasBefore && !before.After(after) {
		t.Errorf("%s: the generated window closes at %s, before it opens at %s",
			label, before.UTC().Format(time.RFC3339), after.UTC().Format(time.RFC3339))
	}
	return true
}

const presetMarker = "preset_contract.presets "

type presetDocument struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type presetPayload struct {
	Flags    []presetDocument `json:"flags"`
	Segments []presetDocument `json:"segments"`
}

// parsePresetPayload reads the machine-readable line the Node contract prints, so
// the documents validated here are byte-for-byte the ones the console built.
func parsePresetPayload(out []byte) (*presetPayload, error) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, presetMarker) {
			continue
		}
		var payload presetPayload
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, presetMarker)), &payload); err != nil {
			return nil, fmt.Errorf("preset payload is not valid JSON: %w", err)
		}
		return &payload, nil
	}
	return nil, fmt.Errorf("the contract printed no %q line", strings.TrimSpace(presetMarker))
}

// decodePresetDocument decodes one printed document into the wire type with
// unknown fields rejected: a misspelled or stale field name must fail here
// rather than be silently dropped (the server would reject it on save).
func decodePresetDocument(t *testing.T, label string, raw json.RawMessage, out any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("%s does not decode into the wire type: %v", label, err)
	}
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
