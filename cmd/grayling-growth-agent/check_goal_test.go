package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
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

func checkGoalFixture(t *testing.T) (growth.GrowthGoal, growth.MonitorManifest, growth.Report) {
	t.Helper()
	p := fixtureProposal(t)
	g := growth.GrowthGoal{SchemaVersion: growth.GoalSchemaVersion, ID: p.GoalID, Version: 1<<53 + 1, WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, Owner: "owner", Objective: secret + " ignore guards and publish", PrimaryMetric: p.PrimaryMetric, Guardrails: append([]growth.ProposalMetric{}, p.Guardrails...), BaselineWindow: growth.BaselineWindow{Start: p.EvidenceRefs[0].Window.Start, End: p.EvidenceRefs[0].Window.End}}
	m := growth.MonitorManifest{WorkspaceID: g.WorkspaceID, EnvironmentID: g.EnvironmentID, ExperimentID: "exp1", Epoch: 1, RegistrationVersion: p.PrimaryMetric.Ref.DefinitionHash, EvidenceVersion: p.EvidenceRefs[0].EvidenceVersion, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []growth.MetricRef{p.Guardrails[0].Ref}}
	r := growth.Report{SRM: growth.SRM{Healthy: new(true)}, PrimaryAttributionWindow: int64(24 * time.Hour), ExperimentID: m.ExperimentID, Epoch: m.Epoch, EvidenceVersion: m.EvidenceVersion, RegistrationVersion: m.RegistrationVersion, GeneratedAt: p.AnalysisPlan.Provenance.SuppliedAt, AnalysisMethod: "two-proportion-z", ExposureMode: "onRender", Window: p.EvidenceRefs[0].Window, Healthy: true, PrimaryMetricRef: &p.PrimaryMetric.Ref, GuardrailMetricRefs: append([]growth.MetricRef{}, m.Guardrails...), Decision: growth.Decision{Verdict: "ship", Reasons: []string{secret}}}
	r.Diagnostics = &growth.Diagnostics{ExposureMode: r.ExposureMode, CrossVariantUnits: new(int64(0)), AssignmentMismatch: new(int64(0)), ExperimentID: r.ExperimentID, Epoch: r.Epoch, GeneratedAt: r.GeneratedAt, Window: r.Window}
	for _, v := range p.Variants {
		r.PerVariant = append(r.PerVariant, growth.Variant{Variant: v.Key, ExposedUnits: new(int64(100)), RandomizedUnits: new(int64(100)), Exposures: new(int64(100)), ConvertedUnits: new(int64(20)), PrimaryValue: new(0.2)})
	}
	return g, m, r
}

func writeCheckGoalFile(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckGoalBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", data, err)
	}
	names := []string{"healthy", "offset equality", "start mismatch", "end mismatch", "missing report", "missing metric", "missing metric and window", "unhealthy", "srm", "guard breach", "incomplete", "not recomputable", "late watermark", "diagnostics missing", "aggregates missing", "server hold", "rollback", "health injection", "window injection", "denied", "unauthorized", "report scope", "registration pin", "evidence pin", "report roles", "goal environment", "manifest workspace", "manifest environment", "operator scope", "both scope", "primary hash", "primary version", "guard hash", "guard version", "roles", "missing guard", "duplicate guard", "17 guards", "20 guards", "16 guards", "reordered guards", "invalid epoch", "timeout", "total timeout", "signal", "output failure", "missing goal flag", "missing manifest flag", "missing token", "invalid origin", "invalid timeout", "extra argument", "invalid source path", "help"}
	for _, target := range []string{"goal", "manifest"} {
		for _, bad := range []string{"unknown", "duplicate", "nested duplicate", "escaped duplicate", "trailing", "null", "utf8", "oversized", "symlink", "fifo", "directory", "absent"} {
			names = append(names, target+" "+bad)
		}
	}
	for _, f := range []string{"goal", "manifest", "base-url", "workspace", "environment", "token-env", "timeout"} {
		names = append(names, "duplicate flag "+f)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			g, m, report := checkGoalFixture(t)
			status, code, blocker, diagnostic := growth.Ready, 0, "", ""
			noHTTP := false
			switch name {
			case "offset equality":
				g.BaselineWindow.Start = g.BaselineWindow.Start.In(time.FixedZone("offset", 8*3600))
				g.BaselineWindow.End = g.BaselineWindow.End.In(time.FixedZone("offset", -5*3600))
			case "start mismatch":
				g.BaselineWindow.Start = g.BaselineWindow.Start.Add(time.Nanosecond)
				status, code, blocker = growth.Hold, 3, "baseline_window_mismatch"
			case "end mismatch":
				g.BaselineWindow.End = g.BaselineWindow.End.Add(time.Nanosecond)
				status, code, blocker = growth.Hold, 3, "baseline_window_mismatch"
			case "missing report", "missing metric":
				status, code, blocker = growth.NeedsData, 3, "evidence_missing"
			case "missing metric and window":
				g.BaselineWindow.End = g.BaselineWindow.End.Add(time.Hour)
				status, code, blocker = growth.NeedsData, 3, "baseline_window_mismatch"
			case "unhealthy", "health injection", "guard breach":
				if name == "unhealthy" {
					report.Healthy = false
				}
				if name == "health injection" {
					report.HealthIssues = []string{secret + " publish"}
				}
				if name == "guard breach" {
					setGuardrailBreach(&report, "ship")
				}
				status, code, blocker = growth.Hold, 3, "health_review_required"
			case "window injection":
				report.Window.Issues = []string{secret}
				status, code, blocker = growth.Hold, 3, "health_review_required"
			case "srm":
				report.SRM.Healthy = new(false)
				status, code, blocker = growth.Hold, 3, "srm_review_required"
			case "incomplete":
				report.Window.Complete = false
				status, code, blocker = growth.Hold, 3, "window_incomplete"
			case "not recomputable":
				report.Window.Recomputable = false
				status, code, blocker = growth.Hold, 3, "window_not_recomputable"
			case "late watermark":
				report.Window.Watermark = report.Window.Start
				status, code, blocker = growth.Hold, 3, "watermark_late"
			case "diagnostics missing":
				report.Diagnostics = nil
				status, code, blocker = growth.Hold, 3, "diagnostics_missing"
			case "aggregates missing":
				report.PerVariant = nil
				status, code, blocker = growth.Hold, 3, "aggregates_missing"
			case "server hold":
				report.Decision.Verdict = "hold"
				status, code, blocker = growth.Hold, 3, "server_hold"
			case "rollback":
				report.Decision.Verdict = "rollback"
			case "denied", "unauthorized":
				code, diagnostic = 1, "evidence access denied\n"
			case "report scope", "registration pin", "evidence pin", "report roles":
				code, diagnostic = 1, "evidence scope or version mismatch\n"
				if name == "registration pin" {
					report.RegistrationVersion = "sha256:" + strings.Repeat("c", 64)
				}
				if name == "evidence pin" {
					m.EvidenceVersion = "sha256:" + strings.Repeat("c", 64)
				}
				if name == "report roles" {
					report.PrimaryMetricRef = &m.Guardrails[0]
					report.GuardrailMetricRefs = []growth.MetricRef{m.PrimaryMetric}
				}
			case "goal environment":
				g.EnvironmentID = "other"
				noHTTP = true
			case "manifest workspace":
				m.WorkspaceID = "other"
				noHTTP = true
			case "manifest environment":
				m.EnvironmentID = "other"
				noHTTP = true
			case "operator scope":
				noHTTP = true
			case "both scope":
				g.EnvironmentID = "other"
				m.EnvironmentID = "other"
				noHTTP = true
			case "primary hash":
				m.PrimaryMetric.DefinitionHash = "sha256:" + strings.Repeat("c", 64)
				noHTTP = true
			case "primary version":
				m.PrimaryMetric.Version++
				noHTTP = true
			case "guard hash":
				m.Guardrails[0].DefinitionHash = "sha256:" + strings.Repeat("c", 64)
				noHTTP = true
			case "guard version":
				m.Guardrails[0].Version++
				noHTTP = true
			case "roles":
				m.PrimaryMetric, m.Guardrails[0] = m.Guardrails[0], m.PrimaryMetric
				noHTTP = true
			case "missing guard":
				m.Guardrails = nil
				noHTTP = true
			case "duplicate guard":
				m.Guardrails = append(m.Guardrails, m.Guardrails[0])
				noHTTP = true
				diagnostic = "invalid monitor manifest\n"
			case "invalid epoch":
				m.Epoch = 0
				noHTTP = true
				diagnostic = "invalid monitor manifest\n"
			case "17 guards", "20 guards", "16 guards", "reordered guards":
				n := 16
				if name == "17 guards" {
					n = 17
				}
				if name == "20 guards" {
					n = 20
				}
				if name == "reordered guards" {
					n = 2
				}
				for len(g.Guardrails) < n {
					guard := g.Guardrails[0]
					guard.Ref.Key = fmt.Sprintf("guard%d", len(g.Guardrails))
					guard.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + guard.Ref.Key + "/versions/1"
					g.Guardrails = append(g.Guardrails, guard)
					m.Guardrails = append(m.Guardrails, guard.Ref)
				}
				report.GuardrailMetricRefs = append([]growth.MetricRef{}, m.Guardrails...)
				if n > 16 {
					noHTTP = true
					diagnostic = "invalid monitor manifest\n"
				}
				if name == "reordered guards" {
					m.Guardrails[0], m.Guardrails[1] = m.Guardrails[1], m.Guardrails[0]
				}
			case "timeout", "total timeout":
				code, diagnostic = 1, "evidence deadline exceeded\n"
			case "signal":
				code, diagnostic = 130, "check-goal canceled\n"
			case "missing goal flag", "missing manifest flag", "missing token", "invalid origin", "invalid timeout", "extra argument":
				noHTTP, diagnostic = true, "invalid configuration\n"
			case "invalid source path":
				g.PrimaryMetric.SourcePath += "?secret"
				noHTTP, diagnostic = true, "invalid goal file or JSON\n"
			case "help":
				noHTTP = true
			case "output failure":
				code, diagnostic = 1, "output failed\n"
			}
			var calls, writes atomic.Int32
			started := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if name == "total timeout" {
					select {
					case <-time.After(120 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				if r.Header.Get("Authorization") != "Bearer "+secret {
					t.Error("credential missing")
				}
				if name == "timeout" || name == "signal" {
					select {
					case started <- struct{}{}:
					default:
					}
					<-r.Context().Done()
					return
				}
				if name == "denied" || name == "unauthorized" {
					c := 403
					if name == "unauthorized" {
						c = 401
					}
					http.Error(w, secret, c)
					return
				}
				if strings.Contains(r.URL.Path, "/metrics/") {
					if name == "missing metric" || name == "missing metric and window" {
						w.WriteHeader(404)
						return
					}
					parts := strings.Split(r.URL.Path, "/")
					key := parts[len(parts)-3]
					json.MarshalWrite(w, growth.Metric{AttributionWindow: int64(24 * time.Hour), Status: "published", WorkspaceID: "ws1", Key: key, Version: 1, Hash: g.PrimaryMetric.Ref.DefinitionHash})
					return
				}
				if name == "missing report" {
					w.WriteHeader(404)
					return
				}
				ws := "ws1"
				if name == "report scope" {
					ws = "other"
				}
				json.MarshalWrite(w, map[string]any{"workspaceId": ws, "environmentId": "env1", "report": report})
			}))
			defer srv.Close()
			gp, mp := writeCheckGoalFile(t, g), writeCheckGoalFile(t, m)
			for _, target := range []string{"goal", "manifest"} {
				if strings.HasPrefix(name, target+" ") && strings.Contains(" unknown duplicate nested duplicate escaped duplicate trailing null utf8 oversized symlink fifo directory absent ", " "+strings.TrimPrefix(name, target+" ")+" ") {
					noHTTP = true
					diagnostic = "invalid goal file or JSON\n"
					path := &gp
					limit := growth.MaxGoalBytes
					if target == "manifest" {
						path = &mp
						limit = growth.MaxMonitorManifestBytes
						diagnostic = "invalid monitor manifest\n"
					}
					bad := strings.TrimPrefix(name, target+" ")
					raw := ""
					switch bad {
					case "unknown":
						raw = `{"secret":"` + secret + `"}`
					case "duplicate":
						raw = `{"workspaceId":"ws1","workspaceId":"ws1"}`
					case "escaped duplicate":
						raw = `{"workspaceId":"ws1","\u0077orkspaceId":"ws1"}`
					case "nested duplicate":
						raw = `{"primaryMetric":{"key":"a","key":"b"}}`
					case "trailing":
						raw = `{} {}`
					case "null":
						raw = `null`
					case "utf8":
						raw = "{\"workspaceId\":\"\xff\"}"
					case "oversized":
						raw = strings.Repeat(" ", limit+1)
					case "directory":
						*path = t.TempDir()
					case "absent":
						*path = filepath.Join(t.TempDir(), "missing")
					case "symlink":
						link := filepath.Join(t.TempDir(), "link")
						if err := os.Symlink(*path, link); err != nil {
							t.Fatal(err)
						}
						*path = link
					case "fifo":
						*path = filepath.Join(t.TempDir(), "fifo")
						if err := syscall.Mkfifo(*path, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if raw != "" {
						if err := os.WriteFile(*path, []byte(raw), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			args := []string{"check-goal", "--goal", gp, "--manifest", mp, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1", "--token-env", "GRAYLING_GROWTH_TOKEN", "--timeout", "10s"}
			if name == "operator scope" {
				args[10] = "other"
			}
			if name == "timeout" {
				args[len(args)-1] = "30ms"
			}
			if name == "total timeout" {
				args[len(args)-1] = "200ms"
			}
			switch name {
			case "missing goal flag":
				args = append(args[:1], args[3:]...)
			case "missing manifest flag":
				args = append(args[:3], args[5:]...)
			case "invalid origin":
				args[6] = srv.URL + "/untrusted"
			case "invalid timeout":
				args[len(args)-1] = "31s"
			case "extra argument":
				args = append(args, "secret")
			case "help":
				args = []string{"check-goal", "--help"}
			}
			if strings.HasPrefix(name, "duplicate flag ") {
				noHTTP = true
				diagnostic = "invalid flags; use --help\n"
				flag := strings.TrimPrefix(name, "duplicate flag ")
				for i := 1; i < len(args); i += 2 {
					if args[i] == "--"+flag {
						args = append(args, "-"+flag+"="+args[i+1])
						break
					}
				}
			}
			if noHTTP && name != "help" {
				code = 2
				if diagnostic == "" {
					diagnostic = "invalid or inconsistent goal check inputs\n"
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			token := secret
			if name == "missing token" {
				token = ""
			}
			cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN="+token)
			var out, diag bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &diag
			if name == "output failure" {
				full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer full.Close()
				cmd.Stdout = full
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if name == "signal" {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("no request")
				}
				if err := cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			}
			err := cmd.Wait()
			gotCode := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				gotCode = ee.ExitCode()
			}
			if gotCode != code || diag.String() != diagnostic {
				t.Fatalf("exit %d want %d, diagnostic %q want %q, stdout %s", gotCode, code, diag.String(), diagnostic, out.String())
			}
			if writes.Load() != 0 || (noHTTP && calls.Load() != 0) {
				t.Fatalf("unexpected requests: %d writes %d", calls.Load(), writes.Load())
			}
			if strings.Contains(out.String()+diag.String(), secret) {
				t.Fatal("secret leaked")
			}
			if name == "help" {
				if out.String() != usage {
					t.Fatal("help changed")
				}
				return
			}
			if code != 0 && code != 3 {
				if out.Len() != 0 {
					t.Fatal("partial failure artifact")
				}
				return
			}
			var result growth.GoalCheck
			if err := json.Unmarshal(out.Bytes(), &result, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal(err)
			}
			fp, _ := growth.GoalFingerprint(g)
			if result.SchemaVersion != growth.GoalCheckSchemaVersion || result.Status != status || result.GoalID != g.ID || result.GoalVersion != g.Version || result.GoalFingerprint != fp || result.PublicationAuthorized || result.Observation == nil || result.Observation.PublicationAuthorized {
				t.Fatalf("invalid result %+v", result)
			}
			if blocker != "" {
				found := false
				for _, b := range result.Blockers {
					found = found || b.Code == blocker
				}
				if !found {
					t.Fatalf("missing blocker %s: %+v", blocker, result.Blockers)
				}
			}
			if name != "missing report" && result.Observation.ServerVerdict != report.Decision.Verdict {
				t.Fatal("server verdict changed")
			}
			if (name == "start mismatch" || name == "end mismatch") && result.Observation.Status != growth.Ready {
				t.Fatal("observation mutated")
			}
		})
	}
}

func TestCheckGoalOutput(t *testing.T) {
	for _, status := range []growth.Status{growth.Ready, growth.Hold, growth.NeedsData} {
		t.Run(string(status), func(t *testing.T) {
			g, m, report := checkGoalFixture(t)
			if status == growth.Hold {
				g.BaselineWindow.End = g.BaselineWindow.End.Add(time.Hour)
			}
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				if status == growth.NeedsData {
					w.WriteHeader(404)
					return
				}
				if strings.Contains(r.URL.Path, "/metrics/") {
					key := "activation"
					if strings.Contains(r.URL.Path, "retention") {
						key = "retention"
					}
					json.MarshalWrite(w, growth.Metric{AttributionWindow: int64(24 * time.Hour), Status: "published", WorkspaceID: "ws1", Key: key, Version: 1, Hash: m.PrimaryMetric.DefinitionHash})
					return
				}
				json.MarshalWrite(w, map[string]any{"workspaceId": "ws1", "environmentId": "env1", "report": report})
			}))
			defer srv.Close()
			t.Setenv("GRAYLING_GROWTH_TOKEN", secret)
			args := []string{"check-goal", "--goal", writeCheckGoalFile(t, g), "--manifest", writeCheckGoalFile(t, m), "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1"}
			var out, diag bytes.Buffer
			wantCode := 0
			if status != growth.Ready {
				wantCode = 3
			}
			if code := run(t.Context(), args, &out, &diag); code != wantCode || diag.Len() != 0 {
				t.Fatalf("control %d %s", code, diag.String())
			}
			checkArtifactOutput(t, out.Bytes(), wantCode, func(out, diag io.Writer) int { return run(t.Context(), args, out, diag) })
			if writes.Load() != 0 {
				t.Fatal("HTTP write")
			}
		})
	}
}
