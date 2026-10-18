package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/pkg/model"
)

// The legacy (vanilla JS) console contracts have all been RETIRED: the
// report/rollout/i18n/resilience .mjs scripts under web/legacy were deleted
// once their behavior was re-created as vitest suites under web/src/__tests__
// (report, rollout, wizard, flagsList, polling, staleRoute, and the earlier
// flagEditor/segmentEditor/flagPublish/presets/dirtyBlocker suites). What
// remains here is the Go-side gate over that vitest suite and the Go half of
// the preset contract (which reads its machine-readable payload from the
// vitest successor).

// TestReactConsoleContract runs the full React console vitest suite (`pnpm
// vitest run` in the web/ package). The suite is the successor of every
// retired web/legacy/*_contract.mjs: it renders the real React pages against
// the real i18n catalogs in jsdom and pins the report family (every field,
// sequential/Bayesian label separation), the rollout block (all plan states,
// byte-exact request bodies, two-step confirms, the stale-plan guard, the
// report gate), the experiment wizard (per-step validation, the published-flag
// gate, the draft-only create body, the discard guard), the flags list
// (legacy sort/filter/pagination), the polling backoff and the stale-route
// guard.
//
// pnpm is an external toolchain dependency (already used for the web build).
// As with the PostgreSQL integration tests, which skip when docker is absent,
// this skips with a clear message when pnpm is unavailable — it is NOT a
// stubbed or faked pass: when pnpm is present (as in CI and this repo's dev
// env) the test executes the real suite and fails on any contract drift.
func TestReactConsoleContract(t *testing.T) {
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skip("pnpm not available; React console contract requires the web workspace toolchain (same external-tool gating as the PG/docker tests)")
	}
	// go test runs in the package directory, which is the web/ package itself.
	cmd := exec.CommandContext(t.Context(), pnpm, "vitest", "run")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("React console vitest contract failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// TestPresetContract runs the vitest successor of the retired
// preset_contract.mjs (web/src/__tests__/presets.test.tsx, via `pnpm vitest run
// presets` in the web/ package) and then re-validates every document it prints
// with the REAL server validators.
//
// The vitest half proves the console side against the React port's preset
// catalog (web-pages/src/presets): each preset builds a fresh document for the
// requested key, the console's own lintFlag/lintSegment accept it, generated
// time windows open in the future, every preset i18n key resolves in both
// catalogs, and the picker wiring (confirm dialog, source replacement,
// metadata carry-over, select reset) holds. It prints one machine-readable
// `preset_contract.presets {…}` line (byte-compatible with the retired script)
// via process.stdout.write — vitest swallows console.log on non-TTY runs. The
// Go half closes the loop the console cannot: model.ValidateFlag/
// model.ValidateSegment are a separate code path from the browser linter, so a
// preset that would 400 on save fails here instead of in front of an operator,
// and every time operand is re-read with model.ParseTimeOperand with a
// generated window required to open in the future (an elapsed window validates
// cleanly while quietly pinning every unit to the control value). Skips only
// when pnpm is genuinely unavailable (same external-tool gating as the other
// console contract tests).
func TestPresetContract(t *testing.T) {
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skip("pnpm not available; preset contract requires the web workspace toolchain (same external-tool gating as the PG/docker tests)")
	}
	cmd := exec.CommandContext(t.Context(), pnpm, "vitest", "run", "presets")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preset vitest contract failed: %v\n%s", err, out)
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
// The console builds its window timestamps at apply time (web-pages/src/
// presets), so these assertions never pin an absolute instant; they pin the
// two properties that keep the preset useful: the window opens in the future
// and stays ordered.
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

// parsePresetPayload reads the machine-readable line the vitest suite prints,
// so the documents validated here are byte-for-byte the ones the console
// built. The match is a substring scan, not a prefix: pnpm/vitest may decorate
// or interleave stdout lines on a non-TTY run, so the marker is located with
// strings.Index and the JSON read from there to the end of the line.
func parsePresetPayload(out []byte) (*presetPayload, error) {
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.Index(line, presetMarker)
		if idx < 0 {
			continue
		}
		var payload presetPayload
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[idx+len(presetMarker):])), &payload); err != nil {
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
