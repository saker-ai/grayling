package admin

import (
	json "encoding/json/v2"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
)

// ReviewPreview is the evidence shown before an approval or publication.
// EstimatedAffected is deliberately an upper bound: GoLab has no complete
// census of SDK users, so a count would be misleading.
type ReviewPreview struct {
	TargetEnvironment string        `json:"targetEnvironment"`
	Risk              platform.Risk `json:"risk"`
	BaseRevision      int64         `json:"baseRevision"`
	CurrentHash       string        `json:"currentHash,omitempty"`
	ProposedHash      string        `json:"proposedHash"`
	ChangedFields     []string      `json:"changedFields"`
	EstimatedAffected string        `json:"estimatedAffected"`
	RollbackTarget    string        `json:"rollbackTarget"`
	Guardrails        string        `json:"guardrails"`
	ApprovalExpiresAt *time.Time    `json:"approvalExpiresAt,omitempty"`
}

func changedFields(before, after any) []string {
	oldRaw, _ := model.CanonicalJSON(before)
	newRaw, _ := model.CanonicalJSON(after)
	var oldMap, newMap map[string]any
	_ = json.Unmarshal(oldRaw, &oldMap)
	_ = json.Unmarshal(newRaw, &newMap)
	keys := map[string]bool{}
	for k := range oldMap {
		keys[k] = true
	}
	for k := range newMap {
		keys[k] = true
	}
	out := make([]string, 0)
	for k := range keys {
		if !reflect.DeepEqual(oldMap[k], newMap[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) flagPreview(env *platform.Environment, d *platform.FlagDraft, current *platform.FlagRevision) *ReviewPreview {
	p := &ReviewPreview{TargetEnvironment: env.WorkspaceID + "/" + env.ID + " (" + env.Slug + ")",
		Risk: d.Risk, BaseRevision: d.BaseRevision,
		EstimatedAffected: "up to 100% of evaluations of this flag; exact user count unavailable",
		Guardrails:        "distinct approver, 24-hour grant, content hash and revision check",
		ApprovalExpiresAt: d.ApprovalExpiresAt}
	if d.Deleted {
		p.EstimatedAffected = "all current users of this flag"
	}
	if current != nil {
		p.CurrentHash = current.Hash
		p.RollbackTarget = fmt.Sprintf("r%d (%s)", current.Revision, current.Hash)
		p.ChangedFields = changedFields(current.Content, d.Content)
	} else {
		p.RollbackTarget = "no prior revision"
		p.ChangedFields = changedFields(nil, d.Content)
	}
	if d.Deleted {
		p.ChangedFields = append(p.ChangedFields, "deleted")
	}
	p.ProposedHash = d.SubmittedHash
	if p.ProposedHash == "" {
		raw, _ := model.CanonicalJSON(d.Content)
		p.ProposedHash = model.HashBytes(raw)
	}
	return p
}

// GetSegmentPreview resolves a segment with the same authorization as flags.
func (s *Service) GetSegmentPreview(actor Actor, workspaceID, environmentID, key string) (*ReviewPreview, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, err
	}
	d, err := s.Store.GetSegmentDraft(env.WorkspaceID, env.ID, key)
	if err != nil {
		return nil, err
	}
	p := &ReviewPreview{TargetEnvironment: env.WorkspaceID + "/" + env.ID + " (" + env.Slug + ")",
		Risk: d.Risk, BaseRevision: d.BaseRevision,
		EstimatedAffected: "up to 100% of evaluations of flags referencing this segment; exact user count unavailable",
		Guardrails:        "distinct approver, 24-hour grant, content hash and revision check",
		ApprovalExpiresAt: d.ApprovalExpiresAt}
	if current := s.Store.CurrentSegmentContents(env.WorkspaceID, env.ID)[key]; current != nil {
		p.CurrentHash = current.Hash
		p.RollbackTarget = fmt.Sprintf("r%d (%s)", current.Revision, current.Hash)
		p.ChangedFields = changedFields(current.Content, d.Content)
	} else {
		p.RollbackTarget = "no prior revision"
		p.ChangedFields = changedFields(nil, d.Content)
	}
	p.ProposedHash = d.SubmittedHash
	if p.ProposedHash == "" {
		raw, _ := model.CanonicalJSON(d.Content)
		p.ProposedHash = model.HashBytes(raw)
	}
	return p, nil
}
