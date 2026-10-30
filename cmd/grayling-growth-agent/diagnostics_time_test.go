package main

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

var diagnosticsTimeCases = []string{
	"diagnostics time earlier second", "diagnostics time later second",
	"diagnostics time earlier nanosecond", "diagnostics time later nanosecond",
	"diagnostics time missing diagnostics", "diagnostics time missing report", "diagnostics time both missing",
	"diagnostics time equal UTC", "diagnostics time equal positive offset", "diagnostics time equal negative offset",
}

func isDiagnosticsTimeCase(name string) bool { return strings.HasPrefix(name, "diagnostics time ") }

func invalidDiagnosticsTimeCase(name string) bool {
	return isDiagnosticsTimeCase(name) && !strings.HasPrefix(name, "diagnostics time equal ")
}

// Mutate only generation metadata, keeping all other evidence healthy.
func applyDiagnosticsTimeCase(name string, report *growth.Report) bool {
	if !isDiagnosticsTimeCase(name) {
		return false
	}
	report.GeneratedAt = report.GeneratedAt.Add(123456789 * time.Nanosecond)
	report.Diagnostics.GeneratedAt = report.GeneratedAt
	switch name {
	case "diagnostics time earlier second":
		report.Diagnostics.GeneratedAt = report.GeneratedAt.Add(-time.Second)
	case "diagnostics time later second":
		report.Diagnostics.GeneratedAt = report.GeneratedAt.Add(time.Second)
	case "diagnostics time earlier nanosecond":
		report.Diagnostics.GeneratedAt = report.GeneratedAt.Add(-time.Nanosecond)
	case "diagnostics time later nanosecond":
		report.Diagnostics.GeneratedAt = report.GeneratedAt.Add(time.Nanosecond)
	case "diagnostics time missing diagnostics":
		report.Diagnostics.GeneratedAt = time.Time{}
	case "diagnostics time missing report":
		report.GeneratedAt = time.Time{}
	case "diagnostics time both missing":
		report.GeneratedAt, report.Diagnostics.GeneratedAt = time.Time{}, time.Time{}
	case "diagnostics time equal positive offset":
		report.Diagnostics.GeneratedAt = report.GeneratedAt.In(time.FixedZone("east", 8*3600))
	case "diagnostics time equal negative offset":
		report.Diagnostics.GeneratedAt = report.GeneratedAt.In(time.FixedZone("west", -7*3600))
	}
	return invalidDiagnosticsTimeCase(name)
}

// Omit missing members on the wire; library fixtures separately cover zero times.
func diagnosticsTimeReportWire(name string, report growth.Report) any {
	if name != "diagnostics time missing report" && name != "diagnostics time missing diagnostics" && name != "diagnostics time both missing" {
		return report
	}
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		panic(err)
	}
	if name != "diagnostics time missing diagnostics" {
		delete(wire, "generatedAt")
	}
	if name != "diagnostics time missing report" {
		delete(wire["diagnostics"].(map[string]any), "generatedAt")
	}
	return wire
}

func checkDiagnosticsTimeBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !isDiagnosticsTimeCase(name) {
		return
	}
	expected := []growth.MonitorBlocker{}
	if name == "diagnostics time missing report" || name == "diagnostics time both missing" {
		expected = append(expected, growth.MonitorBlocker{Code: "generation_time_missing", Remediation: "refresh_report"})
	}
	if invalidDiagnosticsTimeCase(name) {
		expected = append(expected, growth.MonitorBlocker{Code: "diagnostics_metadata_invalid", Remediation: "refresh_report"})
	}
	if !reflect.DeepEqual(got.Blockers, expected) {
		t.Fatalf("diagnostic blockers: %+v want %+v", got.Blockers, expected)
	}
}
