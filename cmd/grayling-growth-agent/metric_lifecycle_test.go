package main

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

var metricLifecycleCases = func() []string {
	var names []string
	for _, key := range []string{"activation", "retention"} {
		for _, status := range []string{"published", "deprecated", "draft", "missing", "unknown", "absent"} {
			names = append(names, "lifecycle "+key+" "+status)
		}
	}
	return names
}()

func lifecycleStatus(name string) string {
	if strings.HasPrefix(name, "lifecycle ") {
		return strings.Fields(name)[2]
	}
	return "published"
}

// These are wire fixtures; missing really omits the lifecycle member.
func writeLifecycleMetric(w http.ResponseWriter, name string, metric growth.Metric) {
	status := "published"
	if strings.HasPrefix(name, "lifecycle "+metric.Key+" ") {
		status = lifecycleStatus(name)
	}
	if status == "absent" {
		w.WriteHeader(404)
		return
	}
	wire := map[string]any{"workspaceId": metric.WorkspaceID, "key": metric.Key, "version": metric.Version, "hash": metric.Hash, "attributionWindow": int64(24 * time.Hour)}
	if status != "missing" {
		if status == "unknown" {
			status = secret
		}
		wire["status"] = status
	}
	if strings.HasPrefix(name, "attribution ") {
		switch name {
		case "attribution missing":
			if metric.Key == "activation" {
				delete(wire, "attributionWindow")
			}
		case "attribution invalid":
			if metric.Key == "activation" {
				wire["attributionWindow"] = -1
			}
		case "attribution mismatch":
			if metric.Key == "activation" {
				wire["attributionWindow"] = int64(48 * time.Hour)
			}
		case "attribution sub-hour":
			if metric.Key == "activation" {
				wire["attributionWindow"] = int64(30 * time.Minute)
			}
		case "attribution longer guardrail":
			if metric.Key == "retention" {
				wire["attributionWindow"] = int64(72 * time.Hour)
			}
		}
	}
	json.MarshalWrite(w, wire)
}

var attributionCases = []string{"attribution match", "attribution missing", "attribution invalid", "attribution mismatch", "attribution sub-hour", "attribution overflow", "attribution longer guardrail"}
