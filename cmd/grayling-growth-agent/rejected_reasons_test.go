package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var rejectionsTokens = map[string]string{
	"omitted": "", "null": "null", "empty": "{}", "zero": `{"late_event":0}`, "duplicates-only": "",
	"positive": `{"late_event":1}`, "negative": `{"missing_unit":-1}`, "null-count": `{"invalid_event":null}`,
	"max": `{"future_event":9223372036854775807}`, "min": `{"stale_epoch":-9223372036854775808}`,
	"unknown": `{"secret":0}`, "mixed": `{"late_event":0,"missing_exposure":1}`,
	"duplicate-reason": `{"duplicate_event":0}`,
	"bad-string":       `{"late_event":"secret"}`, "bad-bool": `{"late_event":true}`,
	"bad-array": `{"late_event":[]}`, "bad-object": `{"late_event":{}}`,
	"bad-fraction": `{"late_event":0.5}`, "bad-exponent": `{"late_event":1e0}`,
	"bad-overflow": `{"late_event":9223372036854775808}`, "bad-underflow": `{"late_event":-9223372036854775809}`,
	"bad-duplicate": `{"late_event":0,"late_event":1}`, "bad-escaped-duplicate": `{"late_event":0,"late_\u0065vent":0}`,
	"bad-unknown": `{"secret":9223372036854775808}`, "bad-map-array": "[]", "bad-map-string": `"secret"`, "bad-duplicate-map": "{}",
}
var rejectionsCases, rejectionsMalformedCases = func() ([]string, []string) {
	var valid, malformed []string
	for value := range rejectionsTokens {
		for _, verdict := range []string{"ship", "continue", "rollback"} {
			name := "rejections two-proportion-z " + value + " " + verdict
			if strings.HasPrefix(value, "bad-") {
				malformed = append(malformed, name)
			} else {
				valid = append(valid, name)
			}
		}
	}
	return valid, malformed
}()

func malformedRejectionsCase(name string) bool {
	return strings.HasPrefix(name, "rejections ") && strings.HasPrefix(strings.Fields(name)[2], "bad-")
}
func invalidRejectionsCase(name string) bool {
	if !strings.HasPrefix(name, "rejections ") || malformedRejectionsCase(name) {
		return false
	}
	switch strings.Fields(name)[2] {
	case "omitted", "null", "empty", "zero", "duplicates-only":
		return false
	}
	return true
}
func applyRejectionsCase(name string, report *growth.Report) {
	if !strings.HasPrefix(name, "rejections ") {
		return
	}
	fields := strings.Fields(name)
	report.AnalysisMethod, report.Decision.Verdict = fields[1], fields[3]
}
func rejectionsReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "rejections ") {
		return unexposedReportWire(name, report)
	}
	fields := strings.Fields(name)
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
	delete(diagnostics, "rejectedByReason")
	diagnostics["unitIds"] = jsontext.Value(` ["` + secret + `"]`)
	base, err := json.Marshal(diagnostics)
	if err != nil {
		panic(err)
	}
	suffix := `,"duplicateEvents":999`
	if token := rejectionsTokens[fields[2]]; token != "" {
		suffix += `,"rejectedByReason":` + strings.ReplaceAll(token, "secret", secret)
	}
	if fields[2] == "bad-duplicate-map" {
		suffix += `,"rejectedByReason":{}`
	}
	base = []byte(string(base[:len(base)-1]) + suffix + "}")
	wire["diagnostics"] = base
	return wire
}
func checkRejectionsBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "rejections ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidRejectionsCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "health_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("count blockers: %+v want %+v", got.Blockers, want)
	}
}
