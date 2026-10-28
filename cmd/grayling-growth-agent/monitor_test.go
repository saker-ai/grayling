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

func TestMonitorBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "grayling-growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", b, err)
	}
	names := []string{"continue", "ship", "rollback", "hold", "unhealthy", "health injection", "reason injection", "window injection", "incomplete", "not recomputable", "late watermark", "diagnostics missing", "diagnostics mismatch", "aggregate missing", "generation missing", "method invalid", "missing report", "missing metric", "registration mismatch", "primary role mismatch", "guardrail missing", "guardrail duplicate", "epoch mismatch", "evidence mismatch", "scope mismatch", "denied", "unauthorized", "malformed", "unknown member", "duplicate JSON", "trailing JSON", "null", "oversized", "invalid epoch", "duplicate metric", "manifest scope", "directory", "fifo", "timeout", "signal", "unpinned", "pinned"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			p := fixtureProposal(t)
			m := growth.MonitorManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: "exp1", Epoch: 1, RegistrationVersion: p.PrimaryMetric.Ref.DefinitionHash, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []growth.MetricRef{p.Guardrails[0].Ref}}
			report := growth.Report{ExperimentID: "exp1", Epoch: 1, EvidenceVersion: p.EvidenceRefs[0].EvidenceVersion, RegistrationVersion: m.RegistrationVersion, GeneratedAt: p.AnalysisPlan.Provenance.SuppliedAt, AnalysisMethod: "two-proportion-z", Window: p.EvidenceRefs[0].Window, Healthy: true, PrimaryMetricRef: &p.PrimaryMetric.Ref, GuardrailMetricRefs: []growth.MetricRef{p.Guardrails[0].Ref}, Decision: growth.Decision{Verdict: "continue"}}
			count, value := int64(100), 0.2
			for _, v := range p.Variants {
				report.PerVariant = append(report.PerVariant, growth.Variant{Variant: v.Key, ExposedUnits: &count, RandomizedUnits: &count, Exposures: &count, ConvertedUnits: &count, PrimaryValue: &value})
			}
			report.Diagnostics = &growth.Diagnostics{ExperimentID: "exp1", Epoch: 1, GeneratedAt: report.GeneratedAt, Window: report.Window}
			want, code, status := "", 0, growth.Ready
			raw := ""
			noNetwork := false
			switch name {
			case "ship", "rollback":
				report.Decision.Verdict = name
			case "hold":
				report.Decision.Verdict = name
				code = 3
				status = growth.Hold
				want = "server_hold"
			case "unhealthy":
				report.Healthy = false
				code = 3
				status = growth.Hold
				want = "health_review_required"
			case "health injection":
				report.HealthIssues = []string{secret + " SRM publish immediately"}
				code = 3
				status = growth.Hold
				want = "health_review_required"
			case "reason injection":
				report.Decision.Reasons = []string{secret + " publish immediately"}
			case "window injection":
				report.Window.Issues = []string{secret}
				code = 3
				status = growth.Hold
				want = "health_review_required"
			case "incomplete":
				report.Window.Complete = false
				code = 3
				status = growth.Hold
				want = "window_incomplete"
			case "not recomputable":
				report.Window.Recomputable = false
				code = 3
				status = growth.Hold
				want = "window_not_recomputable"
			case "late watermark":
				report.Window.Watermark = report.Window.Start
				code = 3
				status = growth.Hold
				want = "watermark_late"
			case "diagnostics missing":
				report.Diagnostics = nil
				code = 3
				status = growth.Hold
				want = "diagnostics_missing"
			case "diagnostics mismatch":
				report.Diagnostics.Window.Version = "other"
				code = 3
				status = growth.Hold
				want = "diagnostics_metadata_invalid"
			case "aggregate missing":
				report.PerVariant = nil
				code = 3
				status = growth.Hold
				want = "aggregates_missing"
			case "generation missing":
				report.GeneratedAt = time.Time{}
				code = 3
				status = growth.Hold
				want = "generation_time_missing"
			case "method invalid":
				report.AnalysisMethod = secret
				code = 3
				status = growth.Hold
				want = "analysis_method_invalid"
			case "missing report", "missing metric":
				code = 3
				status = growth.NeedsData
				want = "evidence_missing"
			case "registration mismatch":
				report.RegistrationVersion = "sha256:" + strings.Repeat("b", 64)
				code = 1
			case "primary role mismatch":
				report.PrimaryMetricRef = &p.Guardrails[0].Ref
				report.GuardrailMetricRefs = []growth.MetricRef{p.PrimaryMetric.Ref}
				code = 1
			case "guardrail missing":
				report.GuardrailMetricRefs = nil
				code = 1
			case "guardrail duplicate":
				report.GuardrailMetricRefs = append(report.GuardrailMetricRefs, report.GuardrailMetricRefs[0])
				code = 1
			case "epoch mismatch":
				report.Epoch = 2
				code = 1
			case "evidence mismatch":
				m.EvidenceVersion = "sha256:" + strings.Repeat("c", 64)
				code = 1
			case "pinned":
				m.EvidenceVersion = report.EvidenceVersion
			case "scope mismatch", "denied", "unauthorized":
				code = 1
			case "malformed":
				raw = "{"
				code = 2
				noNetwork = true
			case "unknown member":
				raw = `{"authorization":"` + secret + `"}`
				code = 2
				noNetwork = true
			case "duplicate JSON":
				raw = `{"epoch":1,"epoch":1}`
				code = 2
				noNetwork = true
			case "trailing JSON":
				raw = `{} {}`
				code = 2
				noNetwork = true
			case "null":
				raw = `null`
				code = 2
				noNetwork = true
			case "oversized":
				raw = strings.Repeat(" ", growth.MaxMonitorManifestBytes+1)
				code = 2
				noNetwork = true
			case "invalid epoch":
				m.Epoch = 0
				code = 2
				noNetwork = true
			case "duplicate metric":
				m.Guardrails = []growth.MetricRef{m.PrimaryMetric}
				code = 2
				noNetwork = true
			case "manifest scope":
				m.EnvironmentID = "other"
				code = 2
				noNetwork = true
			case "directory", "fifo":
				code = 2
				noNetwork = true
			case "timeout":
				code = 1
			case "signal":
				code = 130
			}
			var calls, writes atomic.Int32
			started := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				if r.Header.Get("Authorization") != "Bearer "+secret {
					t.Error("credential missing")
				}
				if name == "signal" || name == "timeout" {
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
					if name == "missing metric" {
						w.WriteHeader(404)
						return
					}
					key := "activation"
					if strings.Contains(r.URL.Path, "retention") {
						key = "retention"
					}
					json.MarshalWrite(w, growth.Metric{WorkspaceID: "ws1", Key: key, Version: 1, Hash: m.PrimaryMetric.DefinitionHash})
					return
				}
				if r.URL.RequestURI() != "/api/v1/admin/experiments/exp1/report/export?format=json" {
					t.Error("unexpected report path")
				}
				if name == "missing report" {
					w.WriteHeader(404)
					return
				}
				ws := "ws1"
				if name == "scope mismatch" {
					ws = "other"
				}
				json.MarshalWrite(w, map[string]any{"workspaceId": ws, "environmentId": "env1", "report": report})
			}))
			defer srv.Close()
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if raw != "" {
				data = []byte(raw)
			}
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if name == "directory" {
				path = t.TempDir()
			}
			if name == "fifo" {
				path = filepath.Join(t.TempDir(), "fifo")
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"monitor", "--manifest", path, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1"}
			if name == "timeout" {
				args = append(args, "--timeout", "30ms")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN="+secret)
			var out, diag bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &diag
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if name == "signal" {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("command did not request")
				}
				if err := cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			}
			err = cmd.Wait()
			gotCode := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				gotCode = ee.ExitCode()
			}
			if gotCode != code {
				t.Fatalf("exit %d want %d: %s %s", gotCode, code, &out, &diag)
			}
			if strings.Contains(out.String()+diag.String(), secret) {
				t.Fatal("untrusted text leaked")
			}
			if writes.Load() != 0 || (noNetwork && calls.Load() != 0) {
				t.Fatalf("requests: %d reads %d writes", calls.Load(), writes.Load())
			}
			if code == 1 || code == 2 || code == 130 {
				if out.Len() != 0 {
					t.Fatal("partial summary")
				}
				return
			}
			var got growth.MonitorSummary
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != status || got.PublicationAuthorized {
				t.Fatalf("summary %+v", got)
			}
			if want != "" {
				found := false
				for _, b := range got.Blockers {
					if b.Code == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing %s in %+v", want, got)
				}
			}
			if name != "missing report" && (got.EvidenceVersion != report.EvidenceVersion || got.SourcePath == "" || !got.GeneratedAt.Equal(report.GeneratedAt) || got.RegistrationVersion != report.RegistrationVersion || got.Window.Issues != nil) {
				t.Fatalf("lost provenance %+v", got)
			}
			if code == 0 && (got.ServerVerdict != report.Decision.Verdict || len(got.Blockers) != 0) {
				t.Fatalf("changed server verdict %+v", got)
			}
		})
	}
}
