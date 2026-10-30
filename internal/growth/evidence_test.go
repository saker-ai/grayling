package growth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const hashA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hashB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fixture() (Metric, Report, Query) {
	ref := MetricRef{Key: "activation", Version: 1, DefinitionHash: hashA}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	window := Window{Version: "utc-exposure-v1", Start: start, End: start.Add(time.Hour), Watermark: start.Add(2 * time.Hour), Complete: true, Recomputable: true, Backend: "postgresql-raw"}
	report := Report{PrimaryAttributionWindow: int64(24 * time.Hour), ExperimentID: "exp1", Epoch: 1, EvidenceVersion: hashB, GeneratedAt: start.Add(3 * time.Hour), RegistrationVersion: hashA, AnalysisMethod: "two-proportion-z", ExposureMode: "onRender", Window: window, Healthy: true, PrimaryMetricRef: &ref,
		Diagnostics: &Diagnostics{ExperimentID: "exp1", Epoch: 1, GeneratedAt: start.Add(3 * time.Hour), Window: window},
		PerVariant:  []Variant{{Variant: "control", ExposedUnits: ptr(int64(100)), RandomizedUnits: ptr(int64(100)), Exposures: ptr(int64(100)), ConvertedUnits: ptr(int64(20)), PrimaryValue: ptr(0.2)}, {Variant: "treatment", ExposedUnits: ptr(int64(100)), RandomizedUnits: ptr(int64(100)), Exposures: ptr(int64(100)), ConvertedUnits: ptr(int64(30)), PrimaryValue: ptr(0.3)}},
		Decision:    Decision{Verdict: "ship", Reasons: []string{"server fixed-sample result"}}}
	return Metric{AttributionWindow: int64(24 * time.Hour), Status: "published", WorkspaceID: "ws1", Key: ref.Key, Version: ref.Version, Hash: ref.DefinitionHash}, report, Query{Metrics: []MetricRef{ref}, Experiments: []ExperimentRef{{ID: "exp1", Epoch: 1, EvidenceVersion: hashB}}}
}

func readerFor(t *testing.T, url string, mutate func(*Config)) *Reader {
	t.Helper()
	c := Config{BaseURL: url, WorkspaceID: "ws1", EnvironmentID: "env1", BearerToken: "secret-token"}
	if mutate != nil {
		mutate(&c)
	}
	r, err := NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestEvidenceFixtures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Metric, *Report, *string, *string)
		status Status
		err    error
	}{
		{name: "healthy", status: Ready},
		{name: "absent aggregate", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PerVariant[0].PrimaryValue = nil }, status: Hold},
		{name: "absent count", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PerVariant[0].RandomizedUnits = nil }, status: Hold},
		{name: "partial variants", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PerVariant = p.PerVariant[:1] }, status: Hold},
		{name: "primary error", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PrimaryError = "insufficient samples" }, status: Hold},
		{name: "metric workspace", mutate: func(m *Metric, _ *Report, _ *string, _ *string) { m.WorkspaceID = "other" }, err: ErrMismatch},
		{name: "metric key", mutate: func(m *Metric, _ *Report, _ *string, _ *string) { m.Key = "other" }, err: ErrMismatch},
		{name: "metric version", mutate: func(m *Metric, _ *Report, _ *string, _ *string) { m.Version = 2 }, err: ErrMismatch},
		{name: "metric hash", mutate: func(m *Metric, _ *Report, _ *string, _ *string) { m.Hash = hashB }, err: ErrMismatch},
		{name: "report workspace", mutate: func(_ *Metric, _ *Report, w *string, _ *string) { *w = "other" }, err: ErrMismatch},
		{name: "report environment", mutate: func(_ *Metric, _ *Report, _ *string, e *string) { *e = "other" }, err: ErrMismatch},
		{name: "experiment", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.ExperimentID = "other" }, err: ErrMismatch},
		{name: "epoch", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Epoch = 2 }, err: ErrMismatch},
		{name: "evidence version", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.EvidenceVersion = hashA }, err: ErrMismatch},
		{name: "empty evidence version", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.EvidenceVersion = "" }, err: ErrMismatch},
		{name: "primary hash", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PrimaryMetricRef.DefinitionHash = hashB }, err: ErrMismatch},
		{name: "primary key", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PrimaryMetricRef.Key = "other" }, err: ErrMismatch},
		{name: "primary version", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PrimaryMetricRef.Version = 2 }, err: ErrMismatch},
		{name: "unrequested guardrail", mutate: func(_ *Metric, p *Report, _ *string, _ *string) {
			p.GuardrailMetricRefs = []MetricRef{{Key: "latency", Version: 1, DefinitionHash: hashA}}
		}, err: ErrMismatch},
		{name: "diagnostic experiment", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Diagnostics.ExperimentID = "other" }, err: ErrMismatch},
		{name: "diagnostic epoch", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Diagnostics.Epoch = 2 }, err: ErrMismatch},
		{name: "unhealthy", mutate: func(_ *Metric, p *Report, _ *string, _ *string) {
			p.Healthy = false
			p.HealthIssues = []string{"SRM alarm"}
		}, status: Hold},
		{name: "incomplete", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Window.Complete = false }, status: Hold},
		{name: "not recomputable", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Window.Recomputable = false }, status: Hold},
		{name: "late watermark", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Window.Watermark = p.Window.Start }, status: Hold},
		{name: "window issue", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Window.Issues = []string{"raw event loss"} }, status: Hold},
		{name: "no diagnostics", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Diagnostics = nil }, status: Hold},
		{name: "diagnostic issue", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Diagnostics.Issues = []string{"contamination"} }, status: Hold},
		{name: "diagnostic window", mutate: func(_ *Metric, p *Report, _ *string, _ *string) {
			p.Diagnostics.Window.End = p.Window.End.Add(time.Hour)
		}, status: Hold},
		{name: "missing metric ref", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.PrimaryMetricRef = nil }, status: Hold},
		{name: "no generation time", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.GeneratedAt = time.Time{} }, status: Hold},
		{name: "server hold", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Decision.Verdict = "hold" }, status: Hold},
		{name: "unknown decision", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Decision.Verdict = "winner" }, status: Hold},
		{name: "rollback", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Decision.Verdict = "rollback" }, status: Ready},
		{name: "continue", mutate: func(_ *Metric, p *Report, _ *string, _ *string) { p.Decision.Verdict = "continue" }, status: Ready},
	}
	for _, verdict := range []string{"rollback", "continue", "ship"} {
		tests = append(tests, struct {
			name   string
			mutate func(*Metric, *Report, *string, *string)
			status Status
			err    error
		}{name: "healthy breach " + verdict, status: Hold, mutate: func(_ *Metric, p *Report, _ *string, _ *string) {
			p.Decision.Verdict = verdict
			p.Decision.GuardrailBreaches = append(p.Decision.GuardrailBreaches, struct {
				Metric string `json:"metric"`
				Detail string `json:"detail"`
			}{})
		}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metric, report, query := fixture()
			ws, env := "ws1", "env1"
			if tt.mutate != nil {
				tt.mutate(&metric, &report, &ws, &env)
			}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer secret-token" {
					t.Error("request contract")
				}
				switch r.URL.RequestURI() {
				case "/api/v1/admin/workspaces/ws1/metrics/activation/versions/1":
					_ = json.NewEncoder(w).Encode(metric)
				case "/api/v1/admin/experiments/exp1/report/export?format=json":
					_ = json.NewEncoder(w).Encode(map[string]any{"workspaceId": ws, "environmentId": env, "report": report})
				default:
					t.Error("unexpected operation")
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			got, err := readerFor(t, srv.URL, nil).Read(context.Background(), query)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				if got != nil {
					t.Fatal("returned partial evidence")
				}
				return
			}
			if got.Status != tt.status || len(got.Evidence) != 1 || calls != 2 {
				t.Fatalf("envelope = %+v, calls %d", got, calls)
			}
			p := got.Evidence[0]
			if p.Status != tt.status {
				t.Fatalf("report status = %s, want %s", p.Status, tt.status)
			}
			if p.SourcePath != "/api/v1/admin/experiments/exp1/report/export?format=json" || p.Report.Decision.Verdict != report.Decision.Verdict || got.Metrics[0].SourcePath == "" {
				t.Fatal("lost provenance or server semantics")
			}
			if tt.name == "absent aggregate" {
				b, _ := json.Marshal(p.Report.PerVariant[0])
				if strings.Contains(string(b), "primaryValue") {
					t.Fatal("absent statistic was synthesized")
				}
			} else if p.Report.PerVariant[0].PrimaryValue == nil || *p.Report.PerVariant[0].PrimaryValue != 0.2 {
				t.Fatal("changed aggregate")
			}

		})
	}
}

func TestTransportAndMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		want       error
		missing    bool
	}{
		{"authorization", "secret-token private body", 401, ErrAuthorization, false},
		{"forbidden", "secret-token private body", 403, ErrAuthorization, false},
		{"missing", "private body", 404, nil, true},
		{"server failure", "secret-token private body", 500, ErrResponse, false},
		{"redirect", "secret-token private body", 302, ErrResponse, false},
		{"invalid JSON", `{"private":"secret-token"`, 200, ErrResponse, false},
		{"wrong shape", `[]`, 200, ErrResponse, false},
		{"null metric", `null`, 200, ErrResponse, false},
		{"trailing JSON", `{} {}`, 200, ErrResponse, false},
		{"duplicate JSON keys", `{"key":"activation","key":"other"}`, 200, ErrResponse, false},
		{"oversized", strings.Repeat("x", 1025), 200, ErrResponse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet {
					t.Error("write request")
				}
				w.Header().Set("Location", "/leak?secret-token")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, _, query := fixture()
			got, err := readerFor(t, srv.URL, func(c *Config) { c.MaxResponseBytes = 1024 }).Read(context.Background(), query)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			if err != nil && (strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), tc.body) || strings.Contains(err.Error(), srv.URL)) {
				t.Fatal("error leaks sensitive input")
			}
			if tc.missing {
				if got.Status != NeedsData || len(got.MissingPaths) != 2 || len(got.Evidence) != 0 {
					t.Fatalf("missing = %+v", got)
				}
			} else if calls != 1 {
				t.Fatal("retried or followed redirect")
			}
		})
	}
}

func TestAllowlistAndMissingReport(t *testing.T) {
	metric, report, query := fixture()
	for _, missing := range []bool{false, true} {
		t.Run(strconvBool(missing), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/metrics/") {
					b, _ := json.Marshal(metric)
					var fields map[string]any
					_ = json.Unmarshal(b, &fields)
					fields["description"] = "private identity secret-token"
					fields["rawUserId"] = "private identity"
					_ = json.NewEncoder(w).Encode(fields)
					return
				}
				var p any = report
				if missing {
					p = nil
				} else {
					b, _ := json.Marshal(report)
					var fields map[string]any
					_ = json.Unmarshal(b, &fields)
					fields["rawEvents"] = []string{"private identity"}
					p = fields
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"workspaceId": "ws1", "environmentId": "env1", "report": p, "secret": "secret-token"})
			}))
			defer srv.Close()
			got, err := readerFor(t, srv.URL, nil).Read(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "private identity") || strings.Contains(string(b), "secret-token") {
				t.Fatal("unknown fields escaped allowlist")
			}
			if missing && (got.Status != NeedsData || len(got.MissingPaths) != 1) {
				t.Fatal("null report not missing")
			}
		})
	}
}
func strconvBool(b bool) string {
	if b {
		return "missing"
	}
	return "allowlist"
}

func TestCancellationTimeoutAndInputValidation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() }))
	defer srv.Close()
	_, _, query := fixture()
	r := readerFor(t, srv.URL, func(c *Config) { c.Timeout = 20 * time.Millisecond })
	if _, err := r.Read(context.Background(), query); !errors.Is(err, ErrTransport) {
		t.Fatalf("timeout %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Read(ctx, query); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	before := calls.Load()
	for _, bad := range []Query{
		{Metrics: []MetricRef{{Key: "../escape", Version: 1, DefinitionHash: hashA}}},
		{Metrics: []MetricRef{{Key: "activation", Version: 0, DefinitionHash: hashA}}},
		{Metrics: []MetricRef{{Key: "activation", Version: 1, DefinitionHash: "invalid"}}},
		{Metrics: append(query.Metrics, query.Metrics...)},
		{Experiments: []ExperimentRef{{ID: "../escape", Epoch: 1}}},
		{Experiments: []ExperimentRef{{ID: "exp1", Epoch: 0}}},
		{Experiments: []ExperimentRef{{ID: "exp1", Epoch: 1, EvidenceVersion: "invalid"}}},
		{Experiments: append(query.Experiments, query.Experiments...)},
	} {
		if _, err := r.Read(context.Background(), bad); !errors.Is(err, ErrConfig) {
			t.Fatalf("invalid reference %v", err)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input made request")
	}
	got, err := r.Read(context.Background(), Query{Experiments: query.Experiments})
	if err != nil || got.Status != NeedsData {
		t.Fatal("empty query")
	}
	for _, origin := range []string{"https://user:secret-token@example.com", "https://example.com/path", "https://example.com?token=secret-token", "https://example.com#fragment", "file:///tmp/evidence"} {
		if _, err := NewReader(Config{BaseURL: origin, WorkspaceID: "ws1", EnvironmentID: "env1", BearerToken: "secret-token"}); !errors.Is(err, ErrConfig) {
			t.Fatalf("bad origin %v", err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestCancellationDuringResponseBody(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"workspaceId":"ws1",`))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	reader := readerFor(t, srv.URL, nil)
	_, _, query := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := reader.Read(ctx, query); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("body cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("body read ignored cancellation")
	}
}
