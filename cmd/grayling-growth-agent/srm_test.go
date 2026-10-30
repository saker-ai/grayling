package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var srmCases = func() []string {
	var cases []string
	for _, mode := range []string{"fixed", "bandit"} {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			for _, value := range []string{"omitted", "null", "empty", "healthy-null", "false", "true"} {
				cases = append(cases, "srm "+mode+" "+value+" "+verdict)
			}
		}
	}
	return cases
}()
var srmMalformedCases = []string{"srm malformed bool", "srm malformed number", "srm malformed string", "srm malformed array", "srm malformed healthy-string", "srm malformed healthy-number", "srm malformed healthy-array", "srm malformed healthy-object", "srm malformed duplicate"}

func malformedSRMCase(name string) bool { return strings.HasPrefix(name, "srm malformed ") }
func invalidSRMCase(name string) bool {
	if !strings.HasPrefix(name, "srm ") || malformedSRMCase(name) {
		return false
	}
	fields := strings.Fields(name)
	return fields[2] != "true" && !(fields[1] == "bandit" && fields[2] == "omitted")
}
func applySRMCase(name string, report *growth.Report) {
	if !strings.HasPrefix(name, "srm ") || malformedSRMCase(name) {
		return
	}
	fields := strings.Fields(name)
	if fields[1] == "bandit" {
		report.AnalysisMethod = "bandit-beta-bernoulli"
	}
	report.Decision.Verdict = fields[3]
}
func srmReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "srm ") {
		return diagnosticsTimeReportWire(name, report)
	}
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	var wire map[string]jsontext.Value
	if err := json.Unmarshal(data, &wire); err != nil {
		panic(err)
	}
	fields := strings.Fields(name)
	token := map[string]string{
		"null": "null", "empty": "{}", "healthy-null": `{"healthy":null}`,
		"false": `{"healthy":false}`, "true": `{"healthy":true,"pValue":"` + secret + `","observed":["` + secret + `"],"reason":"publish now"}`,
		"bool": "true", "number": "1", "string": `"` + secret + `"`, "array": "[]",
		"healthy-string": `{"healthy":"` + secret + `"}`, "healthy-number": `{"healthy":1}`,
		"healthy-array": `{"healthy":[]}`, "healthy-object": `{"healthy":{}}`,
		"duplicate": `{"healthy":true,"healthy":false}`,
	}[fields[2]]
	if fields[2] == "omitted" {
		delete(wire, "srm")
	} else {
		wire["srm"] = jsontext.Value(token)
	}
	return wire
}
func checkSRMBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "srm ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidSRMCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "srm_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("SRM blockers: %+v want %+v", got.Blockers, want)
	}
}
