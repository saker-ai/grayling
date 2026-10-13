package admin

import (
	"cmp"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
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

// SegmentHistory contains retained published revisions and the revision in force.
type SegmentHistory struct {
	CurrentRevision int64                       `json:"currentRevision"`
	Revisions       []*platform.SegmentRevision `json:"revisions"`
}

func (s *Service) GetSegmentHistory(actor Actor, workspaceID, environmentID, key string) (*SegmentHistory, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, err
	}
	return &SegmentHistory{
		CurrentRevision: s.Store.CurrentSegmentRevision(env.WorkspaceID, env.ID, key),
		Revisions:       s.Store.ListSegmentRevisions(env.WorkspaceID, env.ID, key),
	}, nil
}

// SegmentDependencies reports published flags whose active rules reference a
// segment, including references through other published segments.
type SegmentDependencies struct {
	SegmentKey      string           `json:"segmentKey"`
	SegmentRevision int64            `json:"segmentRevision"`
	Flags           []FlagDependency `json:"flags"`
}

type FlagDependency struct {
	Key      string   `json:"key"`
	Revision int64    `json:"revision"`
	Via      []string `json:"via"`
}

func segmentReferencePath(ref, target string, segments map[string]*platform.SegmentRevision,
	seen map[string]bool) []string {
	if ref == target {
		return []string{ref}
	}
	if seen[ref] {
		return nil
	}
	segment := segments[ref]
	if segment == nil || segment.Content == nil {
		return nil
	}
	seen[ref] = true
	defer delete(seen, ref)
	for _, nested := range segment.Content.Segments {
		if path := segmentReferencePath(nested, target, segments, seen); len(path) > 0 {
			return append([]string{ref}, path...)
		}
	}
	return nil
}

func flagSegmentPath(flag *model.Flag, target string, segments map[string]*platform.SegmentRevision) []string {
	check := func(rule model.Rule) []string {
		for _, ref := range rule.Segments {
			if path := segmentReferencePath(ref, target, segments, map[string]bool{}); len(path) > 0 {
				return path
			}
		}
		return nil
	}
	if flag.DefaultRule != nil {
		if path := check(*flag.DefaultRule); len(path) > 0 {
			return path
		}
	}
	for _, rule := range flag.Targeting {
		if path := check(rule); len(path) > 0 {
			return path
		}
	}
	return nil
}

func (s *Service) GetSegmentDependencies(actor Actor, workspaceID, environmentID, key string) (*SegmentDependencies, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, err
	}
	segments := s.Store.CurrentSegmentContents(env.WorkspaceID, env.ID)
	current := segments[key]
	if current == nil {
		if _, err := s.Store.GetSegmentDraft(env.WorkspaceID, env.ID, key); err != nil {
			return nil, err
		}
	}
	result := &SegmentDependencies{SegmentKey: key, Flags: []FlagDependency{}}
	if current != nil {
		result.SegmentRevision = current.Revision
	}
	for _, revision := range s.Store.CurrentFlagContents(env.WorkspaceID, env.ID) {
		if revision == nil || revision.Content == nil {
			continue
		}
		if path := flagSegmentPath(revision.Content, key, segments); len(path) > 0 {
			result.Flags = append(result.Flags, FlagDependency{Key: revision.Key,
				Revision: revision.Revision, Via: path})
		}
	}
	slices.SortFunc(result.Flags, func(a, b FlagDependency) int { return cmp.Compare(a.Key, b.Key) })
	return result, nil
}

// SegmentMatchPreview compares a submitted editor document with the current
// published segment for exactly one diagnostic context. It is not an audience
// size estimate and has no exposure or collection side effects.
type SegmentMatchPreview struct {
	DraftMatched      bool  `json:"draftMatched"`
	PublishedMatched  *bool `json:"publishedMatched,omitempty"`
	PublishedRevision int64 `json:"publishedRevision"`
}

func (s *Service) PreviewSegmentMatch(actor Actor, workspaceID, environmentID, key string,
	content *model.Segment, evalCtx model.EvaluationContext) (*SegmentMatchPreview, error) {
	_, env, err := s.resolveEnv(workspaceID, environmentID, actor, PermFlagRead)
	if err != nil {
		return nil, err
	}
	if content == nil || content.Key != key {
		return nil, fmt.Errorf("%w: segment key does not match path", platform.ErrValidation)
	}
	published := s.Store.CurrentSegmentContents(env.WorkspaceID, env.ID)
	publishedSegments := make(map[string]*model.Segment, len(published))
	for name, revision := range published {
		if revision != nil && revision.Content != nil {
			publishedSegments[name] = revision.Content
		}
	}
	candidateSegments := maps.Clone(publishedSegments)
	candidateSegments[key] = content
	if err := model.ValidateSegment(content, candidateSegments); err != nil {
		return nil, fmt.Errorf("%w: %v", platform.ErrValidation, err)
	}
	match := func(segments map[string]*model.Segment) (bool, error) {
		evaluator, err := evaluation.NewEvaluator(&model.Snapshot{
			SchemaVersion: model.SchemaVersion, Workspace: env.WorkspaceID,
			Environment: env.ID, Segments: segments, Flags: map[string]*model.Flag{},
		})
		if err != nil {
			return false, fmt.Errorf("%w: %v", platform.ErrValidation, err)
		}
		return evaluator.MatchSegment(key, evalCtx)
	}
	draftMatched, err := match(candidateSegments)
	if err != nil {
		return nil, err
	}
	result := &SegmentMatchPreview{DraftMatched: draftMatched}
	if current := published[key]; current != nil {
		publishedMatched, err := match(publishedSegments)
		if err != nil {
			return nil, err
		}
		result.PublishedMatched = &publishedMatched
		result.PublishedRevision = current.Revision
	}
	return result, nil
}
