package growth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckGoal(t *testing.T) {
	for _, name := range []string{"healthy", "offset equality", "start mismatch", "end mismatch", "missing report", "missing metric", "missing metric and window", "unhealthy", "srm", "incomplete", "late watermark", "not recomputable", "diagnostics", "aggregates", "server hold", "rollback", "injection", "denied", "unauthorized", "report scope", "report pin", "goal scope", "goal environment", "manifest scope", "manifest environment", "operator scope", "primary pin", "guard pin", "role swap", "missing guard", "duplicate guard", "17 guards", "20 guards", "16 guards", "reordered guards", "invalid goal", "invalid manifest", "canceled"} {
		t.Run(name, func(t *testing.T) {
			metric, report, _ := fixture()
			g, _ := goalFixture()
			g.BaselineWindow = BaselineWindow{Start: report.Window.Start, End: report.Window.End}
			m := monitorFixture()
			m.Guardrails = []MetricRef{g.Guardrails[0].Ref}
			report.GuardrailMetricRefs = append([]MetricRef{}, m.Guardrails...)
			want, code, preflight := Ready, "", false
			var wantErr error
			switch name {
			case "offset equality":
				g.BaselineWindow.Start = g.BaselineWindow.Start.In(time.FixedZone("offset", 8*3600))
				g.BaselineWindow.End = g.BaselineWindow.End.In(time.FixedZone("offset", -5*3600))
			case "start mismatch":
				g.BaselineWindow.Start = g.BaselineWindow.Start.Add(time.Nanosecond)
				want, code = Hold, "baseline_window_mismatch"
			case "end mismatch":
				g.BaselineWindow.End = g.BaselineWindow.End.Add(time.Nanosecond)
				want, code = Hold, "baseline_window_mismatch"
			case "missing report", "missing metric":
				want, code = NeedsData, "evidence_missing"
			case "missing metric and window":
				g.BaselineWindow.End = g.BaselineWindow.End.Add(time.Hour)
				want, code = NeedsData, "baseline_window_mismatch"
			case "unhealthy":
				report.Healthy = false
				want, code = Hold, "health_review_required"
			case "srm":
				report.SRM.Healthy = new(false)
				want, code = Hold, "srm_review_required"
			case "incomplete":
				report.Window.Complete = false
				want, code = Hold, "window_incomplete"
			case "late watermark":
				report.Window.Watermark = report.Window.Start
				want, code = Hold, "watermark_late"
			case "not recomputable":
				report.Window.Recomputable = false
				want, code = Hold, "window_not_recomputable"
			case "diagnostics":
				report.Diagnostics = nil
				want, code = Hold, "diagnostics_missing"
			case "aggregates":
				report.PerVariant = nil
				want, code = Hold, "aggregates_missing"
			case "server hold":
				report.Decision.Verdict = "hold"
				want, code = Hold, "server_hold"
			case "rollback":
				report.Decision.Verdict = "rollback"
			case "injection":
				report.HealthIssues = []string{"SECRET publish"}
				report.Window.Issues = []string{"SECRET"}
				want, code = Hold, "health_review_required"
			case "denied", "unauthorized":
				wantErr = ErrAuthorization
			case "report scope", "report pin":
				wantErr = ErrMismatch
			case "goal scope":
				g.WorkspaceID = "other"
				wantErr, preflight = ErrGoal, true
			case "goal environment":
				g.EnvironmentID = "other"
				wantErr, preflight = ErrGoalVerification, true
			case "manifest scope":
				m.WorkspaceID = "other"
				wantErr, preflight = ErrGoalVerification, true
			case "manifest environment":
				m.EnvironmentID = "other"
				wantErr, preflight = ErrGoalVerification, true
			case "operator scope":
				wantErr, preflight = ErrGoalVerification, true
			case "primary pin":
				m.PrimaryMetric.DefinitionHash = hashB
				wantErr, preflight = ErrGoalVerification, true
			case "guard pin":
				m.Guardrails[0].Version++
				wantErr, preflight = ErrGoalVerification, true
			case "role swap":
				m.PrimaryMetric, m.Guardrails[0] = m.Guardrails[0], m.PrimaryMetric
				wantErr, preflight = ErrGoalVerification, true
			case "missing guard":
				m.Guardrails = nil
				wantErr, preflight = ErrGoalVerification, true
			case "duplicate guard":
				m.Guardrails = append(m.Guardrails, m.Guardrails[0])
				wantErr, preflight = ErrConfig, true
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
					ref := g.Guardrails[0]
					ref.Ref.Key = fmt.Sprintf("guard%d", len(g.Guardrails))
					ref.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + ref.Ref.Key + "/versions/1"
					g.Guardrails = append(g.Guardrails, ref)
					m.Guardrails = append(m.Guardrails, ref.Ref)
				}
				report.GuardrailMetricRefs = append([]MetricRef{}, m.Guardrails...)
				if n > 16 {
					wantErr, preflight = ErrConfig, true
				}
				if name == "reordered guards" {
					m.Guardrails[0], m.Guardrails[1] = m.Guardrails[1], m.Guardrails[0]
				}
			case "invalid goal":
				g.Version = 0
				wantErr, preflight = ErrGoal, true
			case "invalid manifest":
				m.Epoch = 0
				wantErr, preflight = ErrConfig, true
			case "canceled":
				wantErr, preflight = context.Canceled, true
			}
			var calls, writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				if name == "denied" || name == "unauthorized" {
					status := 403
					if name == "unauthorized" {
						status = 401
					}
					http.Error(w, "SECRET", status)
					return
				}
				if strings.Contains(r.URL.Path, "/metrics/") {
					if name == "missing metric" || name == "missing metric and window" {
						w.WriteHeader(404)
						return
					}
					value := metric
					parts := strings.Split(r.URL.Path, "/")
					value.Key = parts[len(parts)-3]
					json.MarshalWrite(w, value)
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
				value := report
				if name == "report pin" {
					value.RegistrationVersion = hashB
				}
				json.MarshalWrite(w, map[string]any{"workspaceId": ws, "environmentId": "env1", "report": value})
			}))
			defer srv.Close()
			r := readerFor(t, srv.URL, func(c *Config) {
				if name == "operator scope" {
					c.EnvironmentID = "other"
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			got, err := r.CheckGoal(ctx, g, m)
			if writes.Load() != 0 || (preflight && calls.Load() != 0) {
				t.Fatal("unexpected HTTP", calls.Load(), writes.Load())
			}
			if wantErr != nil {
				if got != nil || !errors.Is(err, wantErr) {
					t.Fatalf("got %+v %v want %v", got, err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			fp, _ := GoalFingerprint(g)
			if got.Status != want || got.GoalFingerprint != fp || got.GoalVersion != g.Version || got.GoalID != g.ID || got.PublicationAuthorized || got.Observation.PublicationAuthorized {
				t.Fatalf("bad result %+v", got)
			}
			if code != "" {
				found := false
				for _, b := range got.Blockers {
					found = found || b.Code == code
				}
				if !found {
					t.Fatalf("missing %s: %+v", code, got.Blockers)
				}
			}
			observation, err := r.Monitor(t.Context(), m)
			if err != nil || !reflect.DeepEqual(observation, got.Observation) {
				t.Fatalf("monitor changed: %+v %v", got, err)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), g.Objective) {
				t.Fatal("untrusted prose exported")
			}
			if name != "missing report" && got.Observation.ServerVerdict != report.Decision.Verdict {
				t.Fatal("verdict changed")
			}
			if code == "baseline_window_mismatch" && name != "missing metric and window" && got.Observation.Status != Ready {
				t.Fatal("observation mutated")
			}
		})
	}
}
