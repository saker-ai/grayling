package api

import (
	"net/http"

	"github.com/godeps/grayling/internal/admin"
	"github.com/godeps/grayling/internal/platform"
)

// ---------------------------------------------------------------------------
// Staged-rollout release plans (自动推进, docs/技术方案.md §5.3, P4)
// ---------------------------------------------------------------------------
//
// Plans are pre-registered on an experiment and driven by the rollout
// engine (internal/rollout). The manual endpoints below are the operator
// surface: pause/resume, the 人工接管 switch and manual advance. Every
// action — automatic or manual — is recorded in the plan history and the
// immutable audit log.

// handleCreateReleasePlan pre-registers the staged-rollout plan.
// Body: {"steps":[{"name","ratio","minRuntime"(ns),"observationWindow"(ns),"autoAdvance"}]}
func (s *Server) handleCreateReleasePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Steps      []platform.ReleaseStep `json:"steps"`
		ApprovalID string                 `json:"approvalId"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.ValidateReleaseSteps(req.Steps); err != nil {
		writeStoreError(w, err)
		return
	}
	actor := actorFrom(r)
	exp, err := s.Store.GetExperiment(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermFlagPublish); err != nil {
		writeStoreError(w, err)
		return
	}
	if exp.ReleasePlan != nil {
		writeStoreError(w, platform.ErrConflict)
		return
	}
	err = s.Admin.WithScopeLock(exp.WorkspaceID, exp.EnvironmentID, func() error {
		current, err := s.Store.GetExperiment(exp.ID)
		if err != nil {
			return err
		}
		base := s.Store.CurrentFlagRevision(current.WorkspaceID, current.EnvironmentID, current.FlagKey)
		if err := s.useApproval(actor, req.ApprovalID, current.WorkspaceID, current.EnvironmentID,
			"release_plan.create", current.ID, struct {
				Steps []platform.ReleaseStep `json:"steps"`
			}{req.Steps}, base); err != nil {
			return err
		}
		exp, err = s.Admin.CreateReleasePlan(actor, current.ID, req.Steps)
		return err
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, exp.ReleasePlan)
}

// handleGetReleasePlan returns the plan with its action history.
func (s *Server) handleGetReleasePlan(w http.ResponseWriter, r *http.Request) {
	exp, err := s.Admin.GetExperiment(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if exp.ReleasePlan == nil {
		writeError(w, http.StatusNotFound, "not_found", "experiment has no release plan")
		return
	}
	writeJSON(w, http.StatusOK, exp.ReleasePlan)
}

func (s *Server) handlePauseReleasePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	exp, err := s.Rollout.Pause(actorFrom(r), r.PathValue("id"), req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp.ReleasePlan)
}

func (s *Server) handleResumeReleasePlan(w http.ResponseWriter, r *http.Request) {
	exp, err := s.Rollout.Resume(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp.ReleasePlan)
}

func (s *Server) handleAdvanceReleasePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = decodeJSON(r, &req, maxAdminBodyBytes) // body optional
	exp, err := s.Rollout.AdvanceNow(actorFrom(r), r.PathValue("id"), req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp.ReleasePlan)
}

// handleTakeoverReleasePlan flips the 人工接管开关: while enabled the engine
// takes no automatic action on the plan.
func (s *Server) handleTakeoverReleasePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	exp, err := s.Rollout.SetManualOverride(actorFrom(r), r.PathValue("id"), req.Enabled, req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp.ReleasePlan)
}
