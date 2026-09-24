// Package sdk is the GoLab Go SDK: local evaluation over a synchronized,
// validated configuration snapshot (docs/技术方案.md §3, §8).
//
// Data-plane contract:
//
//   - Hot path is LOCAL: evaluations never perform network I/O. The client
//     keeps an atomically-swapped, hash-validated snapshot of its
//     (workspace, environment) scope — derived from the credential, never
//     from caller input.
//   - Sync = full-snapshot pull with ETag/304, accelerated by the SSE
//     revision stream and backstopped by periodic polling, so a lost
//     notification can never cause permanent staleness (§3).
//   - A candidate snapshot is fully parsed AND evaluator-compiled before the
//     swap; invalid snapshots are rejected and the last good one is kept
//     (§5.2 atomic replacement).
//   - Offline behavior: the last good snapshot keeps serving and is marked
//     stale; once its age exceeds MaxStaleness, evaluations return the
//     caller's default with reason STALE — the bounded kill-switch window
//     operators must plan for (§5.2).
//   - Exposures ("真正使用/进入实验才曝光") are batched and flushed to
//     /api/v1/collect with a separate collect credential; failures are
//     dropped with an observable counter after bounded retries, never
//     blocking evaluation.
package sdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godeps/golab/pkg/evaluation"
	"github.com/godeps/golab/pkg/model"
)

// Errors observed by callers.
var (
	// ErrNotReady is reported (via Result.ErrorDetails) when no snapshot
	// has ever been synchronized.
	ErrNotReady = errors.New("sdk: no snapshot synchronized yet")
	// ErrStale is reported when the snapshot exceeded MaxStaleness.
	ErrStale = errors.New("sdk: snapshot exceeded max staleness; serving caller default")
)

// Options configures a Client.
type Options struct {
	// BaseURL of the GoLab server, e.g. http://127.0.0.1:8688
	BaseURL string
	// Token is an SDK-scope credential; workspace/environment are derived
	// from it server-side.
	Token string
	// CollectToken is the separate collection credential (§9). When empty,
	// exposure reporting is disabled and events are counted as dropped.
	CollectToken string

	// PollInterval bounds staleness when SSE is unavailable (default 30s).
	PollInterval time.Duration
	// MaxStaleness is how long an out-of-date snapshot may keep serving
	// before evaluations fall back to caller defaults (default 5m). This is
	// the operator-visible kill-switch ceiling for offline nodes.
	MaxStaleness time.Duration
	// SyncTimeout bounds one config fetch (default 10s).
	SyncTimeout time.Duration

	// ExposureFlushInterval (default 3s) and ExposureBatchSize (default
	// 100) drive the async exposure pipeline.
	ExposureFlushInterval time.Duration
	ExposureBatchSize     int
	// ExposureQueueCap bounds the in-memory exposure queue (default 10k);
	// on overflow the oldest events are dropped and counted.
	ExposureQueueCap int

	// HTTPClient override (tests, custom transport).
	HTTPClient *http.Client
	// Logger for sync/exposure diagnostics (default: discard).
	Logger *log.Logger
	// OnSyncError is invoked (async) whenever a background sync fails.
	OnSyncError func(err error)
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.PollInterval <= 0 {
		out.PollInterval = 30 * time.Second
	}
	if out.MaxStaleness <= 0 {
		out.MaxStaleness = 5 * time.Minute
	}
	if out.SyncTimeout <= 0 {
		out.SyncTimeout = 10 * time.Second
	}
	if out.ExposureFlushInterval <= 0 {
		out.ExposureFlushInterval = 3 * time.Second
	}
	if out.ExposureBatchSize <= 0 {
		out.ExposureBatchSize = 100
	}
	if out.ExposureQueueCap <= 0 {
		out.ExposureQueueCap = 10_000
	}
	if out.HTTPClient == nil {
		out.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if out.Logger == nil {
		out.Logger = log.New(io.Discard, "", 0)
	}
	return out
}

// Client is safe for concurrent use.
type Client struct {
	opts  Options
	httpc *http.Client

	// internalCtx is cancelled by Close so in-flight SSE/poll requests
	// abort immediately instead of waiting for the next server heartbeat.
	internalCtx    context.Context
	internalCancel context.CancelFunc

	mu        sync.RWMutex
	state     *clientState // atomically swapped
	etag      string
	lastSync  time.Time
	lastErr   error
	syncCount int64

	exposures        chan *collectEvent
	droppedExposures atomic.Int64
	flushedExposures atomic.Int64
	notModified      atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// clientState is the immutable evaluation core.
type clientState struct {
	snapshot  *model.Snapshot
	evaluator *evaluation.Evaluator
	fetchedAt time.Time
}

// Status reports synchronization health (observability, §11).
type Status struct {
	Ready            bool          `json:"ready"`
	Stale            bool          `json:"stale"`
	ManifestRevision int64         `json:"manifestRevision"`
	Age              time.Duration `json:"age"`
	LastSync         time.Time     `json:"lastSync"`
	LastError        string        `json:"lastError,omitempty"`
	SyncCount        int64         `json:"syncCount"`
	NotModifiedCount int64         `json:"notModifiedCount"`
	FlagCount        int           `json:"flagCount"`
	DroppedExposures int64         `json:"droppedExposures"`
	FlushedExposures int64         `json:"flushedExposures"`
}

// New creates a client and performs the initial synchronous fetch. A failed
// initial fetch is NOT fatal (§3: cold start returns caller defaults and
// reports the reason) — check Status()/the returned error and let Start
// keep retrying. An environment without any published manifest (HTTP 404)
// initializes an EMPTY snapshot: the client is ready and converges when the
// first manifest appears.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.BaseURL == "" || opts.Token == "" {
		return nil, errors.New("sdk: BaseURL and Token are required")
	}
	o := opts.withDefaults()
	internalCtx, internalCancel := context.WithCancel(context.Background())
	c := &Client{
		opts:           o,
		httpc:          o.HTTPClient,
		internalCtx:    internalCtx,
		internalCancel: internalCancel,
		exposures:      make(chan *collectEvent, o.ExposureQueueCap),
		closed:         make(chan struct{}),
	}
	err := c.Sync(ctx)
	return c, err
}

// mergedCtx returns a context cancelled when EITHER ctx or Close fires.
func (c *Client) mergedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-c.internalCtx.Done():
			cancel()
		case <-merged.Done():
		}
	}()
	return merged, cancel
}

// Start launches the SSE listener, polling fallback and exposure flusher.
// All goroutines stop when ctx is cancelled or Close is called.
func (c *Client) Start(ctx context.Context) {
	runCtx, cancel := c.mergedCtx(ctx)
	c.wg.Add(3)
	go func() { defer c.wg.Done(); defer cancel(); c.streamLoop(runCtx) }()
	go func() { defer c.wg.Done(); c.pollLoop(runCtx) }()
	go func() { defer c.wg.Done(); c.exposureLoop(runCtx) }()
}

// Close stops all background goroutines and flushes pending exposures once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.internalCancel() // abort in-flight SSE/poll requests immediately
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.flushExposures(ctx)
		c.wg.Wait()
	})
}

// Status returns current sync health.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := Status{
		SyncCount:        c.syncCount,
		NotModifiedCount: c.notModified.Load(),
		DroppedExposures: c.droppedExposures.Load(),
		FlushedExposures: c.flushedExposures.Load(),
		LastSync:         c.lastSync,
	}
	if c.lastErr != nil {
		st.LastError = c.lastErr.Error()
	}
	if c.state != nil {
		st.Ready = true
		st.ManifestRevision = c.state.snapshot.ManifestRevision
		st.Age = time.Since(c.state.fetchedAt)
		st.FlagCount = len(c.state.snapshot.Flags)
		st.Stale = st.Age > c.opts.MaxStaleness
	}
	return st
}

// Snapshot returns the current snapshot (read-only) for debugging.
func (c *Client) Snapshot() *model.Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state == nil {
		return nil
	}
	return c.state.snapshot
}

// Sync pulls the scoped snapshot with ETag revalidation and atomically
// swaps in the validated candidate. Safe to call concurrently.
func (c *Client) Sync(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, c.opts.SyncTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sctx, http.MethodGet,
		strings.TrimRight(c.opts.BaseURL, "/")+"/api/v1/sdk/config", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	c.mu.RLock()
	etag := c.etag
	c.mu.RUnlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		c.recordSyncError(err)
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		// The environment has no published manifest yet. Install an empty
		// snapshot so evaluations answer FLAG_NOT_FOUND deterministically
		// and the client converges when the first publish lands. The ETag
		// stays empty so the next sync always revalidates fully.
		c.mu.Lock()
		if c.state == nil {
			// Placeholder scope identifiers: the empty snapshot carries no
			// flags, so hash coordinates are unreachable; the real manifest
			// (revision ≥ 1) always supersedes revision 0.
			empty := &model.Snapshot{
				SchemaVersion: model.SchemaVersion,
				Workspace:     "empty", Environment: "empty",
				Flags:    map[string]*model.Flag{},
				Segments: map[string]*model.Segment{},
			}
			ev, err := evaluation.NewEvaluator(empty)
			if err == nil {
				c.state = &clientState{snapshot: empty, evaluator: ev, fetchedAt: time.Now()}
				c.lastSync = time.Now()
				c.lastErr = nil
				c.syncCount++
			}
		}
		c.mu.Unlock()
		return nil
	case http.StatusNotModified:
		c.notModified.Add(1)
		c.mu.Lock()
		c.lastSync = time.Now()
		if c.state != nil {
			// Snapshot content unchanged; refresh fetchedAt only if we
			// still hold the same revision — the 304 confirms freshness,
			// which legitimately extends the staleness window (§5.2: a
			// confirmed-unchanged snapshot is NOT stale).
			c.state = &clientState{
				snapshot: c.state.snapshot, evaluator: c.state.evaluator,
				fetchedAt: time.Now(),
			}
		}
		c.mu.Unlock()
		return nil
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
		if err != nil {
			c.recordSyncError(err)
			return err
		}
		var snapshot model.Snapshot
		if err := json.Unmarshal(body, &snapshot); err != nil {
			c.recordSyncError(fmt.Errorf("sdk: parse snapshot: %w", err))
			return errors.New("sdk: parse snapshot")
		}
		ev, err := evaluation.NewEvaluator(&snapshot)
		if err != nil {
			// Reject the candidate wholesale; keep serving the last good
			// snapshot (§5.2).
			c.recordSyncError(fmt.Errorf("sdk: invalid snapshot rejected: %w", err))
			return err
		}
		newEtag := resp.Header.Get("ETag")
		c.mu.Lock()
		// Ignore out-of-order/downgrade responses (§5.2: SDK 比对
		// revision/hash，忽略旧通知).
		if c.state != nil && snapshot.ManifestRevision < c.state.snapshot.ManifestRevision {
			c.mu.Unlock()
			return nil
		}
		c.state = &clientState{snapshot: &snapshot, evaluator: ev, fetchedAt: time.Now()}
		c.etag = newEtag
		c.lastSync = time.Now()
		c.lastErr = nil
		c.syncCount++
		c.mu.Unlock()
		return nil
	default:
		err := fmt.Errorf("sdk: config fetch: HTTP %d", resp.StatusCode)
		c.recordSyncError(err)
		return err
	}
}

func (c *Client) recordSyncError(err error) {
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
	c.opts.Logger.Printf("sdk: sync failed: %v", err)
	if c.opts.OnSyncError != nil {
		c.opts.OnSyncError(err)
	}
}

// streamLoop consumes the SSE revision stream; every event triggers a
// revalidation sync. Reconnects with backoff and Last-Event-ID.
func (c *Client) streamLoop(ctx context.Context) {
	backoff := time.Second
	lastEventID := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		default:
		}
		err := c.streamOnce(ctx, &lastEventID)
		if err != nil {
			c.opts.Logger.Printf("sdk: stream disconnected: %v (retry in %s)", err, backoff)
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, lastEventID *string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.opts.BaseURL, "/")+"/api/v1/sdk/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	req.Header.Set("Accept", "text/event-stream")
	if *lastEventID != "" {
		req.Header.Set("Last-Event-ID", *lastEventID)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream HTTP %d", resp.StatusCode)
	}
	// Any (re)connect resyncs: the server replays the current revision, and
	// even a missed event is covered by the polling fallback.
	if err := c.Sync(ctx); err != nil {
		c.opts.Logger.Printf("sdk: resync-on-connect failed: %v", err)
	}

	sc := newLineScanner(resp.Body)
	currentRevision := int64(-1)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return nil
		default:
		}
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			*lastEventID = strings.TrimSpace(line[4:])
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Revision int64 `json:"revision"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			continue
		}
		if ev.Revision > currentRevision {
			currentRevision = ev.Revision
			// Revalidation sync (cheap 304 when nothing changed).
			if err := c.Sync(ctx); err != nil {
				c.opts.Logger.Printf("sdk: sync after revision %d failed: %v", ev.Revision, err)
			}
		}
	}
	return sc.Err()
}

func (c *Client) pollLoop(ctx context.Context) {
	t := time.NewTicker(c.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, c.opts.SyncTimeout)
			err := c.Sync(sctx)
			cancel()
			if err != nil {
				c.opts.Logger.Printf("sdk: poll sync failed: %v", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

func (c *Client) currentState() (*clientState, *model.Result) {
	c.mu.RLock()
	state := c.state
	maxStale := c.opts.MaxStaleness
	c.mu.RUnlock()

	if state == nil {
		return nil, &model.Result{
			Reason: model.ReasonError, ErrorCode: model.ErrGeneral, Failed: true,
			ErrorDetails: ErrNotReady.Error(), Bucket: -1,
		}
	}
	if age := time.Since(state.fetchedAt); age > maxStale {
		// Kill-switch window exceeded: no config may be trusted (§5.2).
		return nil, &model.Result{
			Reason: model.ReasonStale, ErrorCode: model.ErrGeneral, Failed: true,
			ErrorDetails: fmt.Sprintf("%v (snapshot age %s > max %s)", ErrStale, age.Round(time.Millisecond), maxStale),
			Bucket:       -1,
		}
	}
	return state, nil
}

// Evaluate performs one local evaluation and records any experiment
// exposure asynchronously.
func (c *Client) Evaluate(flagKey string, evalCtx model.EvaluationContext) model.Result {
	state, fail := c.currentState()
	if fail != nil {
		res := *fail
		res.FlagKey = flagKey
		return res
	}
	res := state.evaluator.Evaluate(flagKey, evalCtx)
	if res.TrackEvents && res.Experiment != nil {
		c.queueExposure(state.snapshot, evalCtx, res)
	}
	return res
}

// BoolVariation resolves a bool flag with caller-default semantics.
func (c *Client) BoolVariation(flagKey string, evalCtx model.EvaluationContext, def bool) (bool, model.Result) {
	state, fail := c.currentState()
	if fail != nil {
		res := *fail
		res.FlagKey = flagKey
		return def, res
	}
	v, res := state.evaluator.BoolVariation(flagKey, evalCtx, def)
	if res.TrackEvents && res.Experiment != nil {
		c.queueExposure(state.snapshot, evalCtx, res)
	}
	return v, res
}

// StringVariation resolves a string flag.
func (c *Client) StringVariation(flagKey string, evalCtx model.EvaluationContext, def string) (string, model.Result) {
	state, fail := c.currentState()
	if fail != nil {
		res := *fail
		res.FlagKey = flagKey
		return def, res
	}
	v, res := state.evaluator.StringVariation(flagKey, evalCtx, def)
	if res.TrackEvents && res.Experiment != nil {
		c.queueExposure(state.snapshot, evalCtx, res)
	}
	return v, res
}

// NumberVariation resolves a number flag (float64).
func (c *Client) NumberVariation(flagKey string, evalCtx model.EvaluationContext, def float64) (float64, model.Result) {
	state, fail := c.currentState()
	if fail != nil {
		res := *fail
		res.FlagKey = flagKey
		return def, res
	}
	v, res := state.evaluator.NumberVariation(flagKey, evalCtx, def)
	if res.TrackEvents && res.Experiment != nil {
		c.queueExposure(state.snapshot, evalCtx, res)
	}
	return v, res
}

// JSONVariation resolves a json flag.
func (c *Client) JSONVariation(flagKey string, evalCtx model.EvaluationContext, def any) (any, model.Result) {
	state, fail := c.currentState()
	if fail != nil {
		res := *fail
		res.FlagKey = flagKey
		return def, res
	}
	v, res := state.evaluator.JSONVariation(flagKey, evalCtx, def)
	if res.TrackEvents && res.Experiment != nil {
		c.queueExposure(state.snapshot, evalCtx, res)
	}
	return v, res
}

// ---------------------------------------------------------------------------
// Exposure pipeline
// ---------------------------------------------------------------------------

type collectEvent struct {
	EventID      string    `json:"eventId"`
	Type         string    `json:"type"`
	ExperimentID string    `json:"experimentId"`
	Epoch        int64     `json:"epoch"`
	FlagKey      string    `json:"flagKey"`
	FlagRevision int64     `json:"flagRevision"`
	Variant      string    `json:"variant"`
	Unit         string    `json:"unit"`
	Source       string    `json:"source"`
	MetricKey    string    `json:"metricKey,omitempty"`
	Value        float64   `json:"value,omitempty"`
	SessionID    string    `json:"sessionId,omitempty"`
	TaskID       string    `json:"taskId,omitempty"`
	ClientTime   time.Time `json:"clientTime"`
}

func randomEventID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is catastrophic; fall back to a monotonic
		// pseudo-ID — dedup by (unit, variant, epoch) on the server still
		// protects analysis integrity.
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}

// queueExposure extracts the randomization unit from the evaluation context
// (via the flag's bucketing config) and enqueues an exposure. Overflow drops
// the NEWEST event and counts it — evaluation is never blocked.
func (c *Client) queueExposure(snapshot *model.Snapshot, evalCtx model.EvaluationContext, res model.Result) {
	if c.opts.CollectToken == "" {
		c.droppedExposures.Add(1)
		return
	}
	unit := ""
	if flag, ok := snapshot.Flags[res.FlagKey]; ok && flag.Bucketing != nil {
		if v, found := evalCtx.Field(flag.Bucketing.Unit); found {
			// Canonical form matches what the evaluator hashed, so the
			// server-side HMAC lands on the same unit identity.
			unit, _ = evaluation.CanonicalUnitString(v)
		}
	}
	if unit == "" {
		// No unit → the evaluator would not have produced an exposure;
		// defensive drop.
		c.droppedExposures.Add(1)
		return
	}
	rev := int64(0)
	if f, ok := snapshot.Flags[res.FlagKey]; ok {
		_, _ = fmt.Sscanf(f.Version, "%d", &rev)
	}
	ev := &collectEvent{
		EventID: randomEventID("exp"), Type: "exposure",
		ExperimentID: res.Experiment.ExperimentID, Epoch: res.Experiment.Epoch,
		FlagKey: res.FlagKey, FlagRevision: rev,
		Variant: res.Experiment.Variant, Unit: unit, Source: res.Experiment.Source,
		ClientTime: time.Now().UTC(),
	}
	select {
	case c.exposures <- ev:
	default:
		c.droppedExposures.Add(1)
	}
}

// TrackConversion records a conversion event attributed to the experiment
// of a prior evaluation result. The unit comes from the SAME evaluation
// context, keeping attribution consistent with the exposure.
func TrackConversion(c *Client, evalCtx model.EvaluationContext, res model.Result,
	metricKey string, value float64, sessionID, taskID string) error {

	if res.Experiment == nil {
		return fmt.Errorf("sdk: result carries no experiment attribution (reason %s)", res.Reason)
	}
	if c.opts.CollectToken == "" {
		return errors.New("sdk: collect token not configured")
	}
	c.mu.RLock()
	state := c.state
	c.mu.RUnlock()
	unit := ""
	if state != nil {
		if flag, ok := state.snapshot.Flags[res.FlagKey]; ok && flag.Bucketing != nil {
			if v, found := evalCtx.Field(flag.Bucketing.Unit); found {
				unit, _ = evaluation.CanonicalUnitString(v)
			}
		}
	}
	if unit == "" {
		return errors.New("sdk: cannot derive randomization unit from context")
	}
	rev := int64(0)
	if state != nil {
		if f, ok := state.snapshot.Flags[res.FlagKey]; ok {
			_, _ = fmt.Sscanf(f.Version, "%d", &rev)
		}
	}
	ev := &collectEvent{
		EventID: randomEventID("conv"), Type: "conversion",
		ExperimentID: res.Experiment.ExperimentID, Epoch: res.Experiment.Epoch,
		FlagKey: res.FlagKey, FlagRevision: rev,
		Variant: res.Experiment.Variant, Unit: unit, Source: res.Experiment.Source,
		MetricKey: metricKey, Value: value, SessionID: sessionID, TaskID: taskID,
		ClientTime: time.Now().UTC(),
	}
	select {
	case c.exposures <- ev:
		return nil
	default:
		c.droppedExposures.Add(1)
		return errors.New("sdk: exposure queue full, conversion dropped")
	}
}

func (c *Client) exposureLoop(ctx context.Context) {
	t := time.NewTicker(c.opts.ExposureFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			c.flushExposures(ctx)
		}
	}
}

// flushExposures drains up to one batch and posts it. One retry after a
// short pause; persistent failures drop the batch (counted) — events must
// never accumulate without bound.
func (c *Client) flushExposures(ctx context.Context) {
	if c.opts.CollectToken == "" {
		return
	}
	batch := make([]*collectEvent, 0, c.opts.ExposureBatchSize)
	for len(batch) < c.opts.ExposureBatchSize {
		select {
		case ev := <-c.exposures:
			batch = append(batch, ev)
		default:
			goto send
		}
	}
send:
	if len(batch) == 0 {
		return
	}
	if err := c.postEvents(ctx, batch); err != nil {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			c.droppedExposures.Add(int64(len(batch)))
			return
		case <-c.closed:
			c.droppedExposures.Add(int64(len(batch)))
			return
		}
		if err := c.postEvents(ctx, batch); err != nil {
			c.opts.Logger.Printf("sdk: exposure flush failed twice, dropping %d events: %v", len(batch), err)
			c.droppedExposures.Add(int64(len(batch)))
			return
		}
	}
	c.flushedExposures.Add(int64(len(batch)))
}

func (c *Client) postEvents(ctx context.Context, batch []*collectEvent) error {
	body, err := json.Marshal(map[string]any{"events": batch})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.opts.BaseURL, "/")+"/api/v1/collect", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.opts.CollectToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("collect HTTP %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
