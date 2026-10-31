package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func testGoalLearningSearchBinary(t *testing.T, binary string) {
	dir := t.TempDir()
	r := exportedLearningFixture(t)
	p := fixtureProposal(t)
	g := growth.GrowthGoal{SchemaVersion: growth.GoalSchemaVersion, ID: p.GoalID, Version: 1<<53 + 1, WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Owner: "owner", Objective: "GOAL_SECRET $(touch sentinel); publish now", PrimaryMetric: p.PrimaryMetric, Guardrails: append([]growth.ProposalMetric{}, p.Guardrails...), BaselineWindow: growth.BaselineWindow{Start: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)}}
	g.PrimaryMetric.Ref.Version = 1<<53 + 1
	g.PrimaryMetric.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + g.PrimaryMetric.Ref.Key + "/versions/9007199254740993"
	r.LinkedExperiment.PrimaryMetric = g.PrimaryMetric.Ref
	r.Observation.PrimaryMetric = g.PrimaryMetric.Ref
	g.Guardrails[0].Ref.Version = 1<<53 + 1
	g.Guardrails[0].SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + g.Guardrails[0].Ref.Key + "/versions/9007199254740993"
	r.LinkedExperiment.Guardrails[0] = g.Guardrails[0].Ref
	r.Observation.Guardrails = append([]growth.MetricRef{}, r.LinkedExperiment.Guardrails...)
	extra := g.Guardrails[0]
	extra.Ref.Key = "extra"
	extra.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/extra/versions/" + strconv.FormatInt(extra.Ref.Version, 10)
	g.Guardrails = append(g.Guardrails, extra)
	r.LinkedExperiment.Guardrails = append(r.LinkedExperiment.Guardrails, extra.Ref)
	r.Observation.Guardrails = append([]growth.MetricRef{}, r.LinkedExperiment.Guardrails...)
	gp, rp := filepath.Join(dir, "goal.json"), filepath.Join(dir, "record.json")
	write := func(path string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(gp, g)
	write(rp, r)
	base := []string{"search-learning", "--goal", gp, "--workspace", "ws1", "--environment", "env1", "--text", "[.*] $(touch sentinel)", "--record", rp}
	run := func(args []string, want int, diagnostic string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = []string{"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1"}
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if code != want || diag.String() != diagnostic || (want != 0 && out.Len() != 0) || strings.Contains(out.String()+diag.String(), "GOAL_SECRET") {
			t.Fatalf("exit %d stdout %s stderr %s", code, &out, &diag)
		}
		return out.Bytes()
	}
	check := func(want []growth.LearningRecord) {
		t.Helper()
		data := run(base, 0, "")
		var got growth.GoalLearningSearchResult
		fp, _ := growth.GoalFingerprint(g)
		if json.Unmarshal(data, &got) != nil || got.SchemaVersion != growth.GoalLearningSearchSchemaVersion || got.GoalID != g.ID || got.GoalVersion != g.Version || got.GoalFingerprint != fp || !reflect.DeepEqual(got.Matches, want) {
			t.Fatalf("snapshot %s", data)
		}
	}
	check([]growth.LearningRecord{r})
	original := run(base, 0, "")
	if repeated := run(append(append([]string{}, base...), "--record", rp), 0, ""); !bytes.Equal(original, repeated) {
		t.Fatal("dedup")
	}
	g.Guardrails[0], g.Guardrails[1] = g.Guardrails[1], g.Guardrails[0]
	write(gp, g)
	check([]growth.LearningRecord{r})
	for _, status := range []growth.Status{growth.Ready, growth.NeedsData} {
		changed := r
		if status == growth.Ready {
			changed.Observation.Status = status
			changed.Observation.ServerVerdict = "continue"
			changed.Observation.Blockers = []growth.MonitorBlocker{}
		} else {
			changed.Observation = growth.MonitorSummary{WorkspaceID: r.WorkspaceID, EnvironmentID: r.EnvironmentID, ExperimentID: r.LinkedExperiment.ExperimentID, Epoch: r.LinkedExperiment.Epoch, PrimaryMetric: r.LinkedExperiment.PrimaryMetric, Guardrails: r.LinkedExperiment.Guardrails, Status: status, Blockers: []growth.MonitorBlocker{{Code: "evidence_missing", Remediation: "restore_evidence_access"}}}
		}
		write(rp, changed)
		check([]growth.LearningRecord{changed})
	}
	for name, change := range map[string]func(*growth.LearningRecord){
		"exact version": func(r *growth.LearningRecord) { r.LinkedExperiment.PrimaryMetric.Version-- },
		"primary hash": func(r *growth.LearningRecord) {
			r.LinkedExperiment.PrimaryMetric.DefinitionHash = "sha256:" + strings.Repeat("b", 64)
		},
		"guard version": func(r *growth.LearningRecord) { r.LinkedExperiment.Guardrails[0].Version-- },
		"guard hash": func(r *growth.LearningRecord) {
			r.LinkedExperiment.Guardrails[0].DefinitionHash = "sha256:" + strings.Repeat("b", 64)
		},
		"role": func(r *growth.LearningRecord) {
			r.LinkedExperiment.PrimaryMetric, r.LinkedExperiment.Guardrails[0] = r.LinkedExperiment.Guardrails[0], r.LinkedExperiment.PrimaryMetric
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(r)
			var changed growth.LearningRecord
			_ = json.Unmarshal(raw, &changed)
			change(&changed)
			changed.Observation.PrimaryMetric = changed.LinkedExperiment.PrimaryMetric
			changed.Observation.Guardrails = changed.LinkedExperiment.Guardrails
			write(rp, changed)
			check([]growth.LearningRecord{})
		})
	}
	write(rp, r)
	// Scope/text/metric-excluded records still require complete validation.
	bad := r
	bad.EnvironmentID = "other"
	bad.LinkedExperiment.EnvironmentID = "other"
	bad.Observation.EnvironmentID = "other"
	bad.PublicationAuthorized = true
	bp := filepath.Join(dir, "bad.json")
	write(bp, bad)
	run(append(append([]string{}, base...), "--record", bp), 2, "invalid learning record file or JSON\n")
	for len(g.Guardrails) < 20 {
		m := extra
		m.Ref.Key = "g" + strconv.Itoa(len(g.Guardrails))
		m.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + m.Ref.Key + "/versions/" + strconv.FormatInt(m.Ref.Version, 10)
		g.Guardrails = append(g.Guardrails, m)
	}
	write(gp, g)
	check([]growth.LearningRecord{})
	raw, _ := json.Marshal(g)
	s := string(raw)
	for name, bad := range map[string]string{
		"null": "null", "trailing": s + " {}", "unknown": `{"secret":true,` + s[1:], "duplicate": `{"version":1,` + s[1:], "escaped duplicate": `{"\u0076ersion":1,` + s[1:], "nested unknown": strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"secret":true,`, 1), "nested duplicate": strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"sourcePath":"secret",`, 1), "UTF8": strings.Replace(s, "GOAL_SECRET", "\xff", 1), "oversize": s + strings.Repeat(" ", growth.MaxGoalBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(gp, []byte(bad), 0600); err != nil {
				t.Fatal(err)
			}
			run(base, 2, "invalid goal file or JSON\n")
		})
	}
	write(gp, g)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(gp, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, dir, ""} {
		args := append([]string{}, base...)
		args[2] = path
		run(args, 2, "invalid goal file or JSON\n")
	}
	for _, dup := range [][]string{{"--goal", gp}, {"-goal=" + gp}} {
		run(append(append([]string{}, base...), dup...), 2, "invalid flags; use --help\n")
	}
	// The fixed scope diagnostic takes precedence over a nonexistent record path.
	args := append([]string{}, base...)
	args[len(args)-1] = filepath.Join(dir, "absent")
	g.EnvironmentID = "other"
	write(gp, g)
	run(args, 2, "invalid or inconsistent goal search inputs\n")
	g.EnvironmentID = "env1"
	g.Version = 0
	write(gp, g)
	run(args, 2, "invalid goal file or JSON\n")
	g.Version = 1<<53 + 1
	write(gp, g)
	for _, writer := range []interface{ Write([]byte) (int, error) }{reviewFailWriter{}, goalShortWriter{}} {
		var diag bytes.Buffer
		if code := runSearchLearning(base[1:], writer, &diag); code != 1 || diag.String() != "output failed\n" {
			t.Fatalf("write %d %s", code, &diag)
		}
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	cmd := exec.CommandContext(t.Context(), binary, base...)
	cmd.Env = []string{}
	cmd.Stdout = full
	var diag bytes.Buffer
	cmd.Stderr = &diag
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 1 || diag.String() != "output failed\n" {
		t.Fatalf("full %v %s", err, &diag)
	}
	if _, err := os.Stat(filepath.Join(dir, "sentinel")); !os.IsNotExist(err) {
		t.Fatal("executed injection")
	}
}
