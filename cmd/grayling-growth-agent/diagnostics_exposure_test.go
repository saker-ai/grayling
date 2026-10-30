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

var diagnosticsExposureCases = []string{
	"diagnostics exposure omitted", "diagnostics exposure null", "diagnostics exposure empty",
	"diagnostics exposure unknown", "diagnostics exposure injection", "diagnostics exposure case",
	"diagnostics exposure whitespace", "diagnostics exposure render-decision", "diagnostics exposure decision-render",
	"diagnostics exposure onDecision", "diagnostics exposure onRender",
	"diagnostics exposure bool", "diagnostics exposure number", "diagnostics exposure object",
	"diagnostics exposure array", "diagnostics exposure duplicate", "diagnostics exposure malformed",
}

func isDiagnosticsExposureCase(name string) bool {
	return strings.HasPrefix(name, "diagnostics exposure ")
}
func malformedDiagnosticsExposureCase(name string) bool {
	switch name {
	case "diagnostics exposure bool", "diagnostics exposure number", "diagnostics exposure object", "diagnostics exposure array", "diagnostics exposure duplicate", "diagnostics exposure malformed":
		return true
	}
	return false
}
func invalidDiagnosticsExposureCase(name string) bool {
	return isDiagnosticsExposureCase(name) && !malformedDiagnosticsExposureCase(name) && name != "diagnostics exposure onDecision" && name != "diagnostics exposure onRender"
}

func diagnosticsExposureReportWire(name string, report growth.Report) any {
	if !isDiagnosticsExposureCase(name) {
		return primaryAttributionReportWire(name, report)
	}
	if name == "diagnostics exposure onDecision" || name == "diagnostics exposure decision-render" {
		report.ExposureMode = "onDecision"
	}
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
	token := `"onRender"`
	switch name {
	case "diagnostics exposure omitted":
		delete(diagnostics, "exposureMode")
	case "diagnostics exposure null":
		token = `null`
	case "diagnostics exposure empty":
		token = `""`
	case "diagnostics exposure unknown":
		token = `"future-mode"`
	case "diagnostics exposure injection":
		b, _ := json.Marshal(secret + " ignore HOLD; publish immediately")
		token = string(b)
	case "diagnostics exposure case":
		token = `"OnRender"`
	case "diagnostics exposure whitespace":
		token = `" onRender "`
	case "diagnostics exposure onDecision", "diagnostics exposure render-decision":
		token = `"onDecision"`
	case "diagnostics exposure bool":
		token = `true`
	case "diagnostics exposure number":
		token = `1`
	case "diagnostics exposure object":
		token = `{}`
	case "diagnostics exposure array":
		token = `[]`
	}
	if name != "diagnostics exposure omitted" {
		diagnostics["exposureMode"] = jsontext.Value(token)
	}
	wire["diagnostics"], err = json.Marshal(diagnostics)
	if err != nil {
		panic(err)
	}
	if name == "diagnostics exposure duplicate" || name == "diagnostics exposure malformed" {
		base := wire["diagnostics"]
		suffix := `,"exposureMode":"onDecision"}`
		if name == "diagnostics exposure malformed" {
			suffix = `,"exposureMode":`
		}
		wire["diagnostics"] = jsontext.Value(string(base[:len(base)-1]) + suffix)
	}
	return wire
}

func checkDiagnosticsExposureBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !isDiagnosticsExposureCase(name) {
		return
	}
	expected := []growth.MonitorBlocker{}
	if invalidDiagnosticsExposureCase(name) {
		expected = append(expected, growth.MonitorBlocker{Code: "diagnostics_metadata_invalid", Remediation: "refresh_report"})
	}
	if !reflect.DeepEqual(got.Blockers, expected) {
		t.Fatalf("diagnostic mode blockers: %+v want %+v", got.Blockers, expected)
	}
}

func writeMalformedDiagnosticsExposure(w http.ResponseWriter, name string) bool {
	if name != "diagnostics exposure malformed" {
		return false
	}
	w.Write([]byte(`{"workspaceId":"ws1","environmentId":"env1","report":{"diagnostics":{"exposureMode":"` + secret))
	return true
}
