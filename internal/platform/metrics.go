package platform

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MetricStatus is the lifecycle of one immutable metric definition version.
type MetricStatus string

const (
	MetricDraft      MetricStatus = "draft"
	MetricPublished  MetricStatus = "published"
	MetricDeprecated MetricStatus = "deprecated"
)

// MetricDefinition is a reusable analysis contract. A new version is required
// for every change to its meaning; status transitions do not change its hash.
type MetricDefinition struct {
	WorkspaceID       string        `json:"workspaceId"`
	Key               string        `json:"key"`
	Version           int64         `json:"version"`
	DisplayName       string        `json:"displayName"`
	Description       string        `json:"description"`
	Owner             string        `json:"owner"`
	Source            string        `json:"source"`
	EventKeys         []string      `json:"eventKeys"`
	DedupUnit         string        `json:"dedupUnit"`
	AttributionWindow time.Duration `json:"attributionWindow"`
	Timezone          string        `json:"timezone,omitempty"`
	Spec              MetricSpec    `json:"spec"`
	Status            MetricStatus  `json:"status"`
	Hash              string        `json:"hash"`
	CreatedAt         time.Time     `json:"createdAt"`
	CreatedBy         string        `json:"createdBy"`
	PublishedAt       *time.Time    `json:"publishedAt,omitempty"`
	PublishedBy       string        `json:"publishedBy,omitempty"`
}

// MetricRef identifies the exact published definition frozen by an experiment.
type MetricRef struct {
	Key            string `json:"key"`
	Version        int64  `json:"version"`
	DefinitionHash string `json:"definitionHash,omitempty"`
}

var metricKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

func metricDefinitionKey(workspaceID, key string, version int64) string {
	return workspaceID + "\x00" + key + "\x00" + strconv.FormatInt(version, 10)
}

func cloneMetricDefinition(def *MetricDefinition) *MetricDefinition {
	copy := *def
	copy.EventKeys = slices.Clone(def.EventKeys)
	copy.Spec = cloneRegisteredMetric(def.Spec)
	if def.PublishedAt != nil {
		copy.PublishedAt = new(*def.PublishedAt)
	}
	return &copy
}

func validateMetricDefinition(def *MetricDefinition) error {
	if def == nil || !metricKeyRE.MatchString(def.Key) || def.WorkspaceID == "" {
		return fmt.Errorf("%w: invalid metric key or workspace", ErrValidation)
	}
	if strings.TrimSpace(def.DisplayName) == "" || strings.TrimSpace(def.Owner) == "" {
		return fmt.Errorf("%w: metric displayName and owner are required", ErrValidation)
	}
	if def.Source != "collect" || def.DedupUnit != "user" {
		return fmt.Errorf("%w: metric source must be collect and dedupUnit must be user", ErrValidation)
	}
	if def.AttributionWindow <= 0 || def.AttributionWindow > 30*24*time.Hour {
		return fmt.Errorf("%w: attributionWindow out of range", ErrValidation)
	}
	if len(def.EventKeys) == 0 || len(def.EventKeys) > 32 {
		return fmt.Errorf("%w: metric needs 1..32 event keys", ErrValidation)
	}
	seen := make(map[string]bool, len(def.EventKeys))
	for _, key := range def.EventKeys {
		if !metricKeyRE.MatchString(key) || seen[key] {
			return fmt.Errorf("%w: invalid or repeated event key %q", ErrValidation, key)
		}
		seen[key] = true
	}
	if def.Spec.Key == "" {
		def.Spec.Key = def.EventKeys[0]
	}
	if !slices.Contains(def.EventKeys, def.Spec.Key) {
		return fmt.Errorf("%w: metric spec key must be in eventKeys", ErrValidation)
	}
	if def.Spec.Direction != "increase" && def.Spec.Direction != "decrease" {
		return fmt.Errorf("%w: metric direction must be increase or decrease", ErrValidation)
	}
	switch def.Spec.Type {
	case "proportion", "mean":
	case "ratio":
		if def.Spec.DenominatorKey == "" || !seen[def.Spec.DenominatorKey] ||
			(def.Spec.NumeratorKey != "" && !seen[def.Spec.NumeratorKey]) {
			return fmt.Errorf("%w: ratio numerator and denominator must be eventKeys", ErrValidation)
		}
	case "quantile":
		if def.Spec.QuantileLevel <= 0 || def.Spec.QuantileLevel >= 1 {
			return fmt.Errorf("%w: quantileLevel out of range", ErrValidation)
		}
	case "funnel", "retention":
		if err := validateJourney(def.Spec.Type, def.Spec.Funnel, def.Spec.Retention); err != nil {
			return err
		}
		if def.Spec.Funnel != nil {
			for _, step := range def.Spec.Funnel.Steps {
				if !seen[step] {
					return fmt.Errorf("%w: funnel step %q must be in eventKeys", ErrValidation, step)
				}
			}
		}
		if def.Spec.Retention != nil && !seen[def.Spec.Retention.ReturnKey] {
			return fmt.Errorf("%w: retention returnKey must be in eventKeys", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unsupported metric type %q", ErrValidation, def.Spec.Type)
	}
	if def.Spec.Floor != nil && (math.IsNaN(*def.Spec.Floor) || math.IsInf(*def.Spec.Floor, 0)) {
		return fmt.Errorf("%w: invalid metric floor", ErrValidation)
	}
	if def.Spec.Ceil != nil && (math.IsNaN(*def.Spec.Ceil) || math.IsInf(*def.Spec.Ceil, 0)) {
		return fmt.Errorf("%w: invalid metric ceiling", ErrValidation)
	}
	if def.Spec.Floor != nil && def.Spec.Ceil != nil && *def.Spec.Floor > *def.Spec.Ceil {
		return fmt.Errorf("%w: metric floor exceeds ceiling", ErrValidation)
	}
	if def.Timezone != "" {
		if _, err := time.LoadLocation(def.Timezone); err != nil {
			return fmt.Errorf("%w: invalid metric timezone: %v", ErrValidation, err)
		}
	}
	return nil
}

func metricDefinitionHash(def *MetricDefinition) (string, error) {
	// Match the existing experiment registration fingerprint: MetricSpec has
	// time.Duration fields whose nanosecond wire representation is defined by
	// encoding/json v1. json/v2 rejects them without an explicit format.
	contract := struct {
		Key               string
		DisplayName       string
		Description       string
		Owner             string
		Source            string
		EventKeys         []string
		DedupUnit         string
		AttributionWindow time.Duration
		Timezone          string
		Spec              MetricSpec
	}{def.Key, def.DisplayName, def.Description, def.Owner, def.Source,
		def.EventKeys, def.DedupUnit, def.AttributionWindow, def.Timezone, def.Spec}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("%w: encode metric definition: %v", ErrValidation, err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

// ResolveExperimentMetricRefs returns an experiment with published metric
// versions expanded into immutable analysis snapshots. The store repeats this
// check under the creation lock, so deprecation cannot race registration.
func (s *Store) ResolveExperimentMetricRefs(exp *Experiment) (*Experiment, error) {
	if exp == nil {
		return nil, fmt.Errorf("%w: nil experiment", ErrValidation)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	copy := cloneRegisteredExperiment(exp)
	if err := s.resolveExperimentMetricRefsLocked(copy); err != nil {
		return nil, err
	}
	return copy, nil
}

func (s *Store) resolveExperimentMetricRefsLocked(exp *Experiment) error {
	resolve := func(ref *MetricRef) (*MetricDefinition, error) {
		if ref == nil || ref.Key == "" || ref.Version <= 0 {
			return nil, fmt.Errorf("%w: metric reference needs key and positive version", ErrValidation)
		}
		def := s.data.MetricDefinitions[metricDefinitionKey(exp.WorkspaceID, ref.Key, ref.Version)]
		if def == nil {
			return nil, fmt.Errorf("%w: metric %s v%d", ErrNotFound, ref.Key, ref.Version)
		}
		if def.Status != MetricPublished {
			return nil, fmt.Errorf("%w: metric %s v%d is not published", ErrInvalidState, ref.Key, ref.Version)
		}
		// The current report population uses the experiment's window for
		// conversion attribution. Until per-metric replay is implemented,
		// requiring equality prevents a catalog version from silently
		// producing results with a different window than it declares.
		if exp.AnalysisWindow <= 0 || def.AttributionWindow != exp.AnalysisWindow {
			return nil, fmt.Errorf("%w: metric %s attribution window must match experiment analysis window", ErrValidation, ref.Key)
		}
		retention := s.quotasLocked(exp.WorkspaceID).EventRetention
		headroom := maxLateWindow + 7*24*time.Hour
		if retention <= headroom || exp.AnalysisWindow > retention-headroom {
			return nil, fmt.Errorf("%w: event retention cannot cover analysis, late arrivals and replay buffer", ErrValidation)
		}
		hash, err := metricDefinitionHash(def)
		if err != nil || hash != def.Hash || (ref.DefinitionHash != "" && ref.DefinitionHash != def.Hash) {
			return nil, fmt.Errorf("%w: metric %s v%d definition hash mismatch", ErrConflict, ref.Key, ref.Version)
		}
		ref.DefinitionHash = def.Hash
		return def, nil
	}
	if exp.PrimaryMetricRef != nil {
		def, err := resolve(exp.PrimaryMetricRef)
		if err != nil {
			return err
		}
		exp.PrimaryMetric = cloneRegisteredMetric(def.Spec)
	}
	for i := range exp.Guardrails {
		g := &exp.Guardrails[i]
		if g.MetricRef == nil {
			continue
		}
		def, err := resolve(g.MetricRef)
		if err != nil {
			return err
		}
		g.MetricKey = def.Spec.Key
		g.Type = def.Spec.Type
		g.Direction = def.Spec.Direction
		g.Funnel = cloneFunnel(def.Spec.Funnel)
		g.Retention = cloneRetention(def.Spec.Retention)
	}
	return nil
}

// CreateMetricDefinition adds an immutable draft at the next version number.
func (s *Store) CreateMetricDefinition(input *MetricDefinition) (*MetricDefinition, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: nil metric definition", ErrValidation)
	}
	def := cloneMetricDefinition(input)
	if err := validateMetricDefinition(def); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Workspaces[def.WorkspaceID] == nil {
		return nil, fmt.Errorf("%w: workspace %q", ErrNotFound, def.WorkspaceID)
	}
	var last int64
	for _, existing := range s.data.MetricDefinitions {
		if existing.WorkspaceID == def.WorkspaceID && existing.Key == def.Key {
			last = max(last, existing.Version)
		}
	}
	if def.Version == 0 {
		def.Version = last + 1
	}
	if def.Version != last+1 {
		return nil, fmt.Errorf("%w: metric version must be %d", ErrConflict, last+1)
	}
	def.Status = MetricDraft
	def.CreatedAt = s.now()
	def.PublishedAt = nil
	def.PublishedBy = ""
	hash, err := metricDefinitionHash(def)
	if err != nil {
		return nil, err
	}
	def.Hash = hash
	key := metricDefinitionKey(def.WorkspaceID, def.Key, def.Version)
	s.data.MetricDefinitions[key] = def
	s.changes.markDirty(collMetricDefinitions, key)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return cloneMetricDefinition(def), nil
}

// GetMetricDefinition returns a copy, so callers cannot mutate the contract.
func (s *Store) GetMetricDefinition(workspaceID string, ref MetricRef) (*MetricDefinition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def := s.data.MetricDefinitions[metricDefinitionKey(workspaceID, ref.Key, ref.Version)]
	if def == nil {
		return nil, fmt.Errorf("%w: metric %s v%d", ErrNotFound, ref.Key, ref.Version)
	}
	return cloneMetricDefinition(def), nil
}

// ValidateFrozenMetricRef verifies a report's registered definition still
// matches the immutable catalog version. Deprecated versions remain valid for
// existing experiments; drafts are never valid report sources.
func (s *Store) ValidateFrozenMetricRef(workspaceID string, ref MetricRef) error {
	def, err := s.GetMetricDefinition(workspaceID, ref)
	if err != nil {
		return err
	}
	if def.Status != MetricPublished && def.Status != MetricDeprecated {
		return fmt.Errorf("%w: metric %s v%d is not a published version", ErrInvalidState, ref.Key, ref.Version)
	}
	hash, err := metricDefinitionHash(def)
	if err != nil || ref.DefinitionHash == "" || hash != def.Hash || ref.DefinitionHash != def.Hash {
		return fmt.Errorf("%w: metric %s v%d differs from frozen definition", ErrConflict, ref.Key, ref.Version)
	}
	return nil
}

// ListMetricDefinitions lists every version in a workspace, newest first.
func (s *Store) ListMetricDefinitions(workspaceID string) []*MetricDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MetricDefinition, 0)
	for _, def := range s.data.MetricDefinitions {
		if def.WorkspaceID == workspaceID {
			out = append(out, cloneMetricDefinition(def))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key == out[j].Key {
			return out[i].Version > out[j].Version
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// PublishMetricDefinition makes a reviewed draft available for new experiments.
func (s *Store) PublishMetricDefinition(workspaceID string, ref MetricRef, actor string) (*MetricDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := metricDefinitionKey(workspaceID, ref.Key, ref.Version)
	def := s.data.MetricDefinitions[key]
	if def == nil {
		return nil, fmt.Errorf("%w: metric %s v%d", ErrNotFound, ref.Key, ref.Version)
	}
	if def.Status != MetricDraft {
		return nil, fmt.Errorf("%w: metric version is %s", ErrInvalidState, def.Status)
	}
	def.Status = MetricPublished
	now := s.now()
	def.PublishedAt = &now
	def.PublishedBy = actor
	s.changes.markDirty(collMetricDefinitions, key)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return cloneMetricDefinition(def), nil
}

// PublishMetricDefinitionWithApproval consumes the exact reviewed grant and
// publishes its draft in one persistence transaction. A failed commit leaves
// both the definition and grant unchanged.
func (s *Store) PublishMetricDefinitionWithApproval(workspaceID string, ref MetricRef, actor, approvalID string) (*MetricDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.changes.empty() {
		return nil, fmt.Errorf("%w: pending uncommitted store changes", ErrConflict)
	}
	key := metricDefinitionKey(workspaceID, ref.Key, ref.Version)
	def := s.data.MetricDefinitions[key]
	if def == nil {
		return nil, fmt.Errorf("%w: metric %s v%d", ErrNotFound, ref.Key, ref.Version)
	}
	if def.Status != MetricDraft {
		return nil, fmt.Errorf("%w: metric version is %s", ErrInvalidState, def.Status)
	}
	hash, err := metricDefinitionHash(def)
	if err != nil || hash != def.Hash {
		return nil, fmt.Errorf("%w: metric definition hash changed", ErrConflict)
	}
	approval := s.data.Approvals[approvalID]
	if approval == nil {
		return nil, fmt.Errorf("%w: approval %s", ErrNotFound, approvalID)
	}
	token := s.data.Tokens[approval.ApprovedTokenID]
	if token == nil || token.Revoked || token.Kind != TokenAdmin ||
		(token.WorkspaceID != "" && token.WorkspaceID != workspaceID) ||
		(token.Role != RoleOwner && token.Role != RolePublisher) {
		return nil, fmt.Errorf("%w: approval credential revoked or missing", ErrAccessDenied)
	}
	previousApproval := copyApproval(approval)
	previousDefinition := cloneMetricDefinition(def)
	object := fmt.Sprintf("%s/v%d", ref.Key, ref.Version)
	if _, err := s.useApprovalLocked(approvalID, workspaceID, "", "metric.publish", object, def.Hash, ref.Version); err != nil {
		return nil, err
	}
	def.Status = MetricPublished
	now := s.now()
	def.PublishedAt = &now
	def.PublishedBy = actor
	s.changes.markDirty(collMetricDefinitions, key)
	if err := s.commitLocked(); err != nil {
		if !errors.Is(err, ErrConflict) {
			s.data.Approvals[approvalID] = previousApproval
			s.data.MetricDefinitions[key] = previousDefinition
			s.changes = newChangeSet()
		}
		return nil, err
	}
	return cloneMetricDefinition(def), nil
}

// DeprecateMetricDefinition prevents new experiment references while keeping
// the version available for existing reports and historical inspection.
func (s *Store) DeprecateMetricDefinition(workspaceID string, ref MetricRef) (*MetricDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := metricDefinitionKey(workspaceID, ref.Key, ref.Version)
	def := s.data.MetricDefinitions[key]
	if def == nil {
		return nil, fmt.Errorf("%w: metric %s v%d", ErrNotFound, ref.Key, ref.Version)
	}
	if def.Status != MetricPublished {
		return nil, fmt.Errorf("%w: metric version is %s", ErrInvalidState, def.Status)
	}
	def.Status = MetricDeprecated
	s.changes.markDirty(collMetricDefinitions, key)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return cloneMetricDefinition(def), nil
}
