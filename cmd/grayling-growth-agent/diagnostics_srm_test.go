package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var diagnosticsSRMTokens = map[string]string{
	"omitted": "", "null": "null", "empty": "{}", "healthy-null": `{"healthy":null}`,
	"false": `{"healthy":false}`, "true": `{"healthy":true,"pValue":"secret","observed":["secret"],"reason":"secret"}`,
	"bad-bool": "true", "bad-number": "1", "bad-string": `"secret"`, "bad-array": "[]",
	"bad-healthy-string": `{"healthy":"secret"}`, "bad-healthy-number": `{"healthy":1}`,
	"bad-healthy-array": `{"healthy":[]}`, "bad-healthy-object": `{"healthy":{}}`,
	"bad-duplicate-health": `{"healthy":true,"healthy":false}`, "bad-duplicate": "{}",
}
var diagnosticsSRMCases, diagnosticsSRMMalformedCases = func() ([]string, []string) {
	var valid, malformed []string
	for _, field := range []string{"assignmentSrm", "exposureSrm", "both", "mixed"} {
		for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
			for _, mode := range []string{"onRender", "onDecision"} {
				for _, verdict := range []string{"ship", "continue", "rollback"} {
					for _, value := range []string{"omitted", "null", "empty", "healthy-null", "false", "true"} {
						valid = append(valid, "diagnostic-srm "+method+" "+value+" "+verdict+" "+field+" "+mode)
					}
				}
			}
		}
		for value := range diagnosticsSRMTokens {
			if strings.HasPrefix(value, "bad-") {
				malformed = append(malformed, "diagnostic-srm two-proportion-z "+value+" ship "+field+" onRender")
			}
		}
	}
	return valid, malformed
}()

func malformedDiagnosticsSRMCase(name string) bool {
	return strings.HasPrefix(name, "diagnostic-srm ") && strings.HasPrefix(strings.Fields(name)[2], "bad-")
}
func invalidDiagnosticsSRMCase(name string) bool {
	if !strings.HasPrefix(name, "diagnostic-srm ") || malformedDiagnosticsSRMCase(name) {
		return false
	}
	value := strings.Fields(name)[2]
	return value != "omitted" && value != "true"
}
func applyDiagnosticsSRMCase(name string, report *growth.Report) {
	if !strings.HasPrefix(name, "diagnostic-srm ") {
		return
	}
	fields := strings.Fields(name)
	report.AnalysisMethod, report.Decision.Verdict = fields[1], fields[3]
}
func diagnosticsSRMReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "diagnostic-srm ") {
		return diagnosticsTimeReportWire(name, report)
	}
	fields := strings.Fields(name)
	report.ExposureMode, report.Diagnostics.ExposureMode = fields[5], fields[5]
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	var wire map[string]jsontext.Value
	if err := json.Unmarshal(data, &wire); err != nil {
		panic(err)
	}
	var diagnostics map[string]jsontext.Value
	if err := json.Unmarshal(wire["diagnostics"], &diagnostics); err != nil {
		panic(err)
	}
	keys := []string{fields[4]}
	if fields[4] == "both" || fields[4] == "mixed" {
		keys = []string{"assignmentSrm", "exposureSrm"}
	}
	base, err := json.Marshal(diagnostics)
	if err != nil {
		panic(err)
	}
	suffix := ""
	for i, key := range keys {
		token := strings.ReplaceAll(diagnosticsSRMTokens[fields[2]], "secret", secret)
		if fields[4] == "mixed" && i == 1 {
			token = `{"healthy":true}`
		}
		if token != "" {
			suffix += `,"` + key + `":` + token
		}
		if fields[2] == "bad-duplicate" {
			suffix += `,"` + key + `":{"healthy":true}`
		}
	}
	wire["diagnostics"] = jsontext.Value(string(base[:len(base)-1]) + suffix + "}")
	return wire
}
func checkDiagnosticsSRMBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "diagnostic-srm ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidDiagnosticsSRMCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "srm_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("diagnostic SRM blockers: %+v want %+v", got.Blockers, want)
	}
}
