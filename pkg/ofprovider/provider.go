// Package ofprovider implements the OpenFeature Go SDK provider contract
// (github.com/open-feature/go-sdk) on top of the GoLab Go SDK
// (docs/技术方案.md §8: OpenFeature Provider 实现布尔、字符串、数值、对象求值，
// 映射 targetingKey 到稳定单元并返回 variant、reason、error code).
//
// Semantics mapping:
//
//   - Evaluation runs LOCALLY on the golab SDK's synchronized, validated
//     snapshot — no per-request network I/O (hot path, §3).
//   - The OpenFeature targetingKey becomes the golab randomization unit: it
//     is mirrored into the configured UnitField (default "user_id") whenever
//     that attribute is absent, and always available as golab's implicit
//     "targetingKey" field. Explicit attributes win over the mirror.
//   - Reasons map 1:1 where OpenFeature defines them (DEFAULT,
//     TARGETING_MATCH, SPLIT, DISABLED, ERROR); golab-specific reasons
//     (NOT_ENROLLED, EXPERIMENT_NOT_RUNNING, STALE) degrade to the closest
//     OpenFeature reason and stay verbatim in FlagMetadata["golabReason"] —
//     OpenFeature resolution details DO carry reasons (§8).
//   - MISSING_BUCKET_KEY maps to the spec's TARGETING_KEY_MISSING error with
//     the caller's default value; FLAG_NOT_FOUND and TYPE_MISMATCH map to
//     their OpenFeature equivalents.
//   - Exposures follow "真正使用/进入实验才曝光": the underlying golab SDK
//     enqueues an exposure only when the evaluation actually enrolled the
//     unit in a running experiment, and flushes batches to /api/v1/collect
//     with the separate collect credential (§8, §10.1).
//   - Provider lifecycle: Init starts the golab client and emits
//     PROVIDER_READY once the first validated snapshot is synchronized; the
//     monitor degrades to PROVIDER_STALE when the snapshot exceeds the
//     client's MaxStaleness (kill-switch window, §5.2) and recovers
//     automatically. Shutdown flushes pending exposures and closes the
//     client.
//
// Usage:
//
//	provider := ofprovider.NewProvider(ofprovider.Options{
//	    ClientOptions: sdk.Options{
//	        BaseURL:      "http://127.0.0.1:8688",
//	        Token:        sdkToken,
//	        CollectToken: collectToken,
//	    },
//	})
//	if err := openfeature.SetProviderAndWait(provider); err != nil { ... }
//	client := openfeature.NewClient("my-app")
//	details, _ := client.BooleanValueDetails(ctx, "checkout-router", false,
//	    openfeature.NewEvaluationContext("user-42", map[string]any{"tenant": "acme"}))
package ofprovider

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/godeps/grayling/pkg/model"
	"github.com/godeps/grayling/pkg/sdk"
	of "github.com/open-feature/go-sdk/openfeature"
)

// Name is the provider metadata name reported to OpenFeature.
const Name = "golab"

// DefaultUnitField is the golab context field that receives the OpenFeature
// targetingKey when the caller did not set that attribute explicitly. Flags
// bucketing on "targetingKey" itself need no mirroring.
const DefaultUnitField = "user_id"

// Options configures a Provider.
type Options struct {
	// Client is an existing, NOT YET STARTED golab SDK client (the provider
	// owns its lifecycle: Init starts it, Shutdown closes it). When nil, a
	// client is created from ClientOptions during Init.
	Client *sdk.Client
	// ClientOptions configures the golab SDK client when Client is nil.
	// BaseURL and Token are required in that case.
	ClientOptions sdk.Options
	// UnitField is the golab randomization-unit attribute fed from the
	// OpenFeature targetingKey (default "user_id").
	UnitField string
	// MonitorInterval is the state-monitor tick (default 500ms).
	MonitorInterval time.Duration
}

// Provider is an OpenFeature FeatureProvider backed by golab local
// evaluation. It also implements the StateHandler (Init/Shutdown) and
// EventHandler contracts.
type Provider struct {
	opts   Options
	client *sdk.Client

	events chan of.Event

	mu           sync.Mutex
	lastSent     of.State
	lastRevision int64
	seenRevision bool
	stop         chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

// Compile-time contract checks against the OpenFeature SDK interfaces.
var (
	_ of.FeatureProvider = (*Provider)(nil)
	_ of.StateHandler    = (*Provider)(nil)
	_ of.EventHandler    = (*Provider)(nil)
	_ of.Tracker         = (*Provider)(nil)
)

// NewProvider builds the provider. Call OpenFeature's SetProvider/SetProviderAndWait
// to register it; Init performs the golab client startup.
func NewProvider(opts Options) *Provider {
	if opts.UnitField == "" {
		opts.UnitField = DefaultUnitField
	}
	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = 500 * time.Millisecond
	}
	return &Provider{
		opts:     opts,
		events:   make(chan of.Event, 8),
		lastSent: of.NotReadyState,
	}
}

// Metadata implements of.FeatureProvider.
func (p *Provider) Metadata() of.Metadata { return of.Metadata{Name: Name} }

// Hooks implements of.FeatureProvider. Golab exposure capture happens inside
// evaluation (only on real enrollment), so no OF hooks are attached.
func (p *Provider) Hooks() []of.Hook { return nil }

// Track maps an OpenFeature tracking event to the metric of a prior decision.
// Callers pass the opaque golabDecisionId from evaluation metadata as the
// tracking event detail "decisionId". The SDK resolves the unit privately.
func (p *Provider) Track(_ context.Context, trackingEventName string,
	_ of.EvaluationContext, details of.TrackingEventDetails) {
	if p.client == nil || trackingEventName == "" {
		return
	}
	id, ok := details.Attribute("decisionId").(string)
	if !ok || id == "" {
		return
	}
	_ = p.client.TrackConversionByDecisionID(id, trackingEventName, details.Value())
}

// Init implements of.StateHandler: it starts (or adopts and starts) the
// golab SDK client and launches the provider-state monitor. A failed initial
// snapshot sync is NOT fatal — the provider stays NOT_READY, evaluations
// answer with caller defaults + PROVIDER_NOT_READY, and the client keeps
// retrying in the background (§3 cold start).
func (p *Provider) Init(_ of.EvaluationContext) error {
	p.mu.Lock()
	if p.stop == nil {
		p.stop = make(chan struct{})
	}
	p.mu.Unlock()

	client := p.opts.Client
	if client == nil {
		if p.opts.ClientOptions.BaseURL == "" || p.opts.ClientOptions.Token == "" {
			return errors.New("golab provider: ClientOptions.BaseURL and ClientOptions.Token are required when no Client is supplied")
		}
		c, err := sdk.New(context.Background(), p.opts.ClientOptions)
		if c == nil {
			return err
		}
		// sdk.New returns a usable client even when the first sync failed;
		// the monitor reports readiness once a snapshot lands.
		client = c
	}
	p.client = client
	p.client.Start(context.Background())

	p.wg.Go(p.monitor)
	return nil
}

// Shutdown implements of.StateHandler: it stops the monitor and closes the
// golab client (pending exposures are flushed once by Close). Safe to call
// even when Init never ran.
func (p *Provider) Shutdown() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		stop := p.stop
		p.mu.Unlock()
		if stop != nil {
			close(stop)
		}
	})
	p.wg.Wait()
	if p.client != nil {
		p.client.Close()
	}
}

// EventChannel implements of.EventHandler.
func (p *Provider) EventChannel() <-chan of.Event { return p.events }

// SampledEvents exposes opt-in OpenTelemetry-shaped evaluation events after
// Init. Collection exposures and conversions are never sampled.
func (p *Provider) SampledEvents() <-chan model.EvaluationEvent {
	if p.client == nil {
		return nil
	}
	return p.client.SampledEvents()
}

// monitor translates golab SDK sync health into OpenFeature provider state
// transitions: NOT_READY → READY on first snapshot, READY ↔ STALE across the
// MaxStaleness kill-switch window.
func (p *Provider) monitor() {
	t := time.NewTicker(p.opts.MonitorInterval)
	defer t.Stop()
	for {
		if p.client != nil {
			st := p.client.Status()
			switch {
			case st.LastError != "" && !st.Stale:
				p.transition(of.ErrorState, "golab upstream synchronization failed")
			case !st.Ready:
				// no validated snapshot yet
			case st.Stale:
				p.transition(of.StaleState, "golab snapshot exceeded max staleness")
			default:
				p.transition(of.ReadyState, "golab snapshot synchronized")
			}
			if st.Ready {
				p.configurationChanged(st.ManifestRevision)
			}
		}
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
	}
}

func (p *Provider) transition(to of.State, message string) {
	p.mu.Lock()
	if p.lastSent == to {
		p.mu.Unlock()
		return
	}
	p.lastSent = to
	p.mu.Unlock()

	var eventType of.EventType
	switch to {
	case of.ReadyState:
		eventType = of.ProviderReady
	case of.StaleState:
		eventType = of.ProviderStale
	case of.ErrorState:
		eventType = of.ProviderError
	default:
		return
	}
	details := of.ProviderEventDetails{Message: message}
	if p.client != nil {
		details.EventMetadata = map[string]any{
			"manifestRevision": p.client.Status().ManifestRevision,
		}
	}
	if to == of.ErrorState {
		details.ErrorCode = of.GeneralCode
	}
	select {
	case p.events <- of.Event{ProviderName: Name, EventType: eventType,
		ProviderEventDetails: details}:
	default: // bounded channel; slow consumers receive the newest future state
	}
}

func (p *Provider) configurationChanged(revision int64) {
	p.mu.Lock()
	previous := p.lastRevision
	first := !p.seenRevision
	p.seenRevision = true
	if first || revision == previous {
		p.lastRevision = revision
		p.mu.Unlock()
		return
	}
	p.lastRevision = revision
	p.mu.Unlock()
	select {
	case p.events <- of.Event{ProviderName: Name, EventType: of.ProviderConfigChange,
		ProviderEventDetails: of.ProviderEventDetails{
			Message:       "golab configuration changed",
			EventMetadata: map[string]any{"manifestRevision": revision},
		}}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

// BooleanEvaluation implements of.FeatureProvider.
func (p *Provider) BooleanEvaluation(_ context.Context, flag string, defaultValue bool,
	flatCtx of.FlattenedContext) of.BoolResolutionDetail {

	if p.client == nil {
		return of.BoolResolutionDetail{Value: defaultValue,
			ProviderResolutionDetail: notReady()}
	}
	v, res := p.client.BoolVariation(flag, toGolabContext(flatCtx, p.opts.UnitField), defaultValue)
	return of.BoolResolutionDetail{Value: v, ProviderResolutionDetail: convert(res)}
}

// StringEvaluation implements of.FeatureProvider.
func (p *Provider) StringEvaluation(_ context.Context, flag string, defaultValue string,
	flatCtx of.FlattenedContext) of.StringResolutionDetail {

	if p.client == nil {
		return of.StringResolutionDetail{Value: defaultValue,
			ProviderResolutionDetail: notReady()}
	}
	v, res := p.client.StringVariation(flag, toGolabContext(flatCtx, p.opts.UnitField), defaultValue)
	return of.StringResolutionDetail{Value: v, ProviderResolutionDetail: convert(res)}
}

// FloatEvaluation implements of.FeatureProvider: golab number flags map
// directly to float64.
func (p *Provider) FloatEvaluation(_ context.Context, flag string, defaultValue float64,
	flatCtx of.FlattenedContext) of.FloatResolutionDetail {

	if p.client == nil {
		return of.FloatResolutionDetail{Value: defaultValue,
			ProviderResolutionDetail: notReady()}
	}
	v, res := p.client.NumberVariation(flag, toGolabContext(flatCtx, p.opts.UnitField), defaultValue)
	return of.FloatResolutionDetail{Value: v, ProviderResolutionDetail: convert(res)}
}

// IntEvaluation implements of.FeatureProvider: golab stores all numbers as
// float64; an integral value converts losslessly, anything else is a
// TYPE_MISMATCH and the caller default is served (§5.2: 错误时返回显式
// default、reason/error，不静默随机分组).
func (p *Provider) IntEvaluation(_ context.Context, flag string, defaultValue int64,
	flatCtx of.FlattenedContext) of.IntResolutionDetail {

	if p.client == nil {
		return of.IntResolutionDetail{Value: defaultValue,
			ProviderResolutionDetail: notReady()}
	}
	v, res := p.client.NumberVariation(flag, toGolabContext(flatCtx, p.opts.UnitField), float64(defaultValue))
	detail := convert(res)
	if !res.Failed {
		if v != math.Trunc(v) || v > math.MaxInt64 || v < math.MinInt64 {
			detail.ResolutionError = of.NewTypeMismatchResolutionError(
				"golab number value is not an integer")
			detail.Reason = of.ErrorReason
			return of.IntResolutionDetail{Value: defaultValue, ProviderResolutionDetail: detail}
		}
	}
	return of.IntResolutionDetail{Value: int64(v), ProviderResolutionDetail: detail}
}

// ObjectEvaluation implements of.FeatureProvider: golab json flags map to
// OpenFeature objects.
func (p *Provider) ObjectEvaluation(_ context.Context, flag string, defaultValue any,
	flatCtx of.FlattenedContext) of.InterfaceResolutionDetail {

	if p.client == nil {
		return of.InterfaceResolutionDetail{Value: defaultValue,
			ProviderResolutionDetail: notReady()}
	}
	v, res := p.client.JSONVariation(flag, toGolabContext(flatCtx, p.opts.UnitField), defaultValue)
	if !res.Failed {
		switch res.Value.(type) {
		case map[string]any, []any:
		default:
			return of.InterfaceResolutionDetail{Value: defaultValue,
				ProviderResolutionDetail: of.ProviderResolutionDetail{
					Reason:          of.ErrorReason,
					ResolutionError: of.NewTypeMismatchResolutionError("golab value is not an object or array"),
				}}
		}
	}
	return of.InterfaceResolutionDetail{Value: v, ProviderResolutionDetail: convert(res)}
}

// ---------------------------------------------------------------------------
// Mapping helpers
// ---------------------------------------------------------------------------

// toGolabContext maps the OpenFeature flattened context onto golab's
// evaluation context: targetingKey becomes the stable randomization unit and
// is mirrored into unitField when the caller did not provide that attribute
// explicitly (targetingKey IS the subject identity — OF spec §3.2.2).
func toGolabContext(flat of.FlattenedContext, unitField string) model.EvaluationContext {
	ctx := model.EvaluationContext{Attributes: map[string]any{}}
	if flat == nil {
		return ctx
	}
	for k, v := range flat {
		if k == string(of.TargetingKey) {
			if s, ok := v.(string); ok {
				ctx.TargetingKey = s
			}
			continue
		}
		ctx.Attributes[k] = v
	}
	if ctx.TargetingKey != "" {
		if _, exists := ctx.Attributes[unitField]; !exists && unitField != model.TargetingKeyField {
			ctx.Attributes[unitField] = ctx.TargetingKey
		}
	}
	return ctx
}

// convert translates one golab Result into OpenFeature resolution details.
func convert(res model.Result) of.ProviderResolutionDetail {
	detail := of.ProviderResolutionDetail{
		Variant:      res.VariationType,
		Reason:       mapReason(res),
		FlagMetadata: metadata(res),
	}
	if !res.Failed {
		return detail
	}
	detail.Reason = of.ErrorReason
	switch res.ErrorCode {
	case model.ErrFlagNotFound:
		detail.ResolutionError = of.NewFlagNotFoundResolutionError(orDefault(res.ErrorDetails, "flag not found"))
	case model.ErrMissingBucketKey:
		detail.ResolutionError = of.NewTargetingKeyMissingResolutionError(
			orDefault(res.ErrorDetails, "randomization unit missing from evaluation context"))
	case model.ErrTypeMismatch:
		detail.ResolutionError = of.NewTypeMismatchResolutionError(orDefault(res.ErrorDetails, "flag value type mismatch"))
	case model.ErrParse:
		detail.ResolutionError = of.NewParseErrorResolutionError(orDefault(res.ErrorDetails, "flag parse error"))
	default:
		if strings.Contains(res.ErrorDetails, "no snapshot") {
			detail.ResolutionError = of.NewProviderNotReadyResolutionError(res.ErrorDetails)
			break
		}
		detail.ResolutionError = of.NewGeneralResolutionError(orDefault(res.ErrorDetails, "evaluation failed"))
	}
	return detail
}

func mapReason(res model.Result) of.Reason {
	if res.Failed {
		return of.ErrorReason
	}
	switch res.Reason {
	case model.ReasonTargetingMatch:
		return of.TargetingMatchReason
	case model.ReasonSplit:
		return of.SplitReason
	case model.ReasonDisabled:
		return of.DisabledReason
	case model.ReasonDefault, model.ReasonNotEnrolled, model.ReasonExperimentNotRunning:
		// Golab experiment gating states resolve to the deterministic
		// default rule — DEFAULT is the honest OpenFeature reason; the
		// verbatim golab reason stays in FlagMetadata.
		return of.DefaultReason
	case model.ReasonStale:
		return of.ErrorReason
	default:
		return of.UnknownReason
	}
}

// metadata exposes golab-native resolution facts that OpenFeature has no
// first-class fields for (values restricted to OF metadata types).
func metadata(res model.Result) of.FlagMetadata {
	md := of.FlagMetadata{}
	if res.Reason != "" {
		md["golabReason"] = string(res.Reason)
	}
	if res.FlagVersion != "" {
		md["golabFlagVersion"] = res.FlagVersion
	}
	if res.Experiment != nil {
		md["golabExperimentId"] = res.Experiment.ExperimentID
		md["golabExperimentEpoch"] = res.Experiment.Epoch
		md["golabExposureSource"] = res.Experiment.Source
	}
	if res.Bucket >= 0 {
		md["golabBucket"] = int64(res.Bucket)
	}
	if res.DecisionID != "" {
		md["golabDecisionId"] = res.DecisionID
		md["golabManifestRevision"] = res.ManifestRevision
		md["golabSnapshotAgeMillis"] = res.SnapshotAgeMillis
		md["golabRule"] = res.Rule
		if res.ExposureEventID != "" {
			md["golabExposureEventId"] = res.ExposureEventID
		}
	}
	return md
}

func notReady() of.ProviderResolutionDetail {
	return of.ProviderResolutionDetail{
		Reason:          of.ErrorReason,
		ResolutionError: of.NewProviderNotReadyResolutionError("golab provider not initialized"),
	}
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
