package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var journeysCases = func() []string {
	var cases []string
	for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			for _, value := range []string{"omitted", "null", "empty", "section-null", "missing-arms", "null-arms", "empty-arms", "null-arm", "missing", "null-immature", "positive", "negative", "cohort", "short", "negative-cohort", "negative-short", "mixed", "mixed-sections", "zero"} {
				cases = append(cases, "journeys "+method+" "+value+" "+verdict)
			}
		}
	}
	return cases
}()
var journeysTokens = map[string]string{
	"null":            `null`,
	"empty":           `[]`,
	"section-null":    `[null]`,
	"missing-arms":    `[{}]`,
	"null-arms":       `[{"arms":null}]`,
	"empty-arms":      `[{"arms":[]}]`,
	"null-arm":        `[{"arms":[null]}]`,
	"missing":         `[{"arms":[{}]}]`,
	"null-immature":   `[{"arms":[{"immature":null}]}]`,
	"positive":        `[{"arms":[{"immature":1}]}]`,
	"negative":        `[{"arms":[{"immature":-1}]}]`,
	"cohort":          `[{"arms":[{"immature":0,"excluded":{"immature_cohort":1}}]}]`,
	"short":           `[{"arms":[{"immature":0,"excluded":{"attribution_window_short":1}}]}]`,
	"negative-cohort": `[{"arms":[{"immature":0,"excluded":{"immature_cohort":-1}}]}]`,
	"negative-short":  `[{"arms":[{"immature":0,"excluded":{"attribution_window_short":-1}}]}]`,
	"mixed":           `[{"arms":[{"immature":0},{"immature":1}]}]`,
	"mixed-sections":  `[{"arms":[{"immature":0}]},{}]`,
	"zero":            `[{"metric":"secret","retention":{"secret":"secret"},"type":"secret","arms":[{"variant":"secret","numerator":-1,"denominator":-1,"immature":0,"excluded":{"immature_cohort":0,"attribution_window_short":0,"secret":"secret"}}]}]`,
	"bad0":            `true`,
	"bad1":            `1`,
	"bad2":            `"secret"`,
	"bad3":            `{}`,
	"bad4":            `[true]`,
	"bad5":            `[{"arms":{}}]`,
	"bad6":            `[{"arms":[true]}]`,
	"bad7":            `[{"arms":[{"immature":"secret"}]}]`,
	"bad8":            `[{"arms":[{"immature":0.5}]}]`,
	"bad9":            `[{"arms":[{"immature":9223372036854775808}]}]`,
	"bad10":           `[{"arms":[{"immature":0,"immature":1}]}]`,
	"bad11":           `[{"arms":[{"immature":0,"excluded":true}]}]`,
	"bad12":           `[{"arms":[{"immature":0,"excluded":{"immature_cohort":"secret"}}]}]`,
}
var journeysMalformedCases = func() []string {
	var cases []string
	for _, value := range []string{"bad0", "bad1", "bad2", "bad3", "bad4", "bad5", "bad6", "bad7", "bad8", "bad9", "bad10", "bad11", "bad12"} {
		cases = append(cases, "journeys malformed "+value)
	}
	return cases
}()

func malformedJourneysCase(name string) bool { return strings.HasPrefix(name, "journeys malformed ") }
func invalidJourneysCase(name string) bool {
	if !strings.HasPrefix(name, "journeys ") || malformedJourneysCase(name) {
		return false
	}
	switch strings.Fields(name)[2] {
	case "omitted", "null", "empty", "zero":
		return false
	}
	return true
}
func applyJourneysCase(name string, report *growth.Report) {
	if !strings.HasPrefix(name, "journeys ") || malformedJourneysCase(name) {
		return
	}
	fields := strings.Fields(name)
	report.AnalysisMethod, report.Decision.Verdict = fields[1], fields[3]
}
func journeysReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "journeys ") {
		return countsReportWire(name, report)
	}
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	var wire map[string]jsontext.Value
	if err := json.Unmarshal(data, &wire); err != nil {
		panic(err)
	}
	value := strings.Fields(name)[2]
	if value == "omitted" {
		delete(wire, "journeys")
	} else {
		wire["journeys"] = jsontext.Value(strings.ReplaceAll(journeysTokens[value], "secret", secret))
	}
	return wire
}
func checkJourneysBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "journeys ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidJourneysCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "health_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("guardrail blockers: %+v want %+v", got.Blockers, want)
	}
}
