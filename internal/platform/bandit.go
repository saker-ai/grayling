package platform

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// BanditEvents returns an event-ledger snapshot for policy replay. PostgreSQL
// uses the complete raw ledger; a truncated local ledger fails closed.
func (s *Store) BanditEvents(exp *Experiment, now time.Time) ([]*Event, error) {
	if pg, ok := s.persist.(interface {
		analysisEvents(*Experiment, time.Time) ([]*Event, int64, int64, bool, error)
	}); ok {
		events, total, durable, mismatch, err := pg.analysisEvents(exp, now.UTC())
		if err != nil {
			return nil, err
		}
		if total != durable || mismatch {
			return nil, fmt.Errorf("bandit: raw event ledger differs from aggregates (%d vs %d)", total, durable)
		}
		return events, nil
	}
	events := s.EventsForExperiment(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, "")
	prefix := fmt.Sprintf("%s/%s/%s/%d/", exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)
	s.mu.RLock()
	var total int64
	for key, count := range s.data.EventCounts {
		if strings.HasPrefix(key, prefix) {
			total += count
		}
	}
	s.mu.RUnlock()
	if int64(len(events)) != total {
		return nil, fmt.Errorf("bandit: local raw event ledger truncated (%d of %d)", len(events), total)
	}
	return events, nil
}

// ExperimentsWithBandit returns active contracts in stable order for the
// background policy publisher.
func (s *Store) ExperimentsWithBandit() []*Experiment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Experiment, 0)
	for _, exp := range s.data.Experiments {
		if exp.AllocationMode == "bandit" && exp.State == "running" {
			cp := *exp
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *Experiment) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// BanditAnalysisView creates per-unit aggregates from events attributed to
// the first randomized exposure and the registered conversion window.
func BanditAnalysisView(exp *Experiment, events []*Event, now time.Time) *Store {
	start := exp.CreatedAt
	if exp.StartedAt != nil {
		start = *exp.StartedAt
	}
	first := make(map[string]*Event)
	for _, ev := range events {
		if ev.Type != EventExposure || (ev.Source != "bandit" && ev.Source != "split" && ev.Source != "bandit-fallback") ||
			ev.ClientTime.Before(start) || ev.ClientTime.After(now) {
			continue
		}
		if prior := first[ev.UnitHash]; prior == nil || ev.ClientTime.Before(prior.ClientTime) {
			first[ev.UnitHash] = ev
		}
	}
	filtered := make([]*Event, 0, len(events))
	for _, ev := range events {
		if ev.Type == EventExposure {
			if first[ev.UnitHash] == ev {
				filtered = append(filtered, ev)
			}
			continue
		}
		if ev.Type != EventConversion {
			continue
		}
		exposure := first[ev.UnitHash]
		if exposure != nil && ev.Variant == exposure.Variant && !ev.ClientTime.Before(exposure.ClientTime) &&
			ev.ClientTime.Before(exposure.ClientTime.Add(exp.AnalysisWindow)) && !ev.ClientTime.After(now) {
			filtered = append(filtered, ev)
		}
	}
	return buildAnalysisStore(filtered, AnalysisWindowStatus{Start: start, End: now.Add(time.Nanosecond)})
}

// PinBanditAssignment returns the first assignment for one randomization
// unit. PostgreSQL's unique key serializes concurrent evaluations on replicas.
func (s *Store) PinBanditAssignment(exp *Experiment, candidate *Event) (*Event, error) {
	if pg, ok := s.persist.(interface {
		pinBanditAssignment(*Experiment, *Event) (*Event, error)
	}); ok {
		return pg.pinBanditAssignment(exp, candidate)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.banditPins == nil {
		s.banditPins = make(map[string]*Event)
		for _, ev := range s.data.Events {
			if ev.Type != EventExposure || (ev.Source != "bandit" && ev.Source != "split" && ev.Source != "bandit-fallback") {
				continue
			}
			key := fmt.Sprintf("%s/%d/%s", ev.ExperimentID, ev.Epoch, ev.UnitHash)
			if prior := s.banditPins[key]; prior == nil || ev.ClientTime.Before(prior.ClientTime) {
				s.banditPins[key] = ev
			}
		}
	}
	key := fmt.Sprintf("%s/%d/%s", exp.ID, exp.Epoch, candidate.UnitHash)
	if pin := s.banditPins[key]; pin != nil {
		cp := *pin
		return &cp, nil
	}
	cp := *candidate
	s.banditPins[key] = &cp
	return &cp, nil
}
