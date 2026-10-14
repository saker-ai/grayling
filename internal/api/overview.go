package api

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/experiment"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

const (
	// overviewAuditScanLimit bounds the audit records scanned for recent
	// releases. A full scan window is disclosed via coverage.complete=false;
	// the endpoint never invents a total beyond the records it scanned.
	overviewAuditScanLimit = 500
	// overviewRiskInspectMax matches the experiment-summaries page cap:
	// risks are computed for the most recently updated experiments only,
	// with the true full count reported as coverage.total.
	overviewRiskInspectMax = 50
	// overviewDefaultLimit is the default cap for pendingApprovals.items
	// and recentReleases.items.
	overviewDefaultLimit = 20
)

// overviewReleaseActions is the audit whitelist behind "recent releases".
// It covers every immutable record of a release-shaped effect: manual
// publishes, rollbacks, emergency disables, segment publishes and the
// engine/manual rollout steps (ApplyExperimentEligibility republishes via
// release_plan.apply_ratio and produces no flag.publish audit entry).
var overviewReleaseActions = map[string]bool{
	"flag.publish":             true,
	"flag.rollback":            true,
	"flag.emergency_disable":   true,
	"segment.publish":          true,
	"release_plan.apply_ratio": true,
	"release_plan.rollback":    true,
}

// overviewCoverage states honestly how much of the module's data source was
// inspected. complete=false means the answer may miss older or further-out
// records; no estimated totals are ever fabricated.
type overviewCoverage struct {
	Inspected int    `json:"inspected"`
	Total     int    `json:"total"`
	Complete  bool   `json:"complete"`
	Note      string `json:"note,omitempty"` // e.g. "audit-scan-window"
}

// overviewRiskItem is one server-owned risk fact. Text quotes the
// server-side reason verbatim (health issue, decision reason or the release
// plan's paused reason) and may be empty when the record carries none.
type overviewRiskItem struct {
	Kind          string `json:"kind"` // experiment_unhealthy|experiment_report_unavailable|experiment_paused|plan_paused|plan_rolled_back
	ID            string `json:"id"`
	EnvironmentID string `json:"environmentId"`
	Severity      string `json:"severity"` // block|warn
	Text          string `json:"text"`
}

type risksModule struct {
	Status         string             `json:"status"` // ok|partial|restricted|unavailable
	Reason         string             `json:"reason,omitempty"`
	ReportCoverage string             `json:"reportCoverage"` // complete|partial|unavailable|restricted
	Coverage       overviewCoverage   `json:"coverage"`
	Items          []overviewRiskItem `json:"items"`
}

// pendingApprovalItem carries the server's own verdict about what the caller
// may do (MyAction) or why they may not (BlockedReason). The console renders
// these fields instead of re-deriving them in the browser.
type pendingApprovalItem struct {
	ID            string     `json:"id"`
	Action        string     `json:"action"`
	Object        string     `json:"object"`
	EnvironmentID string     `json:"environmentId"`
	SubmittedBy   string     `json:"submittedBy"`
	SubmittedAt   time.Time  `json:"submittedAt"`
	Risk          string     `json:"risk"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	MyAction      string     `json:"myAction"`      // review|execute|"" — TokenID+Name dual self-approval exclusion
	BlockedReason string     `json:"blockedReason"` // self_approval|role|expired|breakglass_review|""
}

type pendingModule struct {
	Status          string                `json:"status"` // ok|unavailable
	Reason          string                `json:"reason,omitempty"`
	ActionableTotal int                   `json:"actionableTotal"` // true full-scan count, never truncated by limit
	Items           []pendingApprovalItem `json:"items"`           // truncated to limit
}

type releaseItem struct {
	Time          time.Time `json:"time"`
	Actor         string    `json:"actor"`
	Action        string    `json:"action"`
	Object        string    `json:"object"`
	EnvironmentID string    `json:"environmentId"`
}

type releasesModule struct {
	Status   string           `json:"status"` // ok|unavailable
	Reason   string           `json:"reason,omitempty"`
	Coverage overviewCoverage `json:"coverage"`
	Items    []releaseItem    `json:"items"`
}

// overviewCounts holds real counts; a null (nil) value means the underlying
// data source failed and the console renders "—" instead of a fake zero.
// PublishedFlags is only computed when an environment is given.
type overviewCounts struct {
	PausedExperiments  *int `json:"pausedExperiments"`
	RunningExperiments *int `json:"runningExperiments"`
	PendingApprovals   *int `json:"pendingApprovals"` // every submitted approval (workspace level)
	PublishedFlags     *int `json:"publishedFlags"`
}

type overviewResponse struct {
	GeneratedAt      time.Time      `json:"generatedAt"`
	WorkspaceID      string         `json:"workspaceId"`
	EnvironmentID    string         `json:"environmentId,omitempty"`
	Counts           overviewCounts `json:"counts"`
	Risks            risksModule    `json:"risks"`
	PendingApprovals pendingModule  `json:"pendingApprovals"`
	RecentReleases   releasesModule `json:"recentReleases"`
}

// handleWorkspaceOverview serves the console overview page's single read
// model: risk band, approvals waiting for the caller, recent releases and
// real counts. The endpoint gate is the lowest common permission
// (flag.read); each module then trims by capability and degrades
// independently — a failing data source yields 200 with a module-level
// status and reason, never a 5xx and never a fabricated total.
func (s *Server) handleWorkspaceOverview(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, ws.ID, admin.PermFlagRead); err != nil {
		writeStoreError(w, err)
		return
	}
	query := r.URL.Query()
	envID := ""
	if raw := query.Get("env"); raw != "" {
		env, envErr := s.Store.GetEnvironment(ws.ID, raw)
		if envErr != nil {
			writeStoreError(w, envErr) // unknown environment → 404
			return
		}
		envID = env.ID
	}
	limit := overviewDefaultLimit
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 50 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 50")
			return
		}
	}
	now := time.Now()
	resp := overviewResponse{GeneratedAt: now, WorkspaceID: ws.ID, EnvironmentID: envID}

	// Risks and the experiment counts share one authorized experiment list.
	// Experiments are environment-scoped; an env filter narrows both.
	exps, expErr := s.Admin.ListExperiments(actor, ws.ID, envID)
	if expErr != nil {
		resp.Risks = risksModule{
			Status: "unavailable", Reason: "experiments_unavailable",
			ReportCoverage: "unavailable", Items: []overviewRiskItem{},
		}
	} else {
		canReadReport := admin.Authorize(actor, ws.ID, admin.PermReportRead) == nil
		resp.Risks = buildOverviewRisks(actor, exps, canReadReport, now,
			func(id string, at time.Time) (*experiment.Report, error) {
				return experiment.BuildReport(s.Store, id, at)
			})
		paused, running := 0, 0
		for _, e := range exps {
			switch e.State {
			case model.ExperimentPaused:
				paused++
			case model.ExperimentRunning:
				running++
			}
		}
		resp.Counts.PausedExperiments = &paused
		resp.Counts.RunningExperiments = &running
	}

	// Approvals are workspace-level regardless of the env filter: hiding
	// other environments' pending grants would drop real work. Items carry
	// environmentId so the console can label them.
	approvals := s.Store.ListApprovals(ws.ID)
	canApprove := admin.Authorize(actor, ws.ID, admin.PermFlagApprove) == nil
	canPublish := admin.Authorize(actor, ws.ID, admin.PermFlagPublish) == nil
	resp.PendingApprovals = buildOverviewPending(actor, approvals, canApprove, canPublish, now, limit)
	submitted := 0
	for _, a := range approvals {
		if a.State == platform.DraftSubmitted {
			submitted++
		}
	}
	resp.Counts.PendingApprovals = &submitted

	// Recent releases come from a bounded newest-first audit scan filtered
	// through the release-action whitelist.
	entries := s.Store.AuditPage(ws.ID, 0, overviewAuditScanLimit, "")
	resp.RecentReleases = buildOverviewReleases(entries, envID, limit)

	if envID != "" {
		published := len(s.Store.CurrentFlagContents(ws.ID, envID))
		resp.Counts.PublishedFlags = &published
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// buildOverviewRisks inspects the most recently updated experiments (bounded
// by overviewRiskInspectMax) and reports server-owned risk facts verbatim.
// It never claims "no risks" beyond the coverage it states: without
// report.read only state/plan facts are listed (reportCoverage=restricted),
// and a failed report becomes an explicit risk item instead of silence.
func buildOverviewRisks(actor admin.Actor, exps []*platform.Experiment, canReadReport bool,
	now time.Time, buildReport func(string, time.Time) (*experiment.Report, error)) risksModule {
	_ = actor // risks carry no caller-specific trimming beyond canReadReport
	sorted := slices.Clone(exps)
	slices.SortFunc(sorted, func(a, b *platform.Experiment) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	total := len(sorted)
	inspected := min(total, overviewRiskInspectMax)
	mod := risksModule{
		Status: "ok", ReportCoverage: "complete",
		Coverage: overviewCoverage{Inspected: inspected, Total: total},
		Items:    []overviewRiskItem{},
	}
	if !canReadReport {
		// Defensive branch: every in-scope admin role holds report.read
		// today. Degrade honestly instead of asserting health.
		mod.Status = "restricted"
		mod.ReportCoverage = "restricted"
	}
	reportFailures := 0
	for _, e := range sorted[:inspected] {
		add := func(kind, severity, text string) {
			mod.Items = append(mod.Items, overviewRiskItem{
				Kind: kind, ID: e.ID, EnvironmentID: e.EnvironmentID,
				Severity: severity, Text: text,
			})
		}
		if e.State == model.ExperimentPaused {
			// The transition reason lives in the audit log, not on the
			// record; quote nothing rather than invent text.
			add("experiment_paused", "warn", "")
		}
		if plan := e.ReleasePlan; plan != nil {
			switch plan.State {
			case platform.ReleasePlanPaused:
				add("plan_paused", "warn", plan.PausedReason)
			case platform.ReleasePlanRolledBack:
				add("plan_rolled_back", "block", plan.PausedReason)
			}
		}
		if canReadReport && !experimentPreLaunch(e.State) {
			report, err := buildReport(e.ID, now)
			switch {
			case err != nil:
				reportFailures++
				add("experiment_report_unavailable", "warn", err.Error())
			case !report.Healthy:
				add("experiment_unhealthy", "block", reportIssueText(report))
			}
		}
	}
	complete := total <= inspected && reportFailures == 0
	mod.Coverage.Complete = complete
	if reportFailures > 0 {
		mod.ReportCoverage = "partial"
	}
	if canReadReport && (reportFailures > 0 || !complete) {
		mod.Status = "partial"
	}
	return mod
}

// experimentPreLaunch reports whether an experiment has no analysis
// population yet (mirrors handleExperimentSummaries' pending branch).
func experimentPreLaunch(state model.ExperimentState) bool {
	return state == model.ExperimentDraft ||
		state == model.ExperimentReviewed ||
		state == model.ExperimentScheduled
}

// buildOverviewPending scans every workspace approval and mirrors the
// server-side action rules. Self-approval exclusion uses the same dual test
// as ApproveChange/RejectChange (TokenID primary, name fallback); role gates
// mirror the approve/publish RBAC. ActionableTotal counts the full scan and
// is never truncated by limit — Items is.
func buildOverviewPending(actor admin.Actor, approvals []*platform.Approval,
	canApprove, canPublish bool, now time.Time, limit int) pendingModule {
	mod := pendingModule{Status: "ok", Items: []pendingApprovalItem{}}
	for _, a := range approvals {
		if a.State != platform.DraftSubmitted && a.State != platform.DraftApproved {
			continue // rejected/used grants are history, not pending work
		}
		item := pendingApprovalItem{
			ID: a.ID, Action: a.Action, Object: a.Object, EnvironmentID: a.EnvironmentID,
			SubmittedBy: a.SubmittedBy, SubmittedAt: a.SubmittedAt, Risk: string(a.Risk),
			ExpiresAt: a.ExpiresAt,
		}
		selfApproval := actor.TokenID == a.SubmittedTokenID || actor.Name == a.SubmittedBy
		switch a.State {
		case platform.DraftSubmitted:
			switch {
			case selfApproval:
				item.BlockedReason = "self_approval"
			case canApprove:
				item.MyAction = "review"
			default:
				item.BlockedReason = "role"
			}
		case platform.DraftApproved:
			// Mirrors UseApproval: a grant without an expiry is unusable.
			expired := a.ExpiresAt == nil || !now.Before(*a.ExpiresAt)
			switch {
			case expired:
				item.BlockedReason = "expired"
			case a.Action == "breakglass.review":
				// Emergency post-reviews are resolved in the approvals
				// inbox, never executed from the overview.
				item.BlockedReason = "breakglass_review"
			case canPublish:
				item.MyAction = "execute"
			default:
				item.BlockedReason = "role"
			}
		}
		if item.MyAction != "" {
			mod.ActionableTotal++
		}
		if len(mod.Items) < limit {
			mod.Items = append(mod.Items, item)
		}
	}
	return mod
}

// buildOverviewReleases filters a bounded newest-first audit scan through
// the release-action whitelist, optionally narrowing to one environment.
// Coverage reports the scan window honestly: when the window is full,
// complete=false and total stays at the number of records actually scanned.
func buildOverviewReleases(entries []*platform.AuditEntry, envID string, limit int) releasesModule {
	mod := releasesModule{
		Status: "ok",
		Coverage: overviewCoverage{
			Inspected: len(entries), Total: len(entries),
			Complete: len(entries) < overviewAuditScanLimit,
			Note:     "audit-scan-window",
		},
		Items: []releaseItem{},
	}
	for _, e := range entries {
		if !overviewReleaseActions[e.Action] {
			continue
		}
		if envID != "" && e.EnvironmentID != envID {
			continue
		}
		if len(mod.Items) >= limit {
			break
		}
		mod.Items = append(mod.Items, releaseItem{
			Time: e.Time, Actor: e.Actor, Action: e.Action,
			Object: e.Object, EnvironmentID: e.EnvironmentID,
		})
	}
	return mod
}
