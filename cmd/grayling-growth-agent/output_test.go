package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

type injectedOutputWriter struct {
	bytes.Buffer
	count func(int) int
	err   error
	calls int
}

func (w *injectedOutputWriter) Write(p []byte) (int, error) {
	w.calls++
	n := w.count(len(p))
	w.Buffer.Write(p[:n])
	return n, w.err
}

func checkArtifactOutput(t *testing.T, expected []byte, successCode int, run func(io.Writer, io.Writer) int, alternatives ...[]byte) {
	t.Helper()
	if len(expected) == 0 || expected[len(expected)-1] != '\n' {
		t.Fatal("expected complete artifact including newline")
	}
	for _, tc := range []struct {
		name  string
		count func(int) int
		err   error
	}{
		{"zero nil", func(int) int { return 0 }, nil},
		{"partial nil", func(n int) int { return n / 2 }, nil},
		{"missing newline nil", func(n int) int { return n - 1 }, nil},
		{"zero error", func(int) int { return 0 }, errors.New(secret)},
		{"partial error", func(n int) int { return n / 2 }, errors.New(secret)},
		{"full error", func(n int) int { return n }, errors.New(secret)},
		{"success", func(n int) int { return n }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &injectedOutputWriter{count: tc.count, err: tc.err}
			var diagnostics bytes.Buffer
			wantCode, wantDiagnostic := 1, "output failed\n"
			if tc.name == "success" {
				wantCode, wantDiagnostic = successCode, ""
			}
			code := run(out, &diagnostics)
			if code != wantCode || diagnostics.String() != wantDiagnostic {
				t.Fatalf("exit %d want %d, diagnostics %q want %q", code, wantCode, diagnostics.String(), wantDiagnostic)
			}
			matches := false
			for _, artifact := range append([][]byte{expected}, alternatives...) {
				matches = matches || bytes.Equal(out.Bytes(), artifact[:tc.count(len(artifact))])
			}
			if out.calls != 1 || !matches {
				t.Fatalf("output changed or retried: calls %d, bytes %q", out.calls, out.Bytes())
			}
		})
	}
}

func TestOfflineLearningAndFingerprintOutput(t *testing.T) {
	p := fixtureProposal(t)
	r := exportedLearningFixture(t)
	p.Hypothesis = r.OriginalHypothesis
	r.ProposalFingerprint, _ = growth.ProposalFingerprint(p)
	write := func(name string, v any) string {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pp, rp := write("proposal.json", p), write("record.json", r)
	verification, err := growth.VerifyLearning(p, r, "ws1", "env1")
	if err != nil {
		t.Fatal(err)
	}
	search, err := growth.SearchLearning([]growth.LearningRecord{r}, growth.LearningSearchQuery{WorkspaceID: "ws1", EnvironmentID: "env1", Text: "literal"})
	if err != nil {
		t.Fatal(err)
	}
	verifiedJSON, err := json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	searchJSON, err := json.Marshal(search)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		args     []string
		expected []byte
	}{
		{"fingerprint", []string{"fingerprint", "--proposal", pp}, []byte(r.ProposalFingerprint + "\n")},
		{"verify-learning", []string{"verify-learning", "--proposal", pp, "--record", rp, "--workspace", "ws1", "--environment", "env1"}, append(verifiedJSON, '\n')},
		{"search-learning", []string{"search-learning", "--record", rp, "--workspace", "ws1", "--environment", "env1", "--text", "literal"}, append(searchJSON, '\n')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkArtifactOutput(t, tc.expected, 0, func(out, diagnostics io.Writer) int {
				return run(t.Context(), tc.args, out, diagnostics)
			})
		})
	}
}

// Use the same pinned report, metric and operator snapshot wire fixtures as the
// binary tests; output failures must never change the read-only HTTP boundary.
func TestReviewPreparationAndObservationOutput(t *testing.T) {
	p := fixtureProposal(t)
	write := func(name string, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pp := write("proposal.json", p)
	settingsPath := writeSettings(t, p)
	settings, err := readSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	m := growth.MonitorManifest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: "exp1", Epoch: 1, RegistrationVersion: p.PrimaryMetric.Ref.DefinitionHash, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []growth.MetricRef{p.Guardrails[0].Ref}}
	mp := write("monitor.json", m)
	rm := growth.RolloutManifest{WorkspaceID: m.WorkspaceID, EnvironmentID: m.EnvironmentID, ExperimentID: m.ExperimentID, Epoch: m.Epoch, RegistrationVersion: m.RegistrationVersion}
	rp := write("rollout.json", rm)
	report := growth.Report{SRM: growth.SRM{Healthy: new(true)}, PrimaryAttributionWindow: int64(24 * time.Hour), ExperimentID: m.ExperimentID, Epoch: m.Epoch, EvidenceVersion: p.EvidenceRefs[0].EvidenceVersion, RegistrationVersion: m.RegistrationVersion, GeneratedAt: p.AnalysisPlan.Provenance.SuppliedAt, AnalysisMethod: p.AnalysisPlan.Method, ExposureMode: "onRender", Window: p.EvidenceRefs[0].Window, Healthy: true, PrimaryMetricRef: &p.PrimaryMetric.Ref, GuardrailMetricRefs: m.Guardrails, Decision: growth.Decision{Verdict: "continue"}}
	report.Diagnostics = &growth.Diagnostics{ExposureMode: "onRender", CrossVariantUnits: new(int64(0)), AssignmentMismatch: new(int64(0)), ExperimentID: m.ExperimentID, Epoch: m.Epoch, GeneratedAt: report.GeneratedAt, Window: report.Window}
	count, value := int64(100), 0.2
	for _, v := range p.Variants {
		report.PerVariant = append(report.PerVariant, growth.Variant{Variant: v.Key, ExposedUnits: &count, RandomizedUnits: &count, Exposures: &count, ConvertedUnits: &count, PrimaryValue: &value})
	}
	var state atomic.Value
	state.Store(growth.Ready)
	var calls, writes atomic.Int32
	t.Setenv("GRAYLING_GROWTH_TOKEN", secret)
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
		switch {
		case strings.Contains(r.URL.Path, "/metrics/"):
			parts := strings.Split(r.URL.Path, "/")
			writeLifecycleMetric(w, "", growth.Metric{WorkspaceID: p.WorkspaceID, Key: parts[len(parts)-3], Version: 1, Hash: p.PrimaryMetric.Ref.DefinitionHash})
		case r.URL.RequestURI() == "/api/v1/admin/experiments/exp1/report/export?format=json":
			current := report
			current.Healthy = status != growth.Hold
			_ = json.MarshalWrite(w, struct {
				WorkspaceID   string        `json:"workspaceId"`
				EnvironmentID string        `json:"environmentId"`
				Report        growth.Report `json:"report"`
			}{p.WorkspaceID, p.EnvironmentID, current})
		case r.URL.Path == "/api/v1/admin/experiments/exp1/bind-check":
			checks := []map[string]any{}
			for _, code := range []string{"state", "published_flag", "draft_clear", "registered_revision", "fallback", "unit", "split", "bandit_rule", "existing_binding"} {
				checks = append(checks, map[string]any{"code": code, "ready": status == growth.Ready || code != "state", "detail": secret})
			}
			_ = json.MarshalWrite(w, map[string]any{"experimentId": "exp1", "registeredRevision": 7, "currentRevision": 7, "ready": status == growth.Ready, "checks": checks})
		case r.URL.Path == "/api/v1/admin/experiments/exp1":
			_ = json.MarshalWrite(w, map[string]any{"id": "exp1", "workspaceId": "ws1", "environmentId": "env1", "epoch": 1, "registrationVersion": m.RegistrationVersion, "flagRevision": 7, "updatedAt": "2026-01-01T00:00:00Z"})
		default:
			t.Error("unexpected evidence path")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	reader, err := growth.NewReader(growth.Config{BaseURL: srv.URL, WorkspaceID: "ws1", EnvironmentID: "env1", BearerToken: secret, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	q, err := proposalQuery(p)
	if err != nil {
		t.Fatal(err)
	}
	// Serialize expected artifacts using the previous output options and append
	// the newline separately in memory, preserving the successful byte contract.
	check := func(name string, args []string, artifact any, code int, options ...json.Options) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var expected bytes.Buffer
			if err := json.MarshalWrite(&expected, artifact, options...); err != nil {
				t.Fatal(err)
			}
			expected.WriteByte('\n')
			args = append(args, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1")
			var alternatives [][]byte
			if name == "prepare/unreviewed" {
				// Unreviewed preparation intentionally retains nondeterministic map
				// ordering. These are the only two encodings of its proportions map.
				forward := []byte(`"proportions":{"control":50,"treatment":50}`)
				reverse := []byte(`"proportions":{"treatment":50,"control":50}`)
				alternatives = append(alternatives, bytes.Replace(expected.Bytes(), forward, reverse, 1), bytes.Replace(expected.Bytes(), reverse, forward, 1))
			}
			checkArtifactOutput(t, expected.Bytes(), code, func(out, diagnostics io.Writer) int {
				before := calls.Load()
				code := run(t.Context(), args, out, diagnostics)
				if calls.Load() <= before || writes.Load() != 0 {
					t.Error("expected fresh GET-only evidence")
				}
				return code
			}, alternatives...)
		})
	}
	for _, status := range []growth.Status{growth.Ready, growth.Hold, growth.NeedsData} {
		state.Store(status)
		envelope, err := reader.Read(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		review := growth.ReviewProposal(p, envelope)
		code := 3
		if status == growth.Ready {
			code = 0
		}
		check("review/"+string(status), []string{"review", "--proposal", pp}, result{EvidenceStatus: envelope.Status, Review: review}, code)
		monitor, err := reader.Monitor(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		check("monitor/"+string(status), []string{"monitor", "--manifest", mp}, monitor, code)
		rollout, err := reader.Rollout(t.Context(), rm)
		if err != nil {
			t.Fatal(err)
		}
		rolloutCode := 0
		if status == growth.NeedsData {
			rolloutCode = 3
		}
		check("rollout/"+string(status), []string{"rollout", "--manifest", rp}, rollout, rolloutCode)
		binding, err := reader.BindCheck(t.Context(), rm)
		if err != nil {
			t.Fatal(err)
		}
		check("bind-check/"+string(status), []string{"bind-check", "--manifest", rp}, binding, code)
		if status != growth.Ready {
			continue
		}
		fp, err := growth.ProposalFingerprint(p)
		if err != nil {
			t.Fatal(err)
		}
		approval, err := growth.RecordReview(p, envelope, growth.ReviewInputs{ExpectedProposalFingerprint: fp, Reviewer: "operator", ReviewedAt: p.AnalysisPlan.Provenance.SuppliedAt, Decision: growth.ReviewApprove, Rationale: "Ready for preparation"})
		if err != nil {
			t.Fatal(err)
		}
		approvalPath := write("approval.json", approval)
		prepared, err := growth.CompileCreateRequest(p, envelope, *settings)
		if err != nil {
			t.Fatal(err)
		}
		check("prepare/unreviewed", []string{"prepare", "--proposal", pp, "--settings", settingsPath}, prepared, 0, json.Deterministic(false))
		reviewed, err := growth.CompileReviewedCreateRequest(p, *approval, "ws1", "env1", envelope, *settings)
		if err != nil {
			t.Fatal(err)
		}
		check("prepare/reviewed", []string{"prepare", "--proposal", pp, "--settings", settingsPath, "--review-record", approvalPath}, reviewed, 0, json.Deterministic(true))
	}
}
