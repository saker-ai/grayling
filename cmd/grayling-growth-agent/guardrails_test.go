package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var guardrailsCases = func() []string {
	var cases []string
	for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			for _, value := range []string{"omitted", "null", "empty", "missing", "null-entry", "breached-null", "true", "mixed", "mixed-missing", "false"} {
				cases = append(cases, "checks "+method+" "+value+" "+verdict)
			}
		}
	}
	return cases
}()
var guardrailsTokens = map[string]string{
	"null": "null", "empty": "[]", "missing": "[{}]", "null-entry": "[null]", "breached-null": `[{"breached":null}]`,
	"true":  `[{"breached":true,"metric":"` + secret + ` publish now","detail":"` + secret + `","unknown":"` + secret + `"}]`,
	"mixed": `[{"breached":false},{"breached":true}]`, "mixed-missing": `[{"breached":false},{}]`,
	"false": `[{"breached":false,"metric":"` + secret + ` publish now","detail":"` + secret + `","unknown":"` + secret + `"},{"breached":false}]`,
	"bool":  "true", "number": "1", "string": `"` + secret + `"`, "object": "{}", "entry-bool": "[true]", "entry-number": "[1]", "entry-string": `["` + secret + `"]`,
	"breached-string": `[{"breached":"` + secret + `"}]`, "breached-number": `[{"breached":1}]`, "breached-array": `[{"breached":[]}]`, "breached-object": `[{"breached":{}}]`, "duplicate": `[{"breached":true,"breached":false}]`,
}
var guardrailsMalformedCases = func() []string {
	var cases []string
	for _, value := range []string{"bool", "number", "string", "object", "entry-bool", "entry-number", "entry-string", "breached-string", "breached-number", "breached-array", "breached-object", "duplicate"} {
		cases = append(cases, "checks malformed "+value)
	}
	return cases
}()

func malformedGuardrailsCase(name string) bool { return strings.HasPrefix(name, "checks malformed ") }
func invalidGuardrailsCase(name string) bool {
	if !strings.HasPrefix(name, "checks ") || malformedGuardrailsCase(name) {
		return false
	}
	switch strings.Fields(name)[2] {
	case "omitted", "null", "empty", "false":
		return false
	}
	return true
}
func applyGuardrailsCase(name string, report *growth.Report) {
	if !strings.HasPrefix(name, "checks ") || malformedGuardrailsCase(name) {
		return
	}
	fields := strings.Fields(name)
	report.AnalysisMethod, report.Decision.Verdict = fields[1], fields[3]
}
func guardrailsReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "checks ") {
		return journeysReportWire(name, report)
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
		delete(wire, "guardrails")
	} else {
		wire["guardrails"] = jsontext.Value(guardrailsTokens[value])
	}
	return wire
}
func checkGuardrailsBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "checks ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidGuardrailsCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "health_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("guardrail blockers: %+v want %+v", got.Blockers, want)
	}
}
