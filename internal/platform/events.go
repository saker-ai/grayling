package platform

import (
	"fmt"
	"strings"
	"time"

	"github.com/godeps/golab/pkg/model"
)

// IngestResult reports what happened to one event batch (§10.1: 服务端去重、
// 处理迟到数据).
type IngestResult struct {
	Accepted   int      `json:"accepted"`
	Duplicates int      `json:"duplicates"`
	Rejected   int      `json:"rejected"`
	RejectMsgs []string `json:"rejectMessages,omitempty"`
}

// maxLateWindow bounds how old clientTime may be for attribution (§10.1:
// 按实验预注册窗口归因 — the platform-level hard bound; per-experiment
// attribution windows are applied at analysis time).
const maxLateWindow = 72 * time.Hour

// futureSkew bounds acceptable clock skew of clientTime.
const futureSkew = 5 * time.Minute

// IngestEvents records a batch of exposure/conversion events with server-side
// deduplication by event ID and incremental unit-level aggregation.
//
// Semantics:
//   - event_id seen before → counted as duplicate, otherwise ignored.
//   - exposures add the unit hash to the per-variant exposed set (unique
//     units drive SRM, never request counts — §10.1).
//   - conversions accumulate into UnitAgg keyed by
//     (experiment, epoch, variant, unitHash, metric): mean metrics need
//     per-unit sums; proportion metrics treat count>0 as converted.
//   - events with unknown experiment IDs are rejected (analysis is
//     pre-registered; no orphan buckets).
//   - raw events are retained up to MaxRetainedEvents with FIFO eviction;
//     aggregates are never evicted.
func (s *Store) IngestEvents(events []*Event) (IngestResult, error) {
	res := IngestResult{}
	if len(events) == 0 {
		return res, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for _, ev := range events {
		if err := s.validateEventLocked(ev, now); err != nil {
			res.Rejected++
			if len(res.RejectMsgs) < 20 {
				res.RejectMsgs = append(res.RejectMsgs, fmt.Sprintf("%s: %v", ev.EventID, err))
			}
			continue
		}
		if _, dup := s.data.EventIndex[ev.EventID]; dup {
			res.Duplicates++
			continue
		}
		ev.ServerTime = now
		s.appendEventLocked(ev)
		res.Accepted++
	}
	if res.Accepted > 0 {
		if err := s.saveLocked(); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *Store) validateEventLocked(ev *Event, now time.Time) error {
	if ev == nil {
		return fmt.Errorf("nil event")
	}
	if ev.EventID == "" {
		return fmt.Errorf("event_id required (idempotency)")
	}
	switch ev.Type {
	case EventExposure, EventConversion:
	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
	if ev.WorkspaceID == "" || ev.EnvironmentID == "" {
		return fmt.Errorf("workspace/environment required")
	}
	if ev.ExperimentID == "" || ev.Variant == "" || ev.UnitHash == "" {
		return fmt.Errorf("experimentId, variant and unitHash required")
	}
	exp, ok := s.data.Experiments[ev.ExperimentID]
	if !ok {
		return fmt.Errorf("unknown experiment %q", ev.ExperimentID)
	}
	if exp.WorkspaceID != ev.WorkspaceID || exp.EnvironmentID != ev.EnvironmentID {
		return fmt.Errorf("experiment %q does not belong to this workspace/environment", ev.ExperimentID)
	}
	if ev.Epoch != exp.Epoch {
		return fmt.Errorf("epoch %d does not match experiment epoch %d (mixed epochs are rejected, §10.1)",
			ev.Epoch, exp.Epoch)
	}
	if ev.Type == EventConversion && ev.MetricKey == "" {
		return fmt.Errorf("conversion events require metricKey")
	}
	// Late-data window: clientTime older than the retention bound or in the
	// future beyond skew is rejected — reports must be able to state a
	// complete window (§10.2 data-health conditions).
	if !ev.ClientTime.IsZero() {
		if ev.ClientTime.Before(now.Add(-maxLateWindow)) {
			return fmt.Errorf("clientTime %s beyond max late window %s", ev.ClientTime, maxLateWindow)
		}
		if ev.ClientTime.After(now.Add(futureSkew)) {
			return fmt.Errorf("clientTime %s too far in the future", ev.ClientTime)
		}
	}
	return nil
}

// appendEventLocked stores the event and updates aggregates. Caller holds mu.
func (s *Store) appendEventLocked(ev *Event) {
	// Lazy map init: store files written before a field existed unmarshal
	// with nil maps.
	if s.data.EventIndex == nil {
		s.data.EventIndex = map[string]int{}
	}
	if s.data.UnitAggs == nil {
		s.data.UnitAggs = map[string]*UnitAgg{}
	}
	if s.data.ExposedUnits == nil {
		s.data.ExposedUnits = map[string]map[string]bool{}
	}
	if s.data.SplitExposedUnits == nil {
		s.data.SplitExposedUnits = map[string]map[string]bool{}
	}
	if s.data.EventCounts == nil {
		s.data.EventCounts = map[string]int64{}
	}

	idx := len(s.data.Events)
	s.data.Events = append(s.data.Events, ev)
	s.data.EventIndex[ev.EventID] = idx

	// FIFO eviction beyond the cap (aggregates already absorbed the data).
	if len(s.data.Events) > MaxRetainedEvents {
		drop := len(s.data.Events) - MaxRetainedEvents
		for _, old := range s.data.Events[:drop] {
			delete(s.data.EventIndex, old.EventID)
		}
		s.data.Events = append([]*Event(nil), s.data.Events[drop:]...)
		// Rebuild index positions after the shift.
		for i, e := range s.data.Events {
			s.data.EventIndex[e.EventID] = i
		}
	}

	expScope := fmt.Sprintf("%s/%s/%s/%d/%s", ev.WorkspaceID, ev.EnvironmentID,
		ev.ExperimentID, ev.Epoch, ev.Variant)
	switch ev.Type {
	case EventExposure:
		set, ok := s.data.ExposedUnits[expScope]
		if !ok {
			set = map[string]bool{}
			s.data.ExposedUnits[expScope] = set
		}
		set[ev.UnitHash] = true
		if ev.Source == "split" {
			splitSet, ok := s.data.SplitExposedUnits[expScope]
			if !ok {
				splitSet = map[string]bool{}
				s.data.SplitExposedUnits[expScope] = splitSet
			}
			splitSet[ev.UnitHash] = true
		}
		s.data.EventCounts[expScope+"/exposure"]++
	case EventConversion:
		aggKey := expScope + "/" + ev.UnitHash + "/" + ev.MetricKey
		agg, ok := s.data.UnitAggs[aggKey]
		if !ok {
			agg = &UnitAgg{First: ev.ClientTime}
			s.data.UnitAggs[aggKey] = agg
		}
		agg.Count++
		agg.Sum += ev.Value
		agg.SumSquares += ev.Value * ev.Value
		agg.Last = ev.ClientTime
		s.data.EventCounts[expScope+"/conversion"]++
	}
}

// VariantStats is the aggregated analysis input for one variant.
type VariantStats struct {
	Variant string
	// ExposedUnits is the count of UNIQUE pseudonymized units with at least
	// one exposure — the SRM denominator (§10.1).
	ExposedUnits int64
	Exposures    int64
	Conversions  int64
}

// MetricAgg is the per-unit aggregate rollup for one (variant, metric).
type MetricAgg struct {
	Variant    string
	Metric     string
	N          int64   // units with >=1 conversion event
	Count      int64   // total conversion events (requests)
	Sum        float64 // Σ per-unit sums
	SumSquares float64 // Σ per-unit sums of squares — NOTE: sufficient for
	// mean/sum-of-squares stats only; quantile metrics need retained unit
	// detail (§10.1) which raw events provide until eviction.
	Units map[string]*UnitAgg // unitHash → per-unit stats
}

// ExperimentAggregate builds the analysis view for one experiment epoch:
// exposed unit counts per variant and per-metric unit aggregates.
func (s *Store) ExperimentAggregate(workspaceID, environmentID, experimentID string, epoch int64,
	metricKeys []string) map[string]*VariantStats {

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]*VariantStats{}
	prefix := fmt.Sprintf("%s/%s/%s/%d/", workspaceID, environmentID, experimentID, epoch)
	for scope, set := range s.data.ExposedUnits {
		if !strings.HasPrefix(scope, prefix) {
			continue
		}
		variant := scope[len(prefix):]
		st := out[variant]
		if st == nil {
			st = &VariantStats{Variant: variant}
			out[variant] = st
		}
		st.ExposedUnits = int64(len(set))
		st.Exposures = s.data.EventCounts[scope+"/exposure"]
		st.Conversions = s.data.EventCounts[scope+"/conversion"]
	}
	return out
}

// SplitExposedUnitCounts returns, per variant, the number of UNIQUE units
// exposed through the randomized split — the only valid SRM input (§10.1:
// SRM 基于进入实验且真实曝光的单元数，并与预设分配比较).
func (s *Store) SplitExposedUnitCounts(workspaceID, environmentID, experimentID string, epoch int64) map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int64{}
	prefix := fmt.Sprintf("%s/%s/%s/%d/", workspaceID, environmentID, experimentID, epoch)
	for scope, set := range s.data.SplitExposedUnits {
		if !strings.HasPrefix(scope, prefix) {
			continue
		}
		out[scope[len(prefix):]] = int64(len(set))
	}
	return out
}

// SplitExposedUnitSet returns the per-variant sets of pseudonymized unit
// hashes exposed through the randomized split. The analysis population of an
// experiment is this set intersected with conversions — deterministic
// (targeting/default) arms never enter primary statistics (§10.1).
func (s *Store) SplitExposedUnitSet(workspaceID, environmentID, experimentID string, epoch int64) map[string]map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]bool{}
	prefix := fmt.Sprintf("%s/%s/%s/%d/", workspaceID, environmentID, experimentID, epoch)
	for scope, set := range s.data.SplitExposedUnits {
		if !strings.HasPrefix(scope, prefix) {
			continue
		}
		out[scope[len(prefix):]] = set
	}
	return out
}

// MetricAggregate rolls per-unit aggregates up for one metric across
// variants. Proportion metrics: N = converted units, Count = conversion
// events. Mean metrics: callers compute per-unit means from Units.
func (s *Store) MetricAggregate(workspaceID, environmentID, experimentID string, epoch int64,
	metricKey string) map[string]*MetricAgg {

	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := fmt.Sprintf("%s/%s/%s/%d/", workspaceID, environmentID, experimentID, epoch)
	suffix := "/" + metricKey
	out := map[string]*MetricAgg{}
	for key, agg := range s.data.UnitAggs {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		mid := key[len(prefix) : len(key)-len(suffix)] // variant/unitHash
		parts := strings.SplitN(mid, "/", 2)
		if len(parts) != 2 {
			continue
		}
		variant, unit := parts[0], parts[1]
		ma, ok := out[variant]
		if !ok {
			ma = &MetricAgg{Variant: variant, Metric: metricKey, Units: map[string]*UnitAgg{}}
			out[variant] = ma
		}
		ma.N++
		ma.Count += agg.Count
		ma.Sum += agg.Sum
		ma.SumSquares += agg.SumSquares
		ma.Units[unit] = agg
	}
	return out
}

// EventCount returns total stored raw events (observability).
func (s *Store) EventCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Events)
}

// EventsForExperiment returns raw events of one experiment epoch, oldest
// first, optionally filtered by type ("" = all). Used by report builders
// that need unit detail beyond the aggregates.
func (s *Store) EventsForExperiment(workspaceID, environmentID, experimentID string, epoch int64,
	typ EventType) []*Event {

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Event{}
	for _, ev := range s.data.Events {
		if ev.WorkspaceID == workspaceID && ev.EnvironmentID == environmentID &&
			ev.ExperimentID == experimentID && ev.Epoch == epoch &&
			(typ == "" || ev.Type == typ) {
			out = append(out, ev)
		}
	}
	return out
}

// PruneEvents removes raw events older than the retention window (§10.2:
// 按保留期分区删除). Aggregates are kept — they are the sufficient
// statistics for in-flight and analyzed experiments.
func (s *Store) PruneEvents(retention time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-retention)
	kept := make([]*Event, 0, len(s.data.Events))
	removed := 0
	for _, ev := range s.data.Events {
		if ev.ServerTime.Before(cutoff) {
			delete(s.data.EventIndex, ev.EventID)
			removed++
			continue
		}
		kept = append(kept, ev)
	}
	if removed == 0 {
		return 0
	}
	s.data.Events = kept
	for i, e := range s.data.Events {
		s.data.EventIndex[e.EventID] = i
	}
	_ = s.saveLocked()
	return removed
}

// ExposureLedgerForUnit lists what a single unit was exposed to — used by
// session/task snapshot debugging and the console (§7).
func (s *Store) ExposureLedgerForUnit(workspaceID, unitHash string, limit int) []*Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Event{}
	for i := len(s.data.Events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := s.data.Events[i]
		if ev.Type == EventExposure && ev.WorkspaceID == workspaceID && ev.UnitHash == unitHash {
			out = append(out, ev)
		}
	}
	return out
}

// ExperimentBindingFor builds the flag-side binding of an experiment
// (convenience for the admin service when republishing flags on state
// transitions).
func (e *Experiment) ExperimentBinding() *model.ExperimentBinding {
	return &model.ExperimentBinding{
		ID: e.ID, Epoch: e.Epoch, Layer: e.Layer, LayerSlot: e.LayerSlot,
		Eligibility: e.Eligibility, State: e.State,
	}
}
