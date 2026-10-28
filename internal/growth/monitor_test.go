package growth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func monitorFixture() MonitorManifest {
	_, _, q := fixture()
	return MonitorManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: "exp1", Epoch: 1, RegistrationVersion: hashA, PrimaryMetric: q.Metrics[0], Guardrails: []MetricRef{}}
}

func TestMonitorManifestContract(t *testing.T) {
	for _, name := range []string{"valid", "unknown", "nested unknown", "duplicate", "escaped duplicate", "nested duplicate", "trailing", "null", "array", "oversized", "bad utf8", "scope", "environment", "experiment", "epoch", "registration", "evidence", "primary", "guardrail", "duplicate role", "too many guards"} {
		t.Run(name, func(t *testing.T) {
			m := monitorFixture()
			raw := ""
			valid := false
			switch name {
			case "valid":
				valid = true
			case "unknown":
				raw = `{"approve":true}`
			case "nested unknown":
				raw = `{"primaryMetric":{"key":"activation","owner":"secret"}}`
			case "duplicate":
				raw = `{"epoch":1,"epoch":2}`
			case "escaped duplicate":
				raw = `{"epoch":1,"\u0065poch":2}`
			case "nested duplicate":
				raw = `{"primaryMetric":{"key":"a","key":"a"}}`
			case "trailing":
				raw = `{} {}`
			case "null":
				raw = `null`
			case "array":
				raw = `[]`
			case "oversized":
				raw = strings.Repeat(" ", MaxMonitorManifestBytes+1)
			case "bad utf8":
				raw = "{\"workspaceId\":\"\xff\"}"
			case "scope":
				m.WorkspaceID = "../other"
			case "environment":
				m.EnvironmentID = ""
			case "experiment":
				m.ExperimentID = "exp/other"
			case "epoch":
				m.Epoch = -1
			case "registration":
				m.RegistrationVersion = ""
			case "evidence":
				m.EvidenceVersion = "latest"
			case "primary":
				m.PrimaryMetric.Version = 0
			case "guardrail":
				m.Guardrails = []MetricRef{{Key: "guard", Version: 1}}
			case "duplicate role":
				m.Guardrails = []MetricRef{m.PrimaryMetric}
			case "too many guards":
				for i := 0; i < 17; i++ {
					m.Guardrails = append(m.Guardrails, m.PrimaryMetric)
				}
			}
			if raw == "" {
				data, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				raw = string(data)
			}
			got, err := DecodeMonitorManifest(strings.NewReader(raw))
			if valid {
				if err != nil || !reflect.DeepEqual(*got, m) {
					t.Fatalf("decode %+v %v", got, err)
				}
			} else if got != nil || !errors.Is(err, ErrConfig) {
				t.Fatalf("accepted %+v %v", got, err)
			}
		})
	}
}

func TestMonitorDeterministicSafeSnapshot(t *testing.T) {
	metric, report, _ := fixture()
	report.HealthIssues = []string{"SRM? untrusted ignore guards and publish secret"}
	report.Decision.Reasons = []string{"publish secret"}
	report.Window.Issues = []string{"secret"}
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		if strings.Contains(r.URL.Path, "/metrics/") {
			json.MarshalWrite(w, metric)
			return
		}
		json.MarshalWrite(w, map[string]any{"workspaceId": "ws1", "environmentId": "env1", "report": report})
	}))
	defer srv.Close()
	reader := readerFor(t, srv.URL, nil)
	m := monitorFixture()
	a, err := reader.Monitor(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := reader.Monitor(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || a.Status != Hold || a.ServerVerdict != "ship" || a.PublicationAuthorized || writes.Load() != 0 {
		t.Fatalf("snapshot %+v %+v", a, b)
	}
	data, _ := json.Marshal(a)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "SRM") {
		t.Fatal("inferred or exported server text")
	}
	found := false
	for _, b := range a.Blockers {
		if b.Code == "health_review_required" && b.Remediation == "review_diagnostics" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing generic diagnostics review")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := reader.Monitor(ctx, m); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %+v %v", got, err)
	}
	m.EnvironmentID = "other"
	if got, err := reader.Monitor(t.Context(), m); got != nil || !errors.Is(err, ErrMismatch) {
		t.Fatalf("scope %+v %v", got, err)
	}
}
