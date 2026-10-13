package api

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/experiment"
	"github.com/saker-ai/grayling/internal/platform"
)

func (s *Server) authorizedExperimentReport(r *http.Request) (*platform.Experiment, *experiment.Report, error) {
	actor := actorFrom(r)
	exp, err := s.Admin.GetExperiment(actor, r.PathValue("id"))
	if err != nil {
		return nil, nil, err
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermReportRead); err != nil {
		return nil, nil, err
	}
	report, err := experiment.BuildReport(s.Store, exp.ID, time.Now())
	if err != nil {
		return nil, nil, err
	}
	return exp, report, nil
}

// handleExperimentReportExport returns one recomputed, scoped report snapshot.
// CSV contains aggregate variant rows only; JSON preserves the full report.
func (s *Server) handleExperimentReportExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "json" && format != "csv" {
		writeError(w, http.StatusBadRequest, "invalid_format", "format must be json or csv")
		return
	}
	exp, report, err := s.authorizedExperimentReport(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="grayling-report.json"`)
		writeJSON(w, http.StatusOK, struct {
			WorkspaceID   string             `json:"workspaceId"`
			EnvironmentID string             `json:"environmentId"`
			Report        *experiment.Report `json:"report"`
		}{exp.WorkspaceID, exp.EnvironmentID, report})
		return
	}
	data, err := reportCSV(exp, report)
	if err != nil {
		writeStoreError(w, fmt.Errorf("report export: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="grayling-report.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func reportCSV(exp *platform.Experiment, report *experiment.Report) ([]byte, error) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	rows := [][]string{{
		"workspace_id", "environment_id", "experiment_id", "epoch", "registration_version",
		"primary_metric_key", "primary_metric_version", "primary_metric_hash", "analysis_window_version",
		"primary_attribution_window_ns",
		"generated_at", "window_start", "window_end", "watermark", "recomputable", "complete",
		"healthy", "health_issues", "verdict", "variant", "exposed_units", "randomized_units",
		"exposures", "converted_units", "primary_value",
	}}
	metricKey, metricVersion, metricHash := exp.PrimaryMetric.Key, "", ""
	if report.PrimaryMetricRef != nil {
		metricKey = report.PrimaryMetricRef.Key
		metricVersion = strconv.FormatInt(report.PrimaryMetricRef.Version, 10)
		metricHash = report.PrimaryMetricRef.DefinitionHash
	}
	for _, variant := range report.PerVariant {
		rows = append(rows, []string{
			csvCell(exp.WorkspaceID), csvCell(exp.EnvironmentID), csvCell(report.ExperimentID),
			strconv.FormatInt(report.Epoch, 10), csvCell(report.RegistrationVersion),
			csvCell(metricKey), metricVersion, csvCell(metricHash), csvCell(report.Window.Version),
			strconv.FormatInt(int64(report.PrimaryAttributionWindow), 10),
			report.GeneratedAt.Format(time.RFC3339Nano), report.Window.Start.Format(time.RFC3339Nano),
			report.Window.End.Format(time.RFC3339Nano), report.Window.Watermark.Format(time.RFC3339Nano),
			strconv.FormatBool(report.Window.Recomputable), strconv.FormatBool(report.Window.Complete),
			strconv.FormatBool(report.Healthy), csvCell(strings.Join(report.HealthIssues, " | ")),
			csvCell(string(report.Decision.Verdict)), csvCell(variant.Variant),
			strconv.FormatInt(variant.ExposedUnits, 10), strconv.FormatInt(variant.RandomizedUnits, 10),
			strconv.FormatInt(variant.Exposures, 10), strconv.FormatInt(variant.ConvertedUnits, 10),
			strconv.FormatFloat(variant.PrimaryValue, 'g', -1, 64),
		})
	}
	if err := writer.WriteAll(rows); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// csvCell prevents spreadsheet programs from interpreting user-controlled
// names, metric keys, or diagnostic text as formulas when a CSV is opened.
func csvCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
