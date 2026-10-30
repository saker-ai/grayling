package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func TestVerifyLearningBinary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	p := fixtureProposal(t)
	r := exportedLearningFixture(t)
	p.Hypothesis = r.OriginalHypothesis
	r.ProposalFingerprint, _ = growth.ProposalFingerprint(p)
	pp := filepath.Join(dir, "proposal.json")
	rp := filepath.Join(dir, "record.json")
	write := func(path string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(pp, p)
	write(rp, r)
	base := []string{"verify-learning", "--proposal", pp, "--record", rp, "--workspace", "ws1", "--environment", "env1"}
	run := func(args []string) (int, []byte, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN=CREDENTIAL_SENTINEL", "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1")
		var out, diag bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diag
		err := cmd.Run()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if strings.Contains(out.String()+diag.String(), "CREDENTIAL_SENTINEL") {
			t.Fatal("credential leak")
		}
		return code, out.Bytes(), diag.String()
	}
	for _, status := range []growth.Status{growth.Ready, growth.Hold, growth.NeedsData} {
		candidate := r
		if status == growth.Ready {
			candidate.Observation.Status = status
			candidate.Observation.ServerVerdict = "ship"
			candidate.Observation.Blockers = []growth.MonitorBlocker{}
		}
		if status == growth.NeedsData {
			m := r.LinkedExperiment
			candidate.Observation = growth.MonitorSummary{WorkspaceID: m.WorkspaceID, EnvironmentID: m.EnvironmentID, ExperimentID: m.ExperimentID, Epoch: m.Epoch, PrimaryMetric: m.PrimaryMetric, Guardrails: m.Guardrails, Status: status, Blockers: []growth.MonitorBlocker{{Code: "evidence_missing", Remediation: "restore_evidence_access"}}}
		}
		write(rp, candidate)
		code, b, d := run(base)
		var got growth.LearningVerification
		if code != 0 || d != "" || json.Unmarshal(b, &got) != nil || got.ObservationStatus != status || got.PublicationAuthorized || got.BusinessWinEstablished || got.ExperimentID != "linked" {
			t.Fatalf("%d %s %s", code, b, d)
		}
		_, again, _ := run(base)
		if !bytes.Equal(b, again) {
			t.Fatal("nondeterministic")
		}
	}
	write(rp, r)
	fail := func(args []string, diagnostic string) {
		t.Helper()
		code, b, d := run(args)
		if code != 2 || len(b) != 0 || d != diagnostic+"\n" {
			t.Fatalf("%d %s %s", code, b, d)
		}
	}
	stale := p
	stale.Rationale += " edited"
	write(pp, stale)
	fail(base, "invalid or inconsistent learning artifacts")
	write(pp, p)
	copied := r
	copied.OriginalHypothesis = "incorrect"
	write(rp, copied)
	fail(base, "invalid or inconsistent learning artifacts")
	write(rp, r)
	for name, mutate := range map[string]func(*growth.Proposal, *growth.LearningRecord){
		"identity": func(p *growth.Proposal, r *growth.LearningRecord) { r.ProposalID = "other" },
		"version":  func(p *growth.Proposal, r *growth.LearningRecord) { r.ProposalVersion++ },
		"baseline": func(p *growth.Proposal, r *growth.LearningRecord) {
			r.ProposalBaselineReferences[0].Window.Backend = "other"
		},
		"primary role": func(p *growth.Proposal, r *growth.LearningRecord) {
			p.PrimaryMetric, p.Guardrails[0] = p.Guardrails[0], p.PrimaryMetric
			r.ProposalFingerprint, _ = growth.ProposalFingerprint(*p)
		},
		"guard version": func(p *growth.Proposal, r *growth.LearningRecord) {
			p.Guardrails[0].Ref.Version++
			r.ProposalFingerprint, _ = growth.ProposalFingerprint(*p)
		},
		"schema": func(p *growth.Proposal, r *growth.LearningRecord) {
			p.SchemaVersion = "future"
			r.ProposalFingerprint, _ = growth.ProposalFingerprint(*p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Decode independent copies before modifying nested fields.
			pb, _ := json.Marshal(p)
			rb, _ := json.Marshal(r)
			var cp growth.Proposal
			var cr growth.LearningRecord
			if json.Unmarshal(pb, &cp) != nil || json.Unmarshal(rb, &cr) != nil {
				t.Fatal("fixture")
			}
			mutate(&cp, &cr)
			write(pp, cp)
			write(rp, cr)
			fail(base, "invalid or inconsistent learning artifacts")
		})
	}
	write(pp, p)
	write(rp, r)
	for _, path := range []string{pp, rp} {
		valid, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(valid)
		limit := growth.MaxProposalBytes
		if path == rp {
			limit = growth.MaxLearningRecordBytes
		}
		cases := map[string]string{"malformed": "{", "null": "null", "array": "[]", "unknown": `{"execute":"secret",` + s[1:], "duplicate": `{"schemaVersion":"x",` + s[1:], "escaped duplicate": `{"schema\u0056ersion":"x",` + s[1:], "trailing": s + " {}", "utf8": strings.Replace(s, "schemaVersion", "\xff", 1), "oversize": s + strings.Repeat(" ", limit), "nested unknown": strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"execute":true,`, 1)}
		diagnostic := "invalid proposal file or JSON"
		if path == rp {
			diagnostic = "invalid learning record file or JSON"
		}
		for name, raw := range cases {
			t.Run(filepath.Base(path)+name, func(t *testing.T) {
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				fail(base, diagnostic)
			})
		}
		if err := os.WriteFile(path, append(valid, bytes.Repeat([]byte(" "), limit-len(valid))...), 0600); err != nil {
			t.Fatal(err)
		}
		if code, b, d := run(base); code != 0 {
			t.Fatalf("exact limit: %d %s %s", code, b, d)
		}
		os.WriteFile(path, valid, 0600)
		fifo := filepath.Join(dir, filepath.Base(path)+"fifo")
		if err := syscall.Mkfifo(fifo, 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, filepath.Base(path)+"link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{fifo, link, dir, filepath.Join(dir, "absent")} {
			args := append([]string{}, base...)
			if path == pp {
				args[2] = bad
			} else {
				args[4] = bad
			}
			fail(args, diagnostic)
		}
	}
	for _, flag := range []string{"--base-url", "--token-env", "--timeout"} {
		fail(append(append([]string{}, base...), flag, "SECRET"), "invalid flags; use --help")
	}
	fail([]string{"verify-learning"}, "invalid verification configuration")
	fail(append(append([]string{}, base...), "extra"), "invalid verification configuration")
	args := append([]string{}, base...)
	args[6] = "other"
	fail(args, "invalid or inconsistent learning artifacts")
	if _, err := os.Stat(filepath.Join(dir, "sentinel")); !os.IsNotExist(err) {
		t.Fatal("executed injection")
	}
}
