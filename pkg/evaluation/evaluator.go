// Package evaluation implements the GoLab native evaluator over a validated
// model.Snapshot (docs/技术方案.md §5.2, §6).
//
// Normative evaluation order for one flag:
//
//  1. FLAG_NOT_FOUND if the snapshot does not contain the key.
//  2. Disable (kill switch): serve the default rule with reason DISABLED,
//     no exposure.
//  3. Experiment gating (when bound): non-running state → default variation,
//     reason EXPERIMENT_NOT_RUNNING; mutual-exclusion layer slot and
//     eligibility are checked on their own hash purposes — a miss serves the
//     default variation with reason NOT_ENROLLED. Neither emits exposures.
//  4. Targeting rules in list order; disabled rules are skipped. The first
//     rule whose constraints AND segments match decides: a fixed variation
//     (reason TARGETING_MATCH) or a percentage split (reason SPLIT).
//  5. Otherwise the default rule decides (reason DEFAULT, or SPLIT when it
//     carries a percentage).
//
// Percentage resolution requires the bucketing unit from the context. A
// missing unit NEVER generates a random assignment: the evaluator returns
// the flag's explicit default variation (or the caller default in typed APIs)
// with error code MISSING_BUCKET_KEY and Failed=true, and records no
// exposure.
//
// Results are deterministic: identical (snapshot revision, flag, context)
// inputs always produce identical outputs. Errors surface as explicit
// defaults with reason ERROR — never as silent random grouping.
//
// Exposure semantics ("真正使用/进入实验才曝光", §8): Result.Experiment is
// set only when the flag binds a RUNNING experiment, the unit passed
// eligibility and layer gating, and a variation was actually assigned. The
// Source field distinguishes randomized ("split") from deterministic
// ("targeting"/"default") arms so SRM analysis can use the randomized
// population only.
package evaluation

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/godeps/grayling/internal/bandit"
	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/model"
)

// Evaluator evaluates flags from one immutable, pre-validated snapshot.
// It is safe for concurrent use.
type Evaluator struct {
	snap          *model.Snapshot
	flags         map[string]*compiledFlag
	segs          map[string]*compiledSegment
	banditEnabled bool
}

// EnableBandit is used by the remote server, which owns the exposure ledger
// and can pin repeat evaluations. Local SDKs retain the fixed split fallback.
func (e *Evaluator) EnableBandit() { e.banditEnabled = true }

type compiledFlag struct {
	flag        *model.Flag
	rules       []compiledRule
	defaultRule compiledRule
	exp         *compiledExperiment
}

type compiledExperiment struct {
	id          string
	epoch       int64
	layer       string
	layerSlot   *model.BucketRange
	eligBuckets int // 0..10000; 10000 = everyone enrolled
	running     bool
	bandit      *model.BanditBinding
	banditProb  []float64
}

// needsGatingBucket reports whether layer or eligibility checks will hash.
func (exp *compiledExperiment) needsGatingBucket() bool {
	return (exp.layer != "" && exp.layerSlot != nil) || exp.eligBuckets < bucketing.Buckets
}

type compiledRule struct {
	rule        *model.Rule
	split       *bucketing.Split
	constraints []compiledConstraint
	segments    []*compiledSegment
}

// ErrMissingBucketKey is returned internally when a split or gating step
// needs the randomization unit but the context does not carry one.
var ErrMissingBucketKey = errors.New("evaluation: bucketing unit missing from context")

// NewEvaluator validates the whole snapshot (every flag against the segment
// set) and precompiles splits, regexes, CIDRs, semvers and time operands.
// On any validation error it returns an error and no evaluator: callers
// (SDKs) must keep their previous snapshot instead of serving a partially
// valid one (§5.2 atomic replacement).
func NewEvaluator(snap *model.Snapshot) (*Evaluator, error) {
	if snap == nil {
		return nil, fmt.Errorf("evaluation: nil snapshot")
	}
	if snap.SchemaVersion != model.SchemaVersion {
		return nil, fmt.Errorf("evaluation: snapshot schemaVersion %d, want %d",
			snap.SchemaVersion, model.SchemaVersion)
	}
	if snap.Workspace == "" || snap.Environment == "" {
		return nil, fmt.Errorf("evaluation: snapshot missing workspace/environment")
	}
	for _, seg := range snap.Segments {
		if err := model.ValidateSegment(seg, snap.Segments); err != nil {
			return nil, fmt.Errorf("evaluation: segment %q: %w", seg.Key, err)
		}
	}

	e := &Evaluator{
		snap:  snap,
		flags: make(map[string]*compiledFlag, len(snap.Flags)),
		segs:  make(map[string]*compiledSegment, len(snap.Segments)),
	}
	// Compile segments first (flags reference them).
	for key := range snap.Segments {
		if _, err := e.compileSegment(key); err != nil {
			return nil, err
		}
	}
	for key, f := range snap.Flags {
		if err := model.ValidateFlag(f, snap.Segments); err != nil {
			return nil, fmt.Errorf("evaluation: flag %q: %w", key, err)
		}
		cf, err := e.compileFlag(f)
		if err != nil {
			return nil, fmt.Errorf("evaluation: flag %q: %w", key, err)
		}
		e.flags[key] = cf
	}
	return e, nil
}

// Snapshot exposes the underlying immutable snapshot (read-only).
func (e *Evaluator) Snapshot() *model.Snapshot { return e.snap }

func (e *Evaluator) compileFlag(f *model.Flag) (*compiledFlag, error) {
	cf := &compiledFlag{flag: f}
	var err error
	cf.defaultRule, err = e.compileRule(f.DefaultRule)
	if err != nil {
		return nil, fmt.Errorf("defaultRule: %w", err)
	}
	cf.rules = make([]compiledRule, 0, len(f.Targeting))
	for i := range f.Targeting {
		cr, err := e.compileRule(&f.Targeting[i])
		if err != nil {
			return nil, fmt.Errorf("targeting[%d]: %w", i, err)
		}
		cf.rules = append(cf.rules, cr)
	}
	if f.Experiment != nil {
		elig := f.Experiment.Eligibility
		if elig <= 0 {
			elig = 100
		}
		// Eligibility percent → buckets: nearest integer (half up),
		// clamped to [0,10000], so every language agrees on e.g.
		// 12.5% → 1250 buckets.
		eligBuckets := int(math.Floor(elig*100 + 0.5))
		eligBuckets = max(0, min(bucketing.Buckets, eligBuckets))
		cf.exp = &compiledExperiment{
			id:          f.Experiment.ID,
			epoch:       f.Experiment.Epoch,
			layer:       f.Experiment.Layer,
			layerSlot:   f.Experiment.LayerSlot,
			eligBuckets: eligBuckets,
			running:     f.Experiment.State == model.ExperimentRunning,
		}
		if b := f.Experiment.Bandit; b != nil {
			cf.exp.bandit = b
			if !b.Frozen && b.StateVersion > 0 {
				probs, err := bandit.Distribution(b)
				if err != nil {
					return nil, err
				}
				cf.exp.banditProb = probs
			}
		}
	}
	return cf, nil
}

func (e *Evaluator) compileRule(r *model.Rule) (compiledRule, error) {
	cr := compiledRule{rule: r}
	if len(r.Percentage) > 0 {
		split, err := bucketing.NewSplit(r.Percentage)
		if err != nil {
			return cr, err
		}
		cr.split = split
	}
	cr.constraints = make([]compiledConstraint, 0, len(r.Constraints))
	for i := range r.Constraints {
		cc, err := compileConstraint(&r.Constraints[i])
		if err != nil {
			return cr, fmt.Errorf("constraints[%d]: %w", i, err)
		}
		cr.constraints = append(cr.constraints, cc)
	}
	for _, ref := range r.Segments {
		cs, ok := e.segs[ref]
		if !ok {
			return cr, fmt.Errorf("segment %q not in snapshot", ref)
		}
		cr.segments = append(cr.segments, cs)
	}
	return cr, nil
}

type compiledSegment struct {
	key         string
	constraints []compiledConstraint
	nested      []*compiledSegment
}

func (e *Evaluator) compileSegment(key string) (*compiledSegment, error) {
	return e.compileSegmentDepth(key, map[string]bool{}, 0)
}

func (e *Evaluator) compileSegmentDepth(key string, visiting map[string]bool, depth int) (*compiledSegment, error) {
	if cs, ok := e.segs[key]; ok {
		return cs, nil
	}
	if depth > model.MaxSegmentDepth {
		return nil, fmt.Errorf("segment %q: nesting exceeds depth %d", key, model.MaxSegmentDepth)
	}
	if visiting[key] {
		return nil, fmt.Errorf("segment %q: cycle detected", key)
	}
	seg, ok := e.snap.Segments[key]
	if !ok {
		return nil, fmt.Errorf("segment %q not in snapshot", key)
	}
	visiting[key] = true
	defer delete(visiting, key)

	cs := &compiledSegment{key: key}
	for i := range seg.Constraints {
		cc, err := compileConstraint(&seg.Constraints[i])
		if err != nil {
			return nil, fmt.Errorf("segment %q constraints[%d]: %w", key, i, err)
		}
		cs.constraints = append(cs.constraints, cc)
	}
	for _, ref := range seg.Segments {
		nested, err := e.compileSegmentDepth(ref, visiting, depth+1)
		if err != nil {
			return nil, err
		}
		cs.nested = append(cs.nested, nested)
	}
	e.segs[key] = cs
	return cs, nil
}

// Evaluate resolves one flag and returns the raw result. Value is the
// variation's DSL value (any); use the typed Variation methods for
// caller-default semantics.
func (e *Evaluator) Evaluate(flagKey string, ctx model.EvaluationContext) model.Result {
	cf, ok := e.flags[flagKey]
	if !ok {
		return model.Result{
			FlagKey: flagKey, Reason: model.ReasonError, Failed: true,
			ErrorCode: model.ErrFlagNotFound, ErrorDetails: "flag not in snapshot",
			Bucket: -1,
		}
	}
	res := e.evaluateCompiled(cf, ctx)
	res.FlagKey = flagKey
	res.FlagVersion = cf.flag.Version
	return res
}

func (e *Evaluator) evaluateCompiled(cf *compiledFlag, ctx model.EvaluationContext) model.Result {
	f := cf.flag
	base := model.Result{Bucket: -1, Rule: "default"}

	// Kill switch first (§5.2: 优先判断禁用/紧急关闭).
	if f.Disable {
		name, value, bucket, err := e.resolveVariation(cf, &cf.defaultRule, ctx)
		if err != nil {
			if errors.Is(err, ErrMissingBucketKey) {
				return missingBucketKey(base, f)
			}
			return errorResult(base, model.ErrGeneral, err.Error())
		}
		base.VariationType, base.Value, base.Reason = name, value, model.ReasonDisabled
		base.Bucket = bucket
		base.Rule = "default"
		if err := model.CheckVariationType(f.Type, value); err != nil {
			return typeMismatch(base, err)
		}
		return base
	}

	// Experiment gating.
	enrolled := false
	if cf.exp != nil {
		exp := cf.exp
		if !exp.running {
			// Non-running experiments serve the deterministic default
			// variation (validation guarantees defaultRule.variation),
			// with no exposure.
			base.VariationType = f.DefaultRule.Variation
			base.Value = f.Variations[base.VariationType]
			base.Reason = model.ReasonExperimentNotRunning
			return base
		}
		if exp.needsGatingBucket() {
			unit, hasUnit := e.unitValue(cf, ctx)
			if !hasUnit {
				return missingBucketKey(base, f)
			}
			if exp.layer != "" && exp.layerSlot != nil {
				lb, err := e.hashBucket(unit, exp.layer, 0, exp.layer, bucketing.PurposeLayer)
				if err != nil {
					return errorResult(base, model.ErrGeneral, err.Error())
				}
				if lb < exp.layerSlot.Start || lb >= exp.layerSlot.End {
					return notEnrolled(base, f)
				}
			}
			if exp.eligBuckets < bucketing.Buckets {
				eb, err := e.hashBucket(unit, exp.id, exp.epoch, f.Bucketing.Salt, bucketing.PurposeEligibility)
				if err != nil {
					return errorResult(base, model.ErrGeneral, err.Error())
				}
				if eb >= exp.eligBuckets {
					return notEnrolled(base, f)
				}
			}
		}
		enrolled = true
	}

	// Targeting rules in order; first match wins.
	for i := range cf.rules {
		r := &cf.rules[i]
		if r.rule.Disable {
			continue
		}
		if !e.matchRule(r, ctx) {
			continue
		}
		base.Rule = r.rule.Name
		if base.Rule == "" {
			base.Rule = fmt.Sprintf("targeting[%d]", i)
		}
		return e.finish(cf, r, ctx, model.ReasonTargetingMatch, "targeting", enrolled, base)
	}

	// Default rule.
	base.Rule = "default"
	return e.finish(cf, &cf.defaultRule, ctx, model.ReasonDefault, "default", enrolled, base)
}

func (e *Evaluator) finish(cf *compiledFlag, rule *compiledRule, ctx model.EvaluationContext,
	staticReason model.Reason, source string, enrolled bool, base model.Result) model.Result {

	name, value, bucket, err := e.resolveVariation(cf, rule, ctx)
	if err != nil {
		if errors.Is(err, ErrMissingBucketKey) {
			return missingBucketKey(base, cf.flag)
		}
		return errorResult(base, model.ErrGeneral, err.Error())
	}
	reason := staticReason
	if rule.split != nil {
		reason = model.ReasonSplit
		source = "split"
		if cf.exp != nil && e.banditEnabled && cf.exp.banditActive(e.snap.GeneratedAt) {
			source = "bandit"
		}
	}
	base.VariationType, base.Value, base.Reason = name, value, reason
	base.Bucket = bucket
	if err := model.CheckVariationType(cf.flag.Type, value); err != nil {
		return typeMismatch(base, err)
	}
	if enrolled && cf.exp != nil {
		base.TrackEvents = true
		base.Experiment = &model.Exposure{
			ExperimentID: cf.exp.id, Epoch: cf.exp.epoch,
			Variant: name, Source: source,
		}
		if cf.exp.bandit != nil {
			base.Experiment.PolicyVersion = cf.exp.bandit.PolicyVersion
			base.Experiment.StateVersion = cf.exp.bandit.StateVersion
			base.Experiment.AllocationProbability = 1
			if rule.split != nil {
				base.Experiment.AllocationProbability = rule.rule.Percentage[name] / 100
			}
		}
		if source == "bandit" {
			for i, v := range cf.exp.bandit.Variants {
				if v == name {
					base.Experiment.AllocationProbability = cf.exp.banditProb[i]
					break
				}
			}
		}
	}
	return base
}

// resolveVariation turns a matched rule into (variation name, value, bucket).
// bucket is -1 when no split was involved.
func (e *Evaluator) resolveVariation(cf *compiledFlag, rule *compiledRule,
	ctx model.EvaluationContext) (string, any, int, error) {

	if rule.split == nil {
		name := rule.rule.Variation
		return name, cf.flag.Variations[name], -1, nil
	}
	unit, ok := e.unitValue(cf, ctx)
	if !ok {
		return "", nil, -1, ErrMissingBucketKey
	}
	key, epoch := e.hashCoordinate(cf)
	bucket, err := e.hashBucket(unit, key, epoch, cf.flag.Bucketing.Salt, bucketing.PurposeVariant)
	if err != nil {
		return "", nil, -1, err
	}
	if cf.exp != nil && e.banditEnabled && cf.exp.banditActive(e.snap.GeneratedAt) {
		arm, _ := bandit.Assign(cf.exp.bandit, unit, cf.exp.banditProb)
		name := cf.exp.bandit.Variants[arm]
		return name, cf.flag.Variations[name], bucket, nil
	}
	name, err := rule.split.Assign(bucket)
	if err != nil {
		return "", nil, -1, err
	}
	return name, cf.flag.Variations[name], bucket, nil
}

// A stale or offline snapshot serves the pre-registered percentage split.
func (exp *compiledExperiment) banditActive(generated time.Time) bool {
	return exp.bandit != nil && len(exp.banditProb) != 0 && !generated.IsZero() &&
		time.Since(generated) <= 5*time.Minute
}

// hashCoordinate returns the (key, epoch) pair for variant hashing: the
// experiment identity when bound, otherwise the flag key with epoch 0 (§6).
func (e *Evaluator) hashCoordinate(cf *compiledFlag) (string, int64) {
	if cf.exp != nil {
		return cf.exp.id, cf.exp.epoch
	}
	return cf.flag.Key, 0
}

// CanonicalUnitString renders any scalar unit value into the exact string
// that enters the bucketing hash. Exported so servers and SDKs in any
// language hash numeric unit IDs identically (int 42, int64 42 and float64
// 42 all canonicalize to "42"); non-scalars report false and must be
// treated as MISSING_BUCKET_KEY.
func CanonicalUnitString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		if t == "" {
			return "", false
		}
		return t, true
	default:
		if n, isNum := model.Number(v); isNum {
			return canonicalNumberString(n), true
		}
		return "", false
	}
}

func (e *Evaluator) unitValue(cf *compiledFlag, ctx model.EvaluationContext) (string, bool) {
	if cf.flag.Bucketing == nil || cf.flag.Bucketing.Unit == "" {
		return "", false
	}
	v, ok := ctx.Field(cf.flag.Bucketing.Unit)
	if !ok {
		return "", false
	}
	return CanonicalUnitString(v)
}

func canonicalNumberString(n float64) string {
	if n == math.Trunc(n) && math.Abs(n) < 1e15 {
		return fmt.Sprintf("%d", int64(n))
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

func (e *Evaluator) hashBucket(unit, key string, epoch int64, salt, purpose string) (int, error) {
	return bucketing.Bucket(bucketing.Inputs{
		Workspace: e.snap.Workspace, Environment: e.snap.Environment,
		Key: key, Epoch: epoch, Salt: salt, UnitValue: unit, Purpose: purpose,
	})
}

func notEnrolled(base model.Result, f *model.Flag) model.Result {
	base.VariationType = f.DefaultRule.Variation
	base.Value = f.Variations[base.VariationType]
	base.Reason = model.ReasonNotEnrolled
	return base
}

// missingBucketKey implements §6: no stable ID → deterministic default
// variation + MISSING_BUCKET_KEY, never a fresh random assignment.
func missingBucketKey(base model.Result, f *model.Flag) model.Result {
	base.Reason = model.ReasonError
	base.ErrorCode = model.ErrMissingBucketKey
	base.Failed = true
	base.ErrorDetails = "bucketing unit missing from evaluation context"
	if f.DefaultRule != nil && f.DefaultRule.Variation != "" {
		base.VariationType = f.DefaultRule.Variation
		base.Value = f.Variations[base.VariationType]
	}
	return base
}

func errorResult(base model.Result, code model.ErrorCode, details string) model.Result {
	base.Reason = model.ReasonError
	base.ErrorCode = code
	base.ErrorDetails = details
	base.Failed = true
	base.VariationType = ""
	base.Value = nil
	return base
}

func typeMismatch(base model.Result, err error) model.Result {
	base.Reason = model.ReasonError
	base.ErrorCode = model.ErrTypeMismatch
	base.ErrorDetails = err.Error()
	base.Failed = true
	base.VariationType = ""
	base.Value = nil
	base.Experiment = nil
	base.TrackEvents = false
	return base
}

func (e *Evaluator) matchRule(r *compiledRule, ctx model.EvaluationContext) bool {
	for i := range r.constraints {
		if !matchConstraint(&r.constraints[i], ctx) {
			return false
		}
	}
	if len(r.segments) > 0 {
		matched := false
		for _, seg := range r.segments {
			if e.matchSegment(seg, ctx, map[string]bool{}, 0) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (e *Evaluator) matchSegment(seg *compiledSegment, ctx model.EvaluationContext,
	visiting map[string]bool, depth int) bool {
	if depth > model.MaxSegmentDepth || visiting[seg.key] {
		return false // defensive: validation rejects cycles before this runs
	}
	visiting[seg.key] = true
	defer delete(visiting, seg.key)

	for i := range seg.constraints {
		if !matchConstraint(&seg.constraints[i], ctx) {
			return false
		}
	}
	if len(seg.nested) > 0 {
		matched := false
		for _, n := range seg.nested {
			if e.matchSegment(n, ctx, visiting, depth+1) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
