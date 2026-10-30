package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var unexposedTokens = map[string]string{
	"omitted": "", "null": "null", "empty": "[]", "missing": "[{}]", "null-entry": "[null]",
	"null-count": `[{"unexposed":null}]`, "positive": `[{"unexposed":1}]`, "negative": `[{"unexposed":-1}]`,
	"max": `[{"unexposed":9223372036854775807}]`, "min": `[{"unexposed":-9223372036854775808}]`,
	"mixed-missing": `[{"unexposed":0},{}]`, "mixed-positive": `[{"unexposed":0},{"unexposed":1}]`,
	"mixed-negative": `[{"unexposed":-1},{"unexposed":0}]`,
	"zero":           `[{"unexposed":0,"variant":"secret","unitIds":["secret"],"assignments":99},{"unexposed":0}]`,
	"bad-string":     `[{"unexposed":"secret"}]`, "bad-bool": `[{"unexposed":true}]`,
	"bad-array": `[{"unexposed":[]}]`, "bad-object": `[{"unexposed":{}}]`,
	"bad-fraction": `[{"unexposed":0.5}]`, "bad-exponent": `[{"unexposed":1e0}]`,
	"bad-overflow": `[{"unexposed":9223372036854775808}]`, "bad-underflow": `[{"unexposed":-9223372036854775809}]`,
	"bad-duplicate": `[{"unexposed":0,"unexposed":1}]`, "bad-list-object": `{}`, "bad-list-string": `"secret"`,
	"bad-entry": `[0]`, "bad-duplicate-list": `[]`,
}
var unexposedCases, unexposedMalformedCases = func() ([]string, []string) {
	var valid, malformed []string
	for value := range unexposedTokens {
		for _, mode := range []string{"onRender", "onDecision"} {
			for _, verdict := range []string{"ship", "continue", "rollback"} {
				name := "unexposed " + value + " " + mode + " " + verdict
				if strings.HasPrefix(value, "bad-") {
					malformed = append(malformed, name)
				} else {
					valid = append(valid, name)
				}
			}
		}
	}
	return valid, malformed
}()

func malformedUnexposedCase(name string) bool {
	return strings.HasPrefix(name, "unexposed ") && strings.HasPrefix(strings.Fields(name)[1], "bad-")
}
func invalidUnexposedCase(name string) bool {
	if !strings.HasPrefix(name, "unexposed ") || malformedUnexposedCase(name) {
		return false
	}
	fields := strings.Fields(name)
	return fields[2] == "onRender" && fields[1] != "omitted" && fields[1] != "null" && fields[1] != "empty" && fields[1] != "zero"
}
func applyUnexposedCase(name string, report *growth.Report) {
	if strings.HasPrefix(name, "unexposed ") {
		report.Decision.Verdict = strings.Fields(name)[3]
	}
}
func unexposedReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "unexposed ") {
		return diagnosticsSRMReportWire(name, report)
	}
	fields := strings.Fields(name)
	report.ExposureMode, report.Diagnostics.ExposureMode = fields[2], fields[2]
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
	delete(diagnostics, "perVariant")
	base, err := json.Marshal(diagnostics)
	if err != nil {
		panic(err)
	}
	suffix := ""
	if token := unexposedTokens[fields[1]]; token != "" {
		suffix = `,"perVariant":` + strings.ReplaceAll(token, "secret", secret)
	}
	if fields[1] == "bad-duplicate-list" {
		suffix += `,"perVariant":[]`
	}
	wire["diagnostics"] = jsontext.Value(string(base[:len(base)-1]) + suffix + "}")
	return wire
}
func checkUnexposedBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !strings.HasPrefix(name, "unexposed ") {
		return
	}
	want := []growth.MonitorBlocker{}
	if invalidUnexposedCase(name) {
		want = append(want, growth.MonitorBlocker{Code: "health_review_required", Remediation: "review_diagnostics"})
	}
	if !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("unexposed blockers: %+v want %+v", got.Blockers, want)
	}
}
