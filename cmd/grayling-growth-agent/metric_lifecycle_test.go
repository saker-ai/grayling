package main

import (
	"encoding/json/v2"
	"net/http"
	"strings"

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
	wire := map[string]any{"workspaceId": metric.WorkspaceID, "key": metric.Key, "version": metric.Version, "hash": metric.Hash}
	if status != "missing" {
		if status == "unknown" {
			status = secret
		}
		wire["status"] = status
	}
	json.MarshalWrite(w, wire)
}
