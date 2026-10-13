package api

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/experiment"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/stats"
	"github.com/saker-ai/grayling/pkg/model"
)

const experimentSummaryPageSize = 20

type experimentReportSummary struct {
	Status      string        `json:"status"` // pending, ready, unavailable, restricted
	Healthy     bool          `json:"healthy"`
	Verdict     stats.Verdict `json:"verdict,omitempty"`
	Issue       string        `json:"issue,omitempty"`
	GeneratedAt *time.Time    `json:"generatedAt,omitempty"`
}

type experimentSummaryItem struct {
	Experiment *platform.Experiment    `json:"experiment"`
	Report     experimentReportSummary `json:"report"`
}

// handleExperimentSummaries bounds report computation to one visible page.
// Each report uses the same analysis path and permissions as the detail view.
func (s *Server) handleExperimentSummaries(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	list, err := s.Admin.ListExperiments(actor, r.PathValue("ws"), r.PathValue("env"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	query := r.URL.Query()
	limit := experimentSummaryPageSize
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 50 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 50")
			return
		}
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be non-negative")
			return
		}
	}
	search := strings.ToLower(strings.TrimSpace(query.Get("q")))
	state := query.Get("state")
	filtered := make([]*platform.Experiment, 0, len(list))
	for _, item := range list {
		if state != "" && string(item.State) != state {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(item.ID+" "+item.FlagKey+" "+item.CreatedBy), search) {
			continue
		}
		filtered = append(filtered, item)
	}
	slices.SortFunc(filtered, func(a, b *platform.Experiment) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	total := len(filtered)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	items := make([]experimentSummaryItem, 0, end-offset)
	workspace, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	canReadReport := admin.Authorize(actor, workspace.ID, admin.PermReportRead) == nil
	now := time.Now()
	for _, item := range filtered[offset:end] {
		row := experimentSummaryItem{Experiment: item, Report: experimentReportSummary{Status: "pending"}}
		switch {
		case !canReadReport:
			row.Report.Status = "restricted"
		case item.State == model.ExperimentDraft || item.State == model.ExperimentReviewed || item.State == model.ExperimentScheduled:
			// A report has no analysis population before the experiment starts.
		default:
			report, reportErr := experiment.BuildReport(s.Store, item.ID, now)
			if reportErr != nil {
				row.Report.Status = "unavailable"
			} else {
				row.Report = experimentReportSummary{Status: "ready", Healthy: report.Healthy,
					Verdict: report.Decision.Verdict, GeneratedAt: &report.GeneratedAt}
				if len(report.HealthIssues) > 0 {
					row.Report.Issue = report.HealthIssues[0]
				} else if report.PrimaryError != "" {
					row.Report.Issue = report.PrimaryError
				} else if len(report.Decision.Reasons) > 0 && report.Decision.Verdict == stats.VerdictHold {
					row.Report.Issue = report.Decision.Reasons[0]
				}
			}
		}
		items = append(items, row)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		Items  []experimentSummaryItem `json:"items"`
		Total  int                     `json:"total"`
		Offset int                     `json:"offset"`
		Limit  int                     `json:"limit"`
	}{items, total, offset, limit})
}
