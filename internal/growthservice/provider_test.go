package growthservice

import (
	"context"
	"encoding/json/v2"
	"github.com/saker-ai/grayling/internal/growth"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func providerSettings(t *testing.T, origin string) ProviderSettings {
	t.Helper()
	t.Setenv("GROWTH_TEST_ORIGIN", origin)
	t.Setenv("GROWTH_TEST_KEY", "private-fixture-key-not-for-logs")
	t.Setenv("GROWTH_TEST_MODEL", "fixture-model")
	return ProviderSettings{BaseURLEnv: "GROWTH_TEST_ORIGIN", APIKeyEnv: "GROWTH_TEST_KEY", ModelEnv: "GROWTH_TEST_MODEL", MaxOutputTokens: 700, MaxCalls: 1}
}
func TestProviderTransportBudgetSchemaAndSecrets(t *testing.T) {
	for _, scenario := range []string{"valid", "tool", "truncated", "unknown", "redirect", "oversized", "credential_echo"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer private-fixture-key-not-for-logs" {
					t.Error("request contract")
				}
				raw, e := io.ReadAll(r.Body)
				if e != nil {
					t.Error("request read")
				}
				if strings.Contains(string(raw), "goal prose secret") {
					t.Error("unapproved model input")
				}
				if scenario == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1", 302)
					return
				}
				if scenario == "oversized" {
					w.Write([]byte(strings.Repeat("x", 65537)))
					return
				}
				items := suggestions(hb)
				content, _ := json.Marshal(map[string]any{"suggestions": items})
				if scenario == "unknown" {
					content = []byte(`{"publish":true}`)
				}
				if scenario == "credential_echo" {
					items[0].Hypothesis = "private-fixture-key-not-for-logs"
					content, _ = json.Marshal(map[string]any{"suggestions": items})
				}
				finish := "stop"
				if scenario == "truncated" {
					finish = "length"
				}
				message := map[string]any{"content": string(content)}
				if scenario == "tool" {
					message["tool_calls"] = []any{map[string]any{"type": "function"}}
				}
				write(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": message}}})
			}))
			defer server.Close()
			p, e := NewProviderPlanner(providerSettings(t, server.URL+"/v1"))
			if e != nil {
				t.Fatal("configuration failed")
			}
			out, e := p.Plan(t.Context(), PlannerInput{EvidenceVersion: hb})
			if (e == nil) != (scenario == "valid") {
				t.Fatal("provider validation outcome")
			}
			if strings.Contains(string(out), "private-fixture") {
				t.Fatal("credential disclosure")
			}
			if _, e = p.Plan(t.Context(), PlannerInput{EvidenceVersion: hb}); e == nil || calls.Load() != 1 {
				t.Fatal("provider call budget bypass")
			}
		})
	}
	for _, origin := range []string{"http://remote.invalid/v1", "https://user:secret@host.invalid/v1", "https://host.invalid/v1?key=secret"} {
		if _, err := NewProviderPlanner(providerSettings(t, origin)); err == nil {
			t.Fatal("unsafe provider origin")
		}
	}
}

// Explicit opt-in only. One small live call on synthetic aggregates, no tools,
// no retries, no provider configuration or raw response in output/artifacts.
func TestProviderLiveAcceptance(t *testing.T) {
	if os.Getenv("GRAYLING_GROWTH_LIVE_ACCEPTANCE") != "1" {
		t.Skip("explicit bounded provider acceptance not enabled")
	}
	settings := ProviderSettings{BaseURLEnv: "CORE_OPENAI_BASE_URL", APIKeyEnv: "CORE_API_KEY", ModelEnv: "CORE_DEFAULT_LLM_MODEL", MaxOutputTokens: 1800, MaxCalls: 1}
	p, err := NewProviderPlanner(settings)
	if err != nil {
		t.Fatal("inherited provider configuration invalid")
	}
	out, err := p.Plan(context.Background(), PlannerInput{Primary: growth.MetricRef{Key: "activation", Version: 1, DefinitionHash: ha}, EvidenceVersion: hb, Aggregates: []Aggregate{{Arm: 0, ExposedUnits: new(int64(100)), ConvertedUnits: new(int64(20)), PrimaryValue: new(0.2)}, {Arm: 1, ExposedUnits: new(int64(100)), ConvertedUnits: new(int64(30)), PrimaryValue: new(0.3)}}})
	if err != nil {
		t.Fatalf("bounded live acceptance: %v", err)
	}
	var items []Suggestion
	if json.Unmarshal(out, &items) != nil || !validSuggestions(items, hb) {
		t.Fatal("invalid live suggestion contract")
	}
	dir := os.Getenv("GRAYLING_GROWTH_LIVE_EVIDENCE")
	if dir != "" {
		if !filepath.IsAbs(dir) {
			t.Fatal("private evidence path must be absolute")
		}
		if os.MkdirAll(dir, 0700) != nil {
			t.Fatal("evidence directory unavailable")
		}
		if os.WriteFile(filepath.Join(dir, "suggestions.json"), out, 0600) != nil {
			t.Fatal("evidence write failed")
		}
		summary, _ := json.Marshal(map[string]any{"liveCalls": 1, "syntheticInputs": true, "suggestions": len(items), "schemaValid": true, "numericAuthority": "existing server only", "humanQualityReview": false})
		if os.WriteFile(filepath.Join(dir, "result.json"), summary, 0600) != nil {
			t.Fatal("summary write failed")
		}
	}
	t.Log("one bounded real-provider call: schema and exact evidence pin valid; no action authority; synthetic aggregate input only; human quality unassessed")
}
