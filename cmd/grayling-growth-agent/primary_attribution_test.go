package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

var primaryAttributionCases = []string{
	"primary attribution omitted", "primary attribution null", "primary attribution zero",
	"primary attribution negative", "primary attribution minimum", "primary attribution below",
	"primary attribution above", "primary attribution match", "primary attribution deprecated",
}
var primaryAttributionBoundaryCases = []string{
	"primary attribution subhour", "primary attribution max", "primary attribution max mismatch",
	"primary attribution overflow", "primary attribution underflow", "primary attribution fraction",
	"primary attribution exponent", "primary attribution string", "primary attribution bool",
}

func malformedPrimaryAttributionCase(name string) bool {
	switch name {
	case "primary attribution overflow", "primary attribution underflow", "primary attribution fraction",
		"primary attribution exponent", "primary attribution string", "primary attribution bool":
		return true
	}
	return false
}
func invalidPrimaryAttributionCase(name string) bool {
	return strings.HasPrefix(name, "primary attribution ") && !malformedPrimaryAttributionCase(name) &&
		name != "primary attribution match" && name != "primary attribution deprecated" &&
		name != "primary attribution subhour" && name != "primary attribution max"
}
func primaryAttributionReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "primary attribution ") {
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
	token := "86400000000000"
	switch name {
	case "primary attribution omitted":
		delete(wire, "primaryAttributionWindow")
		return wire
	case "primary attribution null":
		token = "null"
	case "primary attribution zero":
		token = "0"
	case "primary attribution negative":
		token = "-1"
	case "primary attribution minimum":
		token = "-9223372036854775808"
	case "primary attribution below":
		token = "86399999999999"
	case "primary attribution above":
		token = "86400000000001"
	case "primary attribution subhour":
		token = "1800000000001"
	case "primary attribution max mismatch":
		token = "9223372036854775806"
	case "primary attribution max":
		token = "9223372036854775807"
	case "primary attribution overflow":
		token = "9223372036854775808"
	case "primary attribution underflow":
		token = "-9223372036854775809"
	case "primary attribution fraction":
		token = "0.5"
	case "primary attribution exponent":
		token = "1e3"
	case "primary attribution string":
		token = `"secret 24h"`
	case "primary attribution bool":
		token = "true"
	}
	wire["primaryAttributionWindow"] = jsontext.Value(token)
	return wire
}
func writePrimaryAttributionMetric(w http.ResponseWriter, name string, wire map[string]any) {
	switch name {
	case "primary attribution subhour":
		wire["attributionWindow"] = int64(1800000000001)
	case "primary attribution max", "primary attribution max mismatch":
		wire["attributionWindow"] = int64(9223372036854775807)
	case "primary attribution deprecated":
		wire["status"] = "deprecated"
	}
	json.MarshalWrite(w, wire)
}
func checkPrimaryAttributionBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	checkWatermarkBlockers(t, name, got)
	if !strings.HasPrefix(name, "primary attribution ") {
		return
	}
	expected := []growth.MonitorBlocker{}
	if invalidPrimaryAttributionCase(name) {
		expected = append(expected, growth.MonitorBlocker{Code: "primary_attribution_invalid", Remediation: "review_metric_definitions"})
	}
	if !reflect.DeepEqual(got.Blockers, expected) {
		t.Fatalf("attribution blockers: %+v want %+v", got.Blockers, expected)
	}
}
