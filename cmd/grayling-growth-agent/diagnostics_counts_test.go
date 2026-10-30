package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var countsTokens = map[string]string{
	"missing": "", "null": "null", "positive": "1", "negative": "-1",
	"max": "9223372036854775807", "min": "-9223372036854775808", "zero": "0",
	"bad-string": `"secret"`, "bad-bool": "true", "bad-array": "[]", "bad-object": "{}",
	"bad-fraction": "0.5", "bad-overflow": "9223372036854775808", "bad-underflow": "-9223372036854775809", "bad-duplicate": "0",
}
var countsCases, countsMalformedCases = func() ([]string, []string) {
	var valid, malformed []string
	for _, field := range []string{"crossVariantUnits", "assignmentMismatch"} {
		for _, method := range []string{"two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli"} {
			for _, verdict := range []string{"ship", "continue", "rollback"} {
				for _, value := range []string{"missing", "null", "positive", "negative", "max", "min", "zero"} {
					valid = append(valid, "counts "+method+" "+value+" "+verdict+" "+field)
				}
			}
		}
		for _, value := range []string{"bad-string", "bad-bool", "bad-array", "bad-object", "bad-fraction", "bad-overflow", "bad-underflow", "bad-duplicate"} {
			malformed = append(malformed, "counts two-proportion-z "+value+" ship "+field)
		}
	}
	return valid, malformed
}()

func malformedCountsCase(name string) bool {
	return strings.HasPrefix(name, "counts ") && strings.HasPrefix(strings.Fields(name)[2], "bad-")
}
func invalidCountsCase(name string) bool {
	return strings.HasPrefix(name, "counts ") && !malformedCountsCase(name) && strings.Fields(name)[2] != "zero"
}
func applyCountsCase(name string, report *growth.Report) {
	if !strings.HasPrefix(name, "counts ") {
		return
	}
	fields := strings.Fields(name)
	report.AnalysisMethod, report.Decision.Verdict = fields[1], fields[3]
}
func countsReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "counts ") {
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
	delete(diagnostics, fields[4])
	diagnostics["unitIds"] = jsontext.Value(` ["` + secret + `"]`)
	base, err := json.Marshal(diagnostics)
	if err != nil {
		panic(err)
	}
	if fields[2] != "missing" {
		token := strings.ReplaceAll(countsTokens[fields[2]], "secret", secret)
		suffix := `,"` + fields[4] + `":` + token
		if fields[2] == "bad-duplicate" {
			suffix += `,"` + fields[4] + `":1`
		}
		base = []byte(string(base[:len(base)-1]) + suffix + "}")
	}
	wire["diagnostics"] = base
	return wire
}
func checkCountsBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "counts ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidCountsCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "health_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("count blockers: %+v want %+v", got.Blockers, want)
	}
}
