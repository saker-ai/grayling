package platform

import (
	"crypto/sha256"
	"encoding/json"
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
	Key     string `json:"key"`
	Version int64  `json:"version"`
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
	if def.Spec.Key != "" && def.Spec.Key != def.Key {
		return fmt.Errorf("%w: metric spec key differs from definition key", ErrValidation)
	}
	def.Spec.Key = def.Key
	if def.Spec.Direction != "increase" && def.Spec.Direction != "decrease" {
		return fmt.Errorf("%w: metric direction must be increase or decrease", ErrValidation)
	}
	switch def.Spec.Type {
	case "proportion", "mean":
	case "ratio":
		if def.Spec.DenominatorKey == "" {
			return fmt.Errorf("%w: ratio metric needs denominatorKey", ErrValidation)
		}
	case "quantile":
		if def.Spec.QuantileLevel <= 0 || def.Spec.QuantileLevel >= 1 {
			return fmt.Errorf("%w: quantileLevel out of range", ErrValidation)
		}
	case "funnel", "retention":
		if err := validateJourney(def.Spec.Type, def.Spec.Funnel, def.Spec.Retention); err != nil {
			return err
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
