package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestHolmCreateAuditCarriesRegistrationVersion(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("holm-flag"))
	metric := platform.MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"}
	exp, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, &platform.Experiment{
		FlagKey: "holm-flag", Unit: "user_id", Variants: []string{"on", "off"},
		Proportions:   map[string]float64{"on": 50, "off": 50},
		PrimaryMetric: metric, Alpha: .05, AnalysisWindow: time.Hour,
		HolmFamily: &platform.HolmFamily{ID: "launch", Hypotheses: []platform.HolmHypothesis{
			{ID: "conversion", Metric: metric, Control: "on", Treatment: "off"},
			{ID: "engagement", Metric: platform.MetricSpec{Key: "engagement", Type: "proportion", Direction: "increase"}, Control: "on", Treatment: "off"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp.AnalysisMethod != "two-proportion-z" {
		t.Fatalf("analysis method: %s", exp.AnalysisMethod)
	}
	for _, entry := range f.store.AuditList(f.ws.ID, 0) {
		if entry.Action == "experiment.create" && entry.Object == exp.ID {
			if !strings.Contains(entry.Detail, exp.RegistrationVersion) || !strings.Contains(entry.Detail, "engagement") {
				t.Fatalf("audit omits version/family: %s", entry.Detail)
			}
			return
		}
	}
	t.Fatal("experiment.create audit entry missing")
}
