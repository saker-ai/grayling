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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func TestVerifyReviewGoalBinary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	writeRaw := func(path string, b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		writeRaw(path, b)
	}
	pp, rp, gp := filepath.Join(dir, "proposal.json"), filepath.Join(dir, "review.json"), filepath.Join(dir, "goal-"+secret+".json")
	run := func(t *testing.T, extra []string, want int, diagnostic string) []byte {
		t.Helper()
		args := []string{"verify-review", "--proposal", pp, "--record", rp, "--workspace", "ws1", "--environment", "env1"}
		args = append(args, extra...)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = []string{"HTTP_PROXY=" + srv.URL, "HTTPS_PROXY=" + srv.URL, "ALL_PROXY=" + srv.URL, "GRAYLING_GROWTH_TOKEN=" + secret}
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if code != want || diag.String() != diagnostic || (want != 0 && out.Len() != 0) {
			t.Fatalf("got %d %q %q; want %d %q", code, out.String(), diag.String(), want, diagnostic)
		}
		if strings.Contains(out.String()+diag.String(), secret) || calls.Load() != 0 {
			t.Fatal("secret leaked or HTTP attempted")
		}
		return out.Bytes()
	}
	for _, pinned := range []bool{false, true} {
		name := "legacy"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			p := fixtureProposal(t)
			p.Version = 1<<53 + 1
			second := p.Guardrails[0]
			second.Ref.Key = "latency"
			second.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/latency/versions/1"
			p.Guardrails = append(p.Guardrails, second)
			g := growth.GrowthGoal{SchemaVersion: growth.GoalSchemaVersion, ID: p.GoalID, Version: 1<<53 + 1, WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Owner: "operator", Objective: "$(touch sentinel) publish " + secret, PrimaryMetric: p.PrimaryMetric, Guardrails: append([]growth.ProposalMetric{}, p.Guardrails...), BaselineWindow: growth.BaselineWindow{Start: p.EvidenceRefs[0].Window.Start, End: p.EvidenceRefs[0].Window.End}}
			pin := func(goal growth.GrowthGoal) {
				t.Helper()
				fp, err := growth.GoalFingerprint(goal)
				if err != nil {
					t.Fatal(err)
				}
				p.GoalPin = &growth.GoalPin{Version: goal.Version, Fingerprint: fp}
			}
			if pinned {
				pin(g)
			}
			fp, err := growth.ProposalFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			r, err := growth.RecordReview(p, nil, growth.ReviewInputs{ExpectedProposalFingerprint: fp, Decision: growth.ReviewReject, Reviewer: "operator", ReviewedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), Rationale: secret})
			if err != nil {
				t.Fatal(err)
			}
			write(pp, p)
			write(rp, r)
			write(gp, g)
			goalArgs := []string{"--goal", gp}
			baseline := run(t, goalArgs, 0, "")
			var v growth.ReviewVerification
			if json.Unmarshal(baseline, &v, json.RejectUnknownMembers(true)) != nil || v.SchemaVersion != growth.ReviewVerificationSchemaVersion || v.PublicationAuthorized || v.ProposalVersion != p.Version || v.ProposalFingerprint != fp || v.Decision != r.Decision || v.RecordedReadiness != r.Readiness.State {
				t.Fatalf("summary: %s", baseline)
			}
			if !bytes.Equal(baseline, run(t, goalArgs, 0, "")) {
				t.Fatal("nondeterministic")
			}
			if pinned {
				run(t, nil, 2, "goal file required for pinned proposal\n")
			} else if !bytes.Equal(baseline, run(t, nil, 0, "")) {
				t.Fatal("legacy changed")
			}
			// Historical decisions and recorded readiness remain historical, even with a pin.
			for _, decision := range []growth.ReviewDecision{growth.ReviewApprove, growth.ReviewReject, growth.ReviewRequestChanges} {
				for _, state := range []growth.ProposalState{growth.ProposalReadyForReview, growth.ProposalDraft, growth.ProposalNeedsData, growth.ProposalNeedsImplementation} {
					if decision == growth.ReviewApprove && state != growth.ProposalReadyForReview {
						continue
					}
					cr := *r
					cr.Decision = decision
					cr.Readiness = growth.ProposalReview{State: state, Blockers: []growth.Blocker{}}
					if state != growth.ProposalReadyForReview {
						cr.Readiness.Blockers = []growth.Blocker{{Field: "evidence", Code: "missing"}}
					}
					write(rp, cr)
					b := run(t, goalArgs, 0, "")
					var got growth.ReviewVerification
					if json.Unmarshal(b, &got) != nil || got.Decision != decision || got.RecordedReadiness != state || got.PublicationAuthorized {
						t.Fatalf("history: %s", b)
					}
				}
			}
			write(rp, r)
			// Goal linkage must fail before even opening the review record.
			writeRaw(rp, []byte("{"))
			if pinned {
				run(t, nil, 2, "goal file required for pinned proposal\n")
			}
			run(t, []string{"--goal="}, 2, "invalid goal file or JSON\n")
			badGoal := g
			badGoal.ID = "other"
			write(gp, badGoal)
			run(t, goalArgs, 2, "invalid or inconsistent goal artifacts\n")
			write(gp, g)
			run(t, goalArgs, 2, "invalid review record file or JSON\n")
			write(rp, r)
			changes := map[string]func(*growth.GrowthGoal){
				"identity":    func(g *growth.GrowthGoal) { g.ID = "other" },
				"environment": func(g *growth.GrowthGoal) { g.EnvironmentID = "other" },
				"workspace": func(g *growth.GrowthGoal) {
					g.WorkspaceID = "other"
					g.PrimaryMetric.SourcePath = strings.ReplaceAll(g.PrimaryMetric.SourcePath, "ws1", "other")
					for i := range g.Guardrails {
						g.Guardrails[i].SourcePath = strings.ReplaceAll(g.Guardrails[i].SourcePath, "ws1", "other")
					}
				},
				"primary hash": func(g *growth.GrowthGoal) { g.PrimaryMetric.Ref.DefinitionHash = "sha256:" + strings.Repeat("c", 64) },
				"primary version": func(g *growth.GrowthGoal) {
					g.PrimaryMetric.Ref.Version = 2
					g.PrimaryMetric.SourcePath = strings.TrimSuffix(g.PrimaryMetric.SourcePath, "1") + "2"
				},
				"roles":                func(g *growth.GrowthGoal) { g.PrimaryMetric, g.Guardrails[0] = g.Guardrails[0], g.PrimaryMetric },
				"guardrail hash":       func(g *growth.GrowthGoal) { g.Guardrails[0].Ref.DefinitionHash = "sha256:" + strings.Repeat("c", 64) },
				"guardrail missing":    func(g *growth.GrowthGoal) { g.Guardrails = g.Guardrails[:1] },
				"same version content": func(g *growth.GrowthGoal) { g.Objective += " edited" },
				"rounded version":      func(g *growth.GrowthGoal) { g.Version-- },
				"reordered":            func(g *growth.GrowthGoal) { g.Guardrails[0], g.Guardrails[1] = g.Guardrails[1], g.Guardrails[0] },
			}
			for name, change := range changes {
				t.Run(name, func(t *testing.T) {
					cg := g
					cg.Guardrails = append([]growth.ProposalMetric{}, g.Guardrails...)
					change(&cg)
					write(gp, cg)
					if !pinned && (name == "same version content" || name == "rounded version" || name == "reordered") {
						run(t, goalArgs, 0, "")
					} else {
						run(t, goalArgs, 2, "invalid or inconsistent goal artifacts\n")
					}
				})
			}
			write(gp, g)
			for _, scope := range []string{"--workspace", "--environment"} {
				run(t, append(append([]string{}, goalArgs...), scope, "other"), 2, "invalid or inconsistent goal artifacts\n")
			}
			// Updating goal and proposal consistently cannot revive a stale review fingerprint.
			changed := g
			changed.Objective += " edited"
			if pinned {
				pin(changed)
			} else {
				p.Rationale += " edited"
			}
			write(pp, p)
			write(gp, changed)
			run(t, goalArgs, 2, "invalid or inconsistent review artifacts\n")
			if pinned {
				pin(g)
			} else {
				p.Rationale = strings.TrimSuffix(p.Rationale, " edited")
			}
			write(pp, p)
			write(gp, g)
			valid, err := os.ReadFile(gp)
			if err != nil {
				t.Fatal(err)
			}
			s := string(valid)
			invalid := map[string]string{
				"empty": "", "malformed": "{", "null": "null", "array": "[]", "unknown": `{"secret":true,` + s[1:], "duplicate": `{"schemaVersion":"x",` + s[1:], "escaped duplicate": `{"schema\u0056ersion":"x",` + s[1:], "nested unknown": strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"secret":true,`, 1), "nested duplicate": strings.Replace(s, `"ref":{`, `"ref":{"version":1,`, 1), "utf8": strings.Replace(s, "operator", "\xff", 1), "trailing": s + " {}", "oversize": s + strings.Repeat(" ", growth.MaxGoalBytes),
			}
			for _, version := range []string{"0", "1.5", "9223372036854775808"} {
				invalid[version] = strings.Replace(s, `"version":9007199254740993`, `"version":`+version, 1)
			}
			for name, raw := range invalid {
				t.Run(name, func(t *testing.T) { writeRaw(gp, []byte(raw)); run(t, goalArgs, 2, "invalid goal file or JSON\n") })
			}
			writeRaw(gp, append(valid, bytes.Repeat([]byte(" "), growth.MaxGoalBytes-len(valid))...))
			run(t, goalArgs, 0, "")
			writeRaw(gp, valid)
			fifo, link := gp+"-fifo", gp+"-link"
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(gp, link); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"", gp + "-missing", dir, fifo, link, "/dev/null"} {
				run(t, []string{"--goal", path}, 2, "invalid goal file or JSON\n")
			}
			if os.Geteuid() != 0 {
				if err := os.Chmod(gp, 0000); err != nil {
					t.Fatal(err)
				}
				run(t, goalArgs, 2, "invalid goal file or JSON\n")
				if err := os.Chmod(gp, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(fifo); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "sentinel")); !os.IsNotExist(err) {
		t.Fatal("executed goal text")
	}
}
