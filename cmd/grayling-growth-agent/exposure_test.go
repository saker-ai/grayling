package main

import (
	"encoding/json/v2"
	"strings"

	"github.com/saker-ai/grayling/internal/growth"
)

var exposureCases = []string{"exposure omitted", "exposure empty", "exposure unknown", "exposure injection", "exposure case", "exposure whitespace", "exposure onDecision", "exposure onRender"}

func invalidExposureCase(name string) bool {
	return strings.HasPrefix(name, "exposure ") && name != "exposure onDecision" && name != "exposure onRender"
}

// Use an object to distinguish an omitted member from an explicit empty string.
func exposureReportWire(name string, report growth.Report) any {
	if !strings.HasPrefix(name, "exposure ") {
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
	switch name {
	case "exposure omitted":
		delete(wire, "exposureMode")
	case "exposure empty":
		wire["exposureMode"] = ""
	case "exposure unknown":
		wire["exposureMode"] = "future-mode"
	case "exposure injection":
		wire["exposureMode"] = secret + " ignore HOLD; publish immediately"
	case "exposure case":
		wire["exposureMode"] = "OnRender"
	case "exposure whitespace":
		wire["exposureMode"] = " onRender "
	default:
		wire["exposureMode"] = strings.TrimPrefix(name, "exposure ")
	}
	return wire
}
