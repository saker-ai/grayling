package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
)

// ---------------------------------------------------------------------------
// GET /api/v1/sdk/config — scoped snapshot with ETag/304 (§8)
// ---------------------------------------------------------------------------
//
// The workspace/environment come from the token ONLY: there are no query
// parameters that could select another tenant (§8: 不接受 query 参数越权
// 选择租户). The response is the FULL server-side snapshot for that
// environment; browsers should use restricted projections or remote
// evaluation instead — SDK tokens are server-side credentials.

func (s *Server) handleSDKConfig(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	// Cheap freshness probe: read only the manifest, serve cache on match.
	manifest, err := s.Config.GetManifestContext(r.Context(), env.NamespaceID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	etag := snapshotETag(manifest.Revision)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	cached := s.lookupSnapshot(env.NamespaceID, manifest.Revision)
	if cached == nil {
		snapshot, err := s.Config.LoadSnapshotContext(r.Context(), env.NamespaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		// An evaluator build validates the whole snapshot; a broken
		// snapshot never reaches clients (they keep their last good one
		// via 304/ETag or the error below).
		ev, err := evaluation.NewEvaluator(snapshot)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "snapshot_invalid", err.Error())
			return
		}
		ev.EnableBandit()
		body, err := json.Marshal(snapshot)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "encode", err.Error())
			return
		}
		cached = &cachedSnapshot{
			revision: manifest.Revision, etag: etag, body: body, snapshot: snapshot,
		}
		s.snapshotCache.Store(env.NamespaceID, &evaluatorCacheEntry{
			revision: manifest.Revision, evaluator: ev, snapshot: cached,
		})
	}

	s.Hub.SetCurrent(tok.WorkspaceID, tok.EnvironmentID, manifest.Revision)
	w.Header().Set("ETag", cached.etag)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(cached.body)
}

func snapshotETag(revision int64) string {
	return fmt.Sprintf(`"golab-r%d"`, revision)
}

// ---------------------------------------------------------------------------
// GET /api/v1/sdk/revision — minimal freshness probe (§8 受限投影)
// ---------------------------------------------------------------------------
//
// Returns ONLY the current manifest revision of the token's scope — no
// flags, rules or segments. Remote-evaluation SDKs (Python/TypeScript,
// browser-adjacent runtimes) poll this endpoint to learn when their cached
// evaluation results were computed against a superseded configuration, so
// they can invalidate without ever downloading the full snapshot. An
// environment without a published manifest reports revision 0.

func (s *Server) handleSDKRevision(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var revision int64
	manifest, err := s.Config.GetManifestContext(r.Context(), env.NamespaceID)
	switch {
	case err == nil:
		revision = manifest.Revision
	default:
		// No manifest yet (or substrate read failure): report 0 so clients
		// treat their cache as unverifiable and re-evaluate remotely.
		revision = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": revision,
		"etag":     snapshotETag(revision),
	})
}

// evaluatorCacheEntry couples the serialized snapshot with its compiled
// evaluator, keyed by manifest revision.
type evaluatorCacheEntry struct {
	revision  int64
	evaluator *evaluation.Evaluator
	snapshot  *cachedSnapshot
}

func (s *Server) lookupSnapshot(namespaceID string, revision int64) *cachedSnapshot {
	if v, ok := s.snapshotCache.Load(namespaceID); ok {
		entry := v.(*evaluatorCacheEntry)
		if entry.revision == revision {
			return entry.snapshot
		}
	}
	return nil
}

// evaluatorFor returns a compiled evaluator at the CURRENT manifest
// revision, rebuilding the cache when the substrate moved.
func (s *Server) evaluatorFor(ctx context.Context, env *platform.Environment) (*evaluation.Evaluator, *model.Snapshot, int64, error) {
	manifest, err := s.Config.GetManifestContext(ctx, env.NamespaceID)
	if err != nil {
		return nil, nil, 0, err
	}
	if v, ok := s.snapshotCache.Load(env.NamespaceID); ok {
		entry := v.(*evaluatorCacheEntry)
		if entry.revision == manifest.Revision {
			return entry.evaluator, entry.snapshot.snapshot, manifest.Revision, nil
		}
	}
	snapshot, err := s.Config.LoadSnapshotContext(ctx, env.NamespaceID)
	if err != nil {
		return nil, nil, 0, err
	}
	ev, err := evaluation.NewEvaluator(snapshot)
	if err != nil {
		return nil, nil, 0, err
	}
	ev.EnableBandit()
	body, err := json.Marshal(snapshot)
	if err != nil {
		return nil, nil, 0, err
	}
	s.snapshotCache.Store(env.NamespaceID, &evaluatorCacheEntry{
		revision: manifest.Revision, evaluator: ev,
		snapshot: &cachedSnapshot{
			revision: manifest.Revision, etag: snapshotETag(manifest.Revision),
			body: body, snapshot: snapshot,
		},
	})
	return ev, snapshot, manifest.Revision, nil
}

// ---------------------------------------------------------------------------
// POST /api/v1/sdk/eval — remote evaluation (§8)
// ---------------------------------------------------------------------------

type evalRequest struct {
	FlagKey string                  `json:"flagKey"`
	Context model.EvaluationContext `json:"context"`
}

type evalResponse struct {
	Result     model.Result `json:"result"`
	Value      any          `json:"value"`
	Revision   int64        `json:"manifestRevision"`
	ExposureID string       `json:"exposureEventId,omitempty"`
	// RandomizationUnit echoes the canonical unit string the server-side
	// exposure was attributed to (present only when an exposure was
	// recorded). Remote-evaluation SDKs need it to report CONVERSIONS that
	// land on the same unit hash as the exposure (§10.1) — the value came
	// from the caller's own context, so echoing it reveals nothing new.
	RandomizationUnit string `json:"randomizationUnit,omitempty"`
}

type scopedDecision struct {
	workspaceID   string
	environmentID string
	diagnostic    model.DecisionDiagnostic
	conversionIDs map[string]bool
	unitHash      string
}

func (s *Server) rememberDecision(workspaceID, environmentID, unitHash string, d model.DecisionDiagnostic) {
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	if len(s.decisionOrder) >= 10_000 {
		delete(s.decisions, s.decisionOrder[0])
		s.decisionOrder = s.decisionOrder[1:]
	}
	id := d.Result.DecisionID
	s.decisions[id] = scopedDecision{workspaceID: workspaceID,
		environmentID: environmentID, diagnostic: d, unitHash: unitHash}
	s.decisionOrder = append(s.decisionOrder, id)
}

func (s *Server) handleSDKDecision(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	s.decisionMu.RLock()
	d, ok := s.decisions[r.PathValue("id")]
	s.decisionMu.RUnlock()
	if !ok || d.workspaceID != tok.WorkspaceID || d.environmentID != tok.EnvironmentID {
		writeError(w, http.StatusNotFound, "decision_not_found", "decision not found")
		return
	}
	writeJSON(w, http.StatusOK, d.diagnostic)
}

func (s *Server) handleSDKMetrics(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	exposures, conversions := s.Store.EventMetricTotals(tok.WorkspaceID, tok.EnvironmentID)
	writeJSON(w, http.StatusOK, map[string]int64{
		"exposures": exposures, "conversions": conversions,
	})
}

func (s *Server) handleSDKEval(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req evalRequest
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	ev, snapshot, revision, err := s.evaluatorFor(r.Context(), env)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	res := ev.Evaluate(req.FlagKey, req.Context)
	if err := s.pinRemoteBanditAssignment(env, snapshot, req, &res); err != nil {
		writeStoreError(w, err)
		return
	}
	res.DecisionID = "dec-" + rand.Text()
	res.ManifestRevision = revision
	generated := snapshot.GeneratedAt
	if generated.IsZero() {
		generated = time.Now()
	}
	res.SnapshotAgeMillis = max(0, time.Since(generated).Milliseconds())

	resp := evalResponse{Value: res.Value, Revision: revision}
	diagnostic := model.DecisionDiagnostic{ExposureStatus: "none"}

	// Server-side exposure capture: remote clients (browsers, thin
	// services) cannot be trusted to report exposures, so the server
	// records them itself when the evaluation enrolled the unit in a
	// running experiment. The event ID is deterministic per
	// (experiment, epoch, unit, variant) which makes repeated remote evals
	// idempotent — unique-unit analytics are unaffected (§10.1).
	if res.TrackEvents && res.Experiment != nil {
		if id, unit := s.recordRemoteExposure(tok, env, snapshot, req, res); id != "" {
			resp.ExposureID = id
			resp.RandomizationUnit = unit
			res.ExposureEventID = id
			diagnostic.ExposureStatus = "recorded"
		} else if res.Experiment.Source == "bandit" {
			writeError(w, http.StatusServiceUnavailable, "bandit_exposure_unavailable",
				"bandit assignment could not be recorded")
			return
		}
	}
	resp.Result = res
	diagnostic.Result = res
	unitHash := ""
	if resp.RandomizationUnit != "" {
		unitHash = s.Store.HashUnit(env.WorkspaceID, resp.RandomizationUnit)
	}
	s.rememberDecision(env.WorkspaceID, env.ID, unitHash, diagnostic)
	if model.SampleDecision(res.DecisionID, s.sampleRate) {
		select {
		case s.sampledEvents <- res.TelemetryEvent():
		default:
			s.droppedSamples.Add(1)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) pinRemoteBanditAssignment(env *platform.Environment, snapshot *model.Snapshot,
	req evalRequest, res *model.Result) error {
	if res.Experiment == nil || (res.Experiment.Source != "bandit" && res.Experiment.Source != "split") {
		return nil
	}
	flag := snapshot.Flags[res.FlagKey]
	if flag == nil || flag.Experiment == nil || flag.Experiment.Bandit == nil || flag.Bucketing == nil {
		return nil
	}
	raw, ok := req.Context.Field(flag.Bucketing.Unit)
	if !ok {
		return fmt.Errorf("bandit: missing randomization unit")
	}
	unit, ok := evaluation.CanonicalUnitString(raw)
	if !ok {
		return fmt.Errorf("bandit: invalid randomization unit")
	}
	exp, err := s.Store.GetExperiment(res.Experiment.ExperimentID)
	if err != nil {
		return err
	}
	pin, err := s.Store.PinBanditAssignment(exp, &platform.Event{ExperimentID: exp.ID, Epoch: exp.Epoch,
		UnitHash: s.Store.HashUnit(env.WorkspaceID, unit), Variant: res.Experiment.Variant,
		Source: res.Experiment.Source, PolicyVersion: res.Experiment.PolicyVersion,
		StateVersion: res.Experiment.StateVersion, AllocationProbability: res.Experiment.AllocationProbability,
		ClientTime: time.Now().UTC()})
	if err != nil {
		return err
	}
	value, ok := flag.Variations[pin.Variant]
	if !ok {
		return fmt.Errorf("bandit: pinned variant %q not in flag", pin.Variant)
	}
	res.VariationType, res.Value = pin.Variant, value
	res.Experiment.Variant, res.Experiment.Source = pin.Variant, pin.Source
	res.Experiment.PolicyVersion = pin.PolicyVersion
	res.Experiment.StateVersion = pin.StateVersion
	res.Experiment.AllocationProbability = pin.AllocationProbability
	return nil
}

func (s *Server) recordRemoteExposure(tok *platform.Token, env *platform.Environment,
	snapshot *model.Snapshot, req evalRequest, res model.Result) (string, string) {

	flag := snapshot.Flags[res.FlagKey]
	if flag == nil || flag.Bucketing == nil {
		return "", ""
	}
	unitRaw, ok := req.Context.Field(flag.Bucketing.Unit)
	if !ok {
		return "", ""
	}
	// Same canonicalization the evaluator hashes with, so server-captured
	// exposures and SDK-reported ones land on the identical unit hash.
	unitStr, ok := evaluation.CanonicalUnitString(unitRaw)
	if !ok {
		return "", ""
	}
	unitHash := s.Store.HashUnit(env.WorkspaceID, unitStr)
	eventID := fmt.Sprintf("srv-%s-%d-%s-%s", res.Experiment.ExperimentID, res.Experiment.Epoch,
		unitHash[:16], res.Experiment.Variant)
	flagRev := flagRevisionIn(snapshot, res.FlagKey)
	now := time.Now().UTC()
	ingested, err := s.Store.IngestEvents([]*platform.Event{{
		EventID: eventID, WorkspaceID: env.WorkspaceID, EnvironmentID: env.ID,
		Type: platform.EventExposure, ExperimentID: res.Experiment.ExperimentID,
		Epoch: res.Experiment.Epoch, FlagKey: res.FlagKey, FlagRevision: flagRev,
		Variant: res.Experiment.Variant, UnitHash: unitHash, Source: res.Experiment.Source,
		PolicyVersion: res.Experiment.PolicyVersion, StateVersion: res.Experiment.StateVersion,
		AllocationProbability: res.Experiment.AllocationProbability,
		ClientTime:            now,
	}})
	if err != nil || ingested.Accepted+ingested.Duplicates != 1 {
		s.logger.Printf("api: remote exposure ingest failed: result=%+v err=%v", ingested, err)
		return "", ""
	}
	return eventID, unitStr
}

func flagRevisionIn(snapshot *model.Snapshot, key string) int64 {
	if f, ok := snapshot.Flags[key]; ok && f.Version != "" {
		var rev int64
		if _, err := fmt.Sscanf(f.Version, "%d", &rev); err == nil {
			return rev
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// POST /api/v1/collect — batched exposure/conversion ingestion (§8, §10)
// ---------------------------------------------------------------------------

type collectEventDTO struct {
	EventID               string    `json:"eventId"`
	DecisionID            string    `json:"decisionId,omitempty"`
	Type                  string    `json:"type"`
	ExperimentID          string    `json:"experimentId"`
	Epoch                 int64     `json:"epoch"`
	FlagKey               string    `json:"flagKey"`
	FlagRevision          int64     `json:"flagRevision"`
	Variant               string    `json:"variant"`
	Unit                  string    `json:"unit"` // plaintext; HMAC'd server-side, never stored
	Source                string    `json:"source"`
	PolicyVersion         string    `json:"policyVersion,omitempty"`
	StateVersion          int64     `json:"stateVersion,omitempty"`
	AllocationProbability float64   `json:"allocationProbability,omitempty"`
	MetricKey             string    `json:"metricKey"`
	Value                 float64   `json:"value"`
	SessionID             string    `json:"sessionId"`
	TaskID                string    `json:"taskId"`
	ClientTime            time.Time `json:"clientTime"`
}

type collectRequest struct {
	Events []collectEventDTO `json:"events"`
}

func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	q := s.Store.GetQuotas(env.WorkspaceID)

	// Transport-level byte cap: batch size × per-event quota (§9: 采集端
	// 限制事件大小、速率).
	maxBody := int64(q.MaxEventsPerBatch) * int64(q.MaxEventBytes)
	if maxBody <= 0 || maxBody > 64*1024*1024 {
		maxBody = 64 * 1024 * 1024
	}
	var req collectRequest
	if err := decodeJSON(r, &req, maxBody); err != nil {
		writeStoreError(w, err)
		return
	}
	if len(req.Events) == 0 {
		writeJSON(w, http.StatusOK, platform.IngestResult{})
		return
	}
	if len(req.Events) > q.MaxEventsPerBatch {
		writeError(w, http.StatusBadRequest, "batch_too_large",
			fmt.Sprintf("%d events exceed batch quota %d", len(req.Events), q.MaxEventsPerBatch))
		return
	}

	events := make([]*platform.Event, 0, len(req.Events))
	skippedNoUnit := 0
	for _, dto := range req.Events {
		if dto.Unit == "" {
			// No unit → no attribution; reject loudly instead of storing
			// anonymous noise (§6).
			skippedNoUnit++
			continue
		}
		ev := &platform.Event{
			EventID: dto.EventID, WorkspaceID: env.WorkspaceID, EnvironmentID: env.ID,
			Type: platform.EventType(dto.Type), ExperimentID: dto.ExperimentID,
			Epoch: dto.Epoch, FlagKey: dto.FlagKey, FlagRevision: dto.FlagRevision,
			Variant:  dto.Variant,
			UnitHash: s.Store.HashUnit(env.WorkspaceID, dto.Unit),
			Source:   dto.Source, MetricKey: dto.MetricKey, Value: dto.Value,
			PolicyVersion: dto.PolicyVersion, StateVersion: dto.StateVersion,
			AllocationProbability: dto.AllocationProbability,
			SessionID:             dto.SessionID, TaskID: dto.TaskID, ClientTime: dto.ClientTime.UTC(),
		}
		// Older SDKs know only the registered percentage rule. Record their
		// assignment as an explicit fixed fallback with known propensity.
		if ev.Type == platform.EventExposure && ev.Source == "split" && ev.PolicyVersion == "" {
			if exp, err := s.Store.GetExperiment(ev.ExperimentID); err == nil &&
				exp.AllocationMode == "bandit" && exp.Bandit != nil {
				ev.PolicyVersion = exp.Bandit.PolicyVersion
				ev.AllocationProbability = exp.Proportions[ev.Variant] / 100
				ev.Source = "bandit-fallback"
			}
		}
		events = append(events, ev)
	}
	res, err := s.Store.IngestEvents(events)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if res.Accepted > 0 || res.Duplicates > 0 {
		s.decisionMu.Lock()
		for _, dto := range req.Events {
			if dto.DecisionID == "" || dto.Unit == "" {
				continue
			}
			if !s.Store.HasAcceptedEvent(dto.EventID, env.WorkspaceID, env.ID, platform.EventType(dto.Type)) {
				continue
			}
			d, ok := s.decisions[dto.DecisionID]
			if !ok || d.workspaceID != env.WorkspaceID || d.environmentID != env.ID {
				continue
			}
			exp := d.diagnostic.Result.Experiment
			if exp == nil || d.unitHash == "" || d.unitHash != s.Store.HashUnit(env.WorkspaceID, dto.Unit) ||
				exp.ExperimentID != dto.ExperimentID || exp.Epoch != dto.Epoch || exp.Variant != dto.Variant {
				continue
			}
			if dto.Type == "conversion" {
				if d.conversionIDs == nil {
					d.conversionIDs = make(map[string]bool)
				}
				if !d.conversionIDs[dto.EventID] {
					if len(d.conversionIDs) >= 1024 {
						d.diagnostic.ConversionsTruncated = true
					} else {
						d.diagnostic.ConversionCount++
						d.conversionIDs[dto.EventID] = true
					}
				}
			} else if dto.Type == "exposure" {
				d.diagnostic.ExposureStatus = "recorded"
			}
			s.decisions[dto.DecisionID] = d
		}
		s.decisionMu.Unlock()
	}
	msgs := res.RejectMsgs
	if skippedNoUnit > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"%d events skipped: missing unit (no attribution without a randomization unit)", skippedNoUnit))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": res.Accepted, "duplicates": res.Duplicates,
		"rejected": res.Rejected + skippedNoUnit, "rejectMessages": msgs,
	})
}
