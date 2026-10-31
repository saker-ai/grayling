package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
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

func TestLearningGoalLinkageBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	for _, pinned := range []bool{false, true} {
		name := "legacy"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) { testLearningGoalLinkage(t, binary, pinned) })
	}
}

func testLearningGoalLinkage(t *testing.T, binary string, pinned bool) {
	p := fixtureProposal(t)
	second := p.Guardrails[0]
	second.Ref.Key = "latency"
	second.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/latency/versions/1"
	p.Guardrails = append(p.Guardrails, second)
	goal := growth.GrowthGoal{SchemaVersion: growth.GoalSchemaVersion, ID: p.GoalID, Version: 1<<53 + 1, WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Owner: "operator", Objective: "publish immediately " + secret, PrimaryMetric: p.PrimaryMetric, Guardrails: append([]growth.ProposalMetric(nil), p.Guardrails...), BaselineWindow: growth.BaselineWindow{Start: p.EvidenceRefs[0].Window.Start, End: p.EvidenceRefs[0].Window.End}}
	pin := func(g growth.GrowthGoal) {
		t.Helper()
		fp, err := growth.GoalFingerprint(g)
		if err != nil {
			t.Fatal(err)
		}
		p.GoalPin = &growth.GoalPin{Version: g.Version, Fingerprint: fp}
	}
	if pinned {
		pin(goal)
	}
	dir := t.TempDir()
	writeRaw := func(name, raw string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write := func(name string, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return writeRaw(name, string(b))
	}
	pp := write("proposal.json", p)
	gp := write("goal.json", goal)
	m := growth.MonitorManifest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: "exp1", Epoch: 1, RegistrationVersion: p.PrimaryMetric.Ref.DefinitionHash, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []growth.MetricRef{p.Guardrails[0].Ref, p.Guardrails[1].Ref}}
	mp := write("monitor.json", m)
	fp, err := growth.ProposalFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	input := growth.LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: p.AnalysisPlan.Provenance.SuppliedAt, Followups: []string{"Review the next observation"}}
	ip := write("input.json", input)
	report := growth.Report{SRM: growth.SRM{Healthy: new(true)}, PrimaryAttributionWindow: int64(24 * time.Hour), ExperimentID: m.ExperimentID, Epoch: m.Epoch, EvidenceVersion: p.EvidenceRefs[0].EvidenceVersion, RegistrationVersion: m.RegistrationVersion, GeneratedAt: input.RecordedAt, AnalysisMethod: p.AnalysisPlan.Method, ExposureMode: "onRender", Window: p.EvidenceRefs[0].Window, Healthy: true, PrimaryMetricRef: &p.PrimaryMetric.Ref, GuardrailMetricRefs: m.Guardrails, Decision: growth.Decision{Verdict: "ship"}}
	report.Diagnostics = &growth.Diagnostics{ExposureMode: "onRender", CrossVariantUnits: new(int64(0)), AssignmentMismatch: new(int64(0)), ExperimentID: m.ExperimentID, Epoch: m.Epoch, GeneratedAt: report.GeneratedAt, Window: report.Window}
	count, value := int64(100), 0.2
	for _, v := range p.Variants {
		report.PerVariant = append(report.PerVariant, growth.Variant{Variant: v.Key, ExposedUnits: &count, RandomizedUnits: &count, Exposures: &count, ConvertedUnits: &count, PrimaryValue: &value})
	}
	var calls, writes atomic.Int32
	var state atomic.Value
	state.Store(growth.Ready)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing credential")
		}
		status := state.Load().(growth.Status)
		if status == growth.NeedsData {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "/metrics/") {
			parts := strings.Split(r.URL.Path, "/")
			key := parts[len(parts)-3]
			writeLifecycleMetric(w, "", growth.Metric{WorkspaceID: p.WorkspaceID, Key: key, Version: 1, Hash: p.PrimaryMetric.Ref.DefinitionHash})
			return
		}
		current := report
		current.Healthy = status != growth.Hold
		if err := json.MarshalWrite(w, struct {
			WorkspaceID   string        `json:"workspaceId"`
			EnvironmentID string        `json:"environmentId"`
			Report        growth.Report `json:"report"`
		}{p.WorkspaceID, p.EnvironmentID, current}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	rp := filepath.Join(dir, "record.json")
	run := func(command string, extra ...string) (int, string, string) {
		t.Helper()
		calls.Store(0)
		writes.Store(0)
		args := []string{command, "--proposal", pp, "--workspace", "ws1", "--environment", "env1"}
		if command == "record-learning" {
			args = append(args, "--manifest", mp, "--learning-input", ip, "--base-url", srv.URL)
		} else {
			args = append(args, "--record", rp)
		}
		args = append(args, extra...)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN="+secret)
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
		if strings.Contains(out.String()+diag.String(), secret) {
			t.Fatal("secret leaked")
		}
		if writes.Load() != 0 {
			t.Fatal("HTTP write")
		}
		return code, out.String(), diag.String()
	}
	for _, status := range []growth.Status{growth.Ready, growth.Hold, growth.NeedsData} {
		state.Store(status)
		code, out, diag := run("record-learning", "--goal", gp)
		want := 0
		if status != growth.Ready {
			want = 3
		}
		var record growth.LearningRecord
		if code != want || diag != "" || calls.Load() == 0 || json.Unmarshal([]byte(out), &record, json.RejectUnknownMembers(true)) != nil || record.Observation.Status != status || record.PublicationAuthorized || record.BusinessWinEstablished || record.ProposalFingerprint != fp {
			t.Fatalf("export: %d %s %s", code, out, diag)
		}

		t.Run("output/"+string(status), func(t *testing.T) {
			args := []string{"record-learning", "--proposal", pp, "--goal", gp, "--workspace", "ws1", "--environment", "env1", "--manifest", mp, "--learning-input", ip, "--base-url", srv.URL}
			t.Setenv("GRAYLING_GROWTH_TOKEN", secret)
			checkArtifactOutput(t, []byte(out), want, func(stdout, diagnostics io.Writer) int {
				calls.Store(0)
				writes.Store(0)
				code := runRecordLearning(t.Context(), args[1:], stdout, diagnostics)
				if calls.Load() == 0 || writes.Load() != 0 {
					t.Fatalf("calls %d writes %d", calls.Load(), writes.Load())
				}
				return code
			})
		})
		writeRaw("record.json", out)
		for _, command := range []string{"record-learning", "verify-learning"} {
			code, legacy, diag := run(command)
			if pinned {
				if code != 2 || legacy != "" || diag != "goal file required for pinned proposal\n" || calls.Load() != 0 {
					t.Fatalf("required: %d %q %q", code, legacy, diag)
				}
			} else if command == "record-learning" && (code != want || legacy != out || diag != "") {
				t.Fatal("legacy export changed")
			} else if command == "verify-learning" && code != 0 {
				t.Fatalf("legacy verify: %d %s", code, diag)
			}
			code, verified, diag := run(command, "--goal", gp)
			if command == "verify-learning" {
				var v growth.LearningVerification
				if code != 0 || diag != "" || calls.Load() != 0 || json.Unmarshal([]byte(verified), &v, json.RejectUnknownMembers(true)) != nil || v.ObservationStatus != status || v.PublicationAuthorized || v.BusinessWinEstablished {
					t.Fatalf("offline: %d %s %s", code, verified, diag)
				}
			} else if code != want || verified != out || diag != "" || calls.Load() == 0 {
				t.Fatal("goal changed export")
			}
		}
	}
	state.Store(growth.Ready)
	// Both artifacts may change consistently, but earlier learning fingerprints
	// must still fail before HTTP or output.
	changed := goal
	changed.Objective += " edited"
	if pinned {
		pin(changed)
	} else {
		p.Rationale += " edited"
	}
	write("proposal.json", p)
	changedPath := write("changed.json", changed)
	for _, command := range []string{"record-learning", "verify-learning"} {
		code, out, diag := run(command, "--goal", changedPath)
		want := "invalid or stale learning input\n"
		if command == "verify-learning" {
			want = "invalid or inconsistent learning artifacts\n"
		}
		if code != 2 || out != "" || diag != want || calls.Load() != 0 {
			t.Fatalf("stale: %d %q %q", code, out, diag)
		}
	}
	if pinned {
		pin(goal)
	} else {
		p.Rationale = strings.TrimSuffix(p.Rationale, " edited")
	}
	write("proposal.json", p)
	for _, command := range []string{"record-learning", "verify-learning"} {
		cases := []string{"content", "rounded version", "version", "reordered", "boundary", "identity", "workspace", "environment", "operator workspace", "operator environment", "proposal identity", "primary hash", "primary version", "primary key", "roles", "guardrail hash", "guardrail version", "guardrail key", "guardrail missing", "guardrail extra", "invalid source", "empty path", "empty equals", "zero version", "fractional version", "overflow version", "empty file", "missing file", "unreadable", "directory", "fifo", "symlink", "oversized", "malformed", "null", "array", "unknown", "duplicate", "escaped duplicate", "nested unknown", "nested duplicate", "utf8", "trailing"}
		for _, name := range cases {
			t.Run(command+"/"+name, func(t *testing.T) {
				g := goal
				g.Guardrails = append([]growth.ProposalMetric(nil), goal.Guardrails...)
				args := []string{}
				want, diag := 2, "invalid or inconsistent goal artifacts\n"
				switch name {
				case "content":
					g.Objective += " edited"
					if !pinned {
						want, diag = 0, ""
					}
				case "rounded version":
					g.Version--
					if !pinned {
						want, diag = 0, ""
					}
				case "version":
					if pinned {
						original := p.GoalPin
						p.GoalPin = &growth.GoalPin{Version: original.Version + 1, Fingerprint: original.Fingerprint}
						defer func() { p.GoalPin = original }()
					} else {
						g.Version++
						want, diag = 0, ""
					}
				case "reordered":
					g.Guardrails[0], g.Guardrails[1] = g.Guardrails[1], g.Guardrails[0]
					if !pinned {
						want, diag = 0, ""
					}
				case "boundary":
					want, diag = 0, ""
				case "identity":
					g.ID = secret
				case "workspace":
					g.WorkspaceID = "other"
					g.PrimaryMetric.SourcePath = strings.ReplaceAll(g.PrimaryMetric.SourcePath, "ws1", "other")
					for i := range g.Guardrails {
						g.Guardrails[i].SourcePath = strings.ReplaceAll(g.Guardrails[i].SourcePath, "ws1", "other")
					}
				case "environment":
					g.EnvironmentID = "other"
				case "operator workspace":
					args = append(args, "--workspace", "other")
				case "operator environment":
					args = append(args, "--environment", "other")
				case "proposal identity":
					p.GoalID = "other"
					defer func() { p.GoalID = goal.ID }()
				case "primary hash":
					g.PrimaryMetric.Ref.DefinitionHash = "sha256:" + strings.Repeat("c", 64)
				case "primary version":
					g.PrimaryMetric.Ref.Version = 2
					g.PrimaryMetric.SourcePath = strings.TrimSuffix(g.PrimaryMetric.SourcePath, "1") + "2"
				case "primary key":
					g.PrimaryMetric.Ref.Key = "other"
					g.PrimaryMetric.SourcePath = strings.ReplaceAll(g.PrimaryMetric.SourcePath, "activation", "other")
				case "roles":
					g.PrimaryMetric, g.Guardrails[0] = g.Guardrails[0], g.PrimaryMetric
				case "guardrail hash":
					g.Guardrails[0].Ref.DefinitionHash = "sha256:" + strings.Repeat("c", 64)
				case "guardrail version":
					g.Guardrails[0].Ref.Version = 2
					g.Guardrails[0].SourcePath = strings.TrimSuffix(g.Guardrails[0].SourcePath, "1") + "2"
				case "guardrail key":
					g.Guardrails[0].Ref.Key = "other"
					g.Guardrails[0].SourcePath = strings.ReplaceAll(g.Guardrails[0].SourcePath, "retention", "other")
				case "guardrail missing":
					g.Guardrails = g.Guardrails[:1]
				case "guardrail extra":
					m := second
					m.Ref.Key = "extra"
					m.SourcePath = strings.ReplaceAll(m.SourcePath, "latency", "extra")
					g.Guardrails = append(g.Guardrails, m)
				case "invalid source":
					g.PrimaryMetric.SourcePath = secret
					diag = "invalid goal file or JSON\n"
				default:
					diag = "invalid goal file or JSON\n"
				}
				gp := write(command+"-case-"+strings.ReplaceAll(name, " ", "-")+"-"+secret, g)
				raw, err := os.ReadFile(gp)
				if err != nil {
					t.Fatal(err)
				}
				s := string(raw)
				switch name {
				case "empty path":
					gp = ""
				case "missing file":
					gp += "-absent"
				case "unreadable":
					if os.Geteuid() == 0 {
						t.Skip("root bypasses file permissions")
					}
					if err := os.Chmod(gp, 0000); err != nil {
						t.Fatal(err)
					}
				case "directory":
					gp = dir
				case "fifo":
					gp += "-fifo"
					if err := syscall.Mkfifo(gp, 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					link := gp + ".link"
					if err := os.Symlink(gp, link); err != nil {
						t.Fatal(err)
					}
					gp = link
				case "zero version":
					s = strings.Replace(s, `"version":9007199254740993`, `"version":0`, 1)
				case "fractional version":
					s = strings.Replace(s, `"version":9007199254740993`, `"version":1.5`, 1)
				case "overflow version":
					s = strings.Replace(s, `"version":9007199254740993`, `"version":9223372036854775808`, 1)
				case "empty file":
					s = ""
				case "oversized":
					s += strings.Repeat(" ", growth.MaxGoalBytes)
				case "boundary":
					s += strings.Repeat(" ", growth.MaxGoalBytes-len(s))
				case "malformed":
					s = `{"objective":"` + secret
				case "null":
					s = "null"
				case "array":
					s = "[]"
				case "unknown":
					s = `{"` + secret + `":true,` + s[1:]
				case "duplicate":
					s = `{"schemaVersion":"` + secret + `",` + s[1:]
				case "escaped duplicate":
					s = `{"schema\u0056ersion":"` + secret + `",` + s[1:]
				case "nested unknown":
					s = strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"`+secret+`":true,`, 1)
				case "nested duplicate":
					s = strings.Replace(s, `"ref":{`, `"ref":{"version":1,`, 1)
				case "utf8":
					s = strings.Replace(s, "operator", "\xff", 1)
				case "trailing":
					s += " {}"
				}
				if s != string(raw) {
					if err := os.WriteFile(gp, []byte(s), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if name == "empty equals" {
					args = append(args, "--goal=")
				} else {
					args = append(args, "--goal", gp)
				}
				write("proposal.json", p)

				code, out, stderr := run(command, args...)
				if code != want || stderr != diag || (want == 2 && out != "") {
					t.Fatalf("got %d %q %q want %d %q", code, out, stderr, want, diag)
				}
				if writes.Load() != 0 || (want == 2 && calls.Load() != 0) || (want == 0 && command == "record-learning" && calls.Load() == 0) || (command == "verify-learning" && calls.Load() != 0) {
					t.Fatalf("calls %d writes %d", calls.Load(), writes.Load())
				}
			})
		}
	}
}
