package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

var watermarkCases = []string{
	"watermark future second", "watermark future nanosecond", "watermark report only future", "watermark diagnostics only future",
	"watermark earlier second", "watermark earlier nanosecond", "watermark equal UTC", "watermark equal positive offset", "watermark equal negative offset", "watermark future offset",
}

func isWatermarkCase(name string) bool { return strings.HasPrefix(name, "watermark ") }
func invalidWatermarkCase(name string) bool {
	return isWatermarkCase(name) && strings.Contains(name, "future")
}
func applyWatermarkCase(name string, report *growth.Report) bool {
	if !isWatermarkCase(name) {
		return false
	}
	delta, offset := time.Duration(0), 0
	switch name {
	case "watermark future second":
		delta = time.Second
	case "watermark future nanosecond", "watermark report only future", "watermark diagnostics only future", "watermark future offset":
		delta = time.Nanosecond
	case "watermark earlier second":
		delta = -time.Second
	case "watermark earlier nanosecond":
		delta = -time.Nanosecond
	}
	if name == "watermark equal positive offset" {
		offset = 8 * 3600
	}
	if name == "watermark equal negative offset" || name == "watermark future offset" {
		offset = -7 * 3600
	}
	report.GeneratedAt = report.Window.Watermark.Add(-delta)
	report.Diagnostics.GeneratedAt = report.GeneratedAt
	w := report.Window.Watermark.In(time.FixedZone("fixture", offset))
	report.Window.Watermark = w
	report.Diagnostics.Window = report.Window
	if name == "watermark report only future" {
		report.Diagnostics.Window.Watermark = report.GeneratedAt
	}
	if name == "watermark diagnostics only future" {
		report.Window.Watermark = report.GeneratedAt
	}
	return invalidWatermarkCase(name)
}

func checkWatermarkBlockers(t *testing.T, name string, got growth.MonitorSummary) {
	t.Helper()
	if !isWatermarkCase(name) {
		return
	}
	expected := []growth.MonitorBlocker{}
	if invalidWatermarkCase(name) {
		expected = append(expected, growth.MonitorBlocker{Code: "window_metadata_invalid", Remediation: "review_window"})
	}
	if name == "watermark report only future" || name == "watermark diagnostics only future" {
		expected = append(expected, growth.MonitorBlocker{Code: "diagnostics_metadata_invalid", Remediation: "refresh_report"})
	}
	if !reflect.DeepEqual(got.Blockers, expected) {
		t.Fatalf("watermark blockers: %+v want %+v", got.Blockers, expected)
	}
}
