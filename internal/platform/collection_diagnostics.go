package platform

import (
	"fmt"
	"strings"
)

// CollectionOutcome describes one collector decision without carrying the
// submitted unit or event ID into the persisted diagnostic dimensions.
type CollectionOutcome struct {
	ExperimentID string
	Status       string
	Code         string
}

type collectionCount struct {
	WorkspaceID   string
	EnvironmentID string
	ExperimentID  string
	Epoch         int64
	Code          string
	Count         int64
}

type collectionCounterStore interface {
	recordCollectionCounts([]collectionCount) error
	collectionCounts(workspaceID, environmentID, experimentID string, epoch int64) (map[string]int64, error)
}

// RecordCollectionOutcomes records attempt counts for known experiments in
// the caller's credential scope. Unknown or foreign experiment IDs are not
// persisted, preventing arbitrary high-cardinality diagnostic keys.
func (s *Store) RecordCollectionOutcomes(workspaceID, environmentID string, outcomes []CollectionOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	grouped := make(map[string]*collectionCount)
	for _, outcome := range outcomes {
		if !countableCollectionOutcome(outcome.Status, outcome.Code) {
			continue
		}
		exp := s.data.Experiments[outcome.ExperimentID]
		if exp == nil || exp.WorkspaceID != workspaceID || exp.EnvironmentID != environmentID {
			continue
		}
		key := collectionCountKey(workspaceID, environmentID, exp.ID, exp.Epoch, outcome.Code)
		if grouped[key] == nil {
			grouped[key] = &collectionCount{WorkspaceID: workspaceID, EnvironmentID: environmentID,
				ExperimentID: exp.ID, Epoch: exp.Epoch, Code: outcome.Code}
		}
		grouped[key].Count++
	}
	if len(grouped) == 0 {
		return nil
	}
	counts := make([]collectionCount, 0, len(grouped))
	for _, count := range grouped {
		counts = append(counts, *count)
	}
	if backend, ok := s.persist.(collectionCounterStore); ok {
		return backend.recordCollectionCounts(counts)
	}
	if s.data.CollectionCounts == nil {
		s.data.CollectionCounts = map[string]int64{}
	}
	for key, count := range grouped {
		s.data.CollectionCounts[key] += count.Count
	}
	s.changes.Meta = true
	return s.commitLocked()
}

// CollectionCounts returns durable per-reason attempt counts for one epoch.
func (s *Store) CollectionCounts(experimentID string) (map[string]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	exp := s.data.Experiments[experimentID]
	if exp == nil {
		return nil, ErrNotFound
	}
	if backend, ok := s.persist.(collectionCounterStore); ok {
		return backend.collectionCounts(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)
	}
	prefix := collectionCountKey(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, "")
	counts := make(map[string]int64)
	for key, count := range s.data.CollectionCounts {
		if code, ok := strings.CutPrefix(key, prefix); ok {
			counts[code] = count
		}
	}
	return counts, nil
}

func collectionCountKey(workspaceID, environmentID, experimentID string, epoch int64, code string) string {
	return fmt.Sprintf("%s/%s/%s/%d/%s", workspaceID, environmentID, experimentID, epoch, code)
}

func countableCollectionOutcome(status, code string) bool {
	if status == "duplicate" {
		return code == "duplicate_event"
	}
	if status != "rejected" {
		return false
	}
	switch code {
	case "invalid_event", "unknown_experiment", "stale_epoch", "assignment_mismatch",
		"missing_exposure", "missing_unit", "invalid_exposure_token",
		"expired_exposure_token", "late_event", "future_event":
		return true
	default:
		return false
	}
}
