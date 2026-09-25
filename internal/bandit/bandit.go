// Package bandit implements exploration/exploitation traffic allocation for
// GoLab experiments (docs/技术方案.md §10.3: Bandit 是探索优化策略，不与固定比例
// A/B 共用传统 p 值结论；分配概率及时间必须逐次记录). It is deliberately
// SEPARATE from the fixed-sample statistics engine: a bandit's adaptive
// allocation invalidates the classic p-value/CI, so bandit experiments must not
// be analyzed with Welch/z/decision-gate conclusions — they use the bandit's own
// posterior and regret accounting.
//
// Two policies are provided behind a common Policy interface:
//
//   - Thompson: Thompson sampling with Beta-Bernoulli posteriors (the default;
//     converges to putting nearly all traffic on the best arm while logging the
//     sampled allocation probabilities each round).
//   - EpsilonGreedy: ε-greedy over empirical posterior means (a simpler
//     reference policy).
//
// SAFETY CONSTRAINT (docs/技术方案.md §10.3 / §5.3): when an SRM alarm or a
// guardrail breach is signalled, exploration is FROZEN — the policy stops
// adaptive sampling and serves the pre-registered fixed proportions exactly
// (a deterministic, non-exploring split) until explicitly unfrozen. This keeps
// an unhealthy experiment from continuing to shift traffic on unreliable data.
//
// The package is deterministic: all randomness flows through a seeded
// math/rand/v2 generator, so simulations are reproducible with a fixed seed. No
// wall-clock time is read; each allocation carries a logical Step (monotonic
// counter) and the caller/engine associates wall-clock timestamps (the rollout
// engine owns the clock, cf. §5.3 fake-clock tests).
package bandit

import (
	"fmt"
	"math"
	mrand "math/rand/v2"
)

// AllocationRecord is the per-round log entry required by docs/技术方案.md §10.3
// (分配概率及时间必须逐次记录). Probabilities is the vector that drove the
// decision this round: the sampled Thompson θ per arm, or the fixed proportions
// when frozen. PosteriorMean is the current posterior mean reward per arm.
type AllocationRecord struct {
	// Step is the logical round counter (1-based). The caller associates a
	// wall-clock timestamp; the bandit itself reads no clock.
	Step int64 `json:"step"`
	// Arm is the index of the allocated arm.
	Arm int `json:"arm"`
	// ArmName is the allocated arm's label (convenience for logs/audit).
	ArmName string `json:"armName"`
	// Probabilities is the decision vector this round (sampled θ or fixed
	// proportions when frozen).
	Probabilities []float64 `json:"probabilities"`
	// PosteriorMean is the posterior mean reward per arm AFTER this round's
	// allocation but BEFORE its reward update.
	PosteriorMean []float64 `json:"posteriorMean"`
	// Frozen records whether exploration was frozen for this allocation.
	Frozen bool `json:"frozen"`
}

// Policy is the common interface for bandit allocation strategies.
type Policy interface {
	// Arms returns the arm labels in stable index order.
	Arms() []string
	// Allocate picks an arm for one unit. unitBucket ∈ [0,1) is the unit's
	// deterministic hash bucket (used for the frozen fixed-proportion split so
	// the same unit maps to the same arm while frozen). It returns the chosen
	// arm index and the per-round AllocationRecord.
	Allocate(unitBucket float64) (int, AllocationRecord)
	// Update records a reward in [0,1] (Bernoulli success probability scale)
	// for the given arm.
	Update(arm int, reward float64)
	// PosteriorMean returns the current posterior mean reward per arm.
	PosteriorMean() []float64
	// Counts returns the cumulative allocation count per arm.
	Counts() []int64
	// Freeze halts exploration (safety constraint); reason is recorded.
	Freeze(reason string)
	// Unfreeze resumes adaptive exploration.
	Unfreeze()
	// Frozen reports whether exploration is currently halted.
	Frozen() bool
	// FreezeReason returns the current/last freeze reason ("" when not frozen).
	FreezeReason() string
	// Log returns the retained allocation records (bounded, oldest evicted).
	Log() []AllocationRecord
}

// gammaSample draws from Gamma(shape, 1) by Marsaglia–Tsang. Shapes >= 1 use
// the direct method; shapes < 1 are boosted via Gamma(a) = Gamma(a+1)·U^{1/a}.
func gammaSample(rng *mrand.Rand, shape float64) float64 {
	if shape < 1 {
		return gammaSample(rng, shape+1) * math.Pow(rng.Float64(), 1/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// betaSample draws from Beta(a, b) as Ga/(Ga+Gb) with independent Gammas.
func betaSample(rng *mrand.Rand, a, b float64) float64 {
	ga := gammaSample(rng, a)
	gb := gammaSample(rng, b)
	if ga+gb == 0 {
		return 0.5
	}
	return ga / (ga + gb)
}

// defaultMaxLog bounds the retained allocation log so a long-running bandit does
// not grow memory without limit.
const defaultMaxLog = 4096

// ---------------------------------------------------------------------------
// Thompson sampling (Beta-Bernoulli)
// ---------------------------------------------------------------------------

// Thompson is a Beta-Bernoulli Thompson-sampling bandit. Each arm k carries a
// Beta(alpha_k, beta_k) posterior initialized to Beta(priorAlpha, priorBeta);
// Allocate samples θ_k from each posterior and picks argmax_k θ_k; Update folds
// a [0,1] reward into the posterior (alpha += reward, beta += 1 − reward).
type Thompson struct {
	arms  []string
	alpha []float64
	beta  []float64
	fixed []float64 // pre-registered proportions for the frozen split

	frozen       bool
	freezeReason string

	rng   *mrand.Rand
	count []int64
	step  int64

	log    []AllocationRecord
	maxLog int
}

// NewThompson builds a Thompson-sampling bandit over arms with fixed
// proportions (used only when frozen). priorAlpha/priorBeta set the Beta prior
// (<= 0 selects the uniform Beta(1,1)). seed makes the policy deterministic.
// fixedProportions must be non-negative and sum to 1 (within 1e-9); pass nil to
// default to a uniform split.
func NewThompson(arms []string, fixedProportions []float64, priorAlpha, priorBeta float64, seed uint64) (*Thompson, error) {
	if len(arms) < 2 {
		return nil, fmt.Errorf("bandit: need at least 2 arms, got %d", len(arms))
	}
	seen := map[string]bool{}
	for _, a := range arms {
		if a == "" {
			return nil, fmt.Errorf("bandit: empty arm name")
		}
		if seen[a] {
			return nil, fmt.Errorf("bandit: duplicate arm %q", a)
		}
		seen[a] = true
	}
	fixed, err := normalizeFixed(fixedProportions, len(arms))
	if err != nil {
		return nil, err
	}
	if priorAlpha <= 0 {
		priorAlpha = 1
	}
	if priorBeta <= 0 {
		priorBeta = 1
	}
	alpha := make([]float64, len(arms))
	beta := make([]float64, len(arms))
	for i := range arms {
		alpha[i] = priorAlpha
		beta[i] = priorBeta
	}
	return &Thompson{
		arms: append([]string(nil), arms...), alpha: alpha, beta: beta, fixed: fixed,
		rng: mrand.New(mrand.NewPCG(seed, seed^0x9e3779b9)), count: make([]int64, len(arms)),
		maxLog: defaultMaxLog,
	}, nil
}

func normalizeFixed(fixedProportions []float64, k int) ([]float64, error) {
	if fixedProportions == nil {
		out := make([]float64, k)
		for i := range out {
			out[i] = 1 / float64(k)
		}
		return out, nil
	}
	if len(fixedProportions) != k {
		return nil, fmt.Errorf("bandit: fixedProportions length %d != arms %d", len(fixedProportions), k)
	}
	sum := 0.0
	for _, p := range fixedProportions {
		if p < 0 || math.IsNaN(p) {
			return nil, fmt.Errorf("bandit: fixedProportions must be non-negative and finite")
		}
		sum += p
	}
	if math.Abs(sum-1) > 1e-9 {
		return nil, fmt.Errorf("bandit: fixedProportions sum to %v, want 1", sum)
	}
	return append([]float64(nil), fixedProportions...), nil
}

func (t *Thompson) Arms() []string { return append([]string(nil), t.arms...) }

// pickFixed maps a unit bucket ∈ [0,1) to an arm via the cumulative fixed
// proportions (deterministic, stable while frozen).
func pickFixed(fixed []float64, unitBucket float64) int {
	if unitBucket < 0 {
		unitBucket = 0
	}
	if unitBucket >= 1 {
		unitBucket = math.Nextafter(1, 0)
	}
	cum := 0.0
	for i, p := range fixed {
		cum += p
		if unitBucket < cum {
			return i
		}
	}
	return len(fixed) - 1
}

func (t *Thompson) posteriorMeanLocked() []float64 {
	out := make([]float64, len(t.arms))
	for i := range t.arms {
		out[i] = t.alpha[i] / (t.alpha[i] + t.beta[i])
	}
	return out
}

func (t *Thompson) Allocate(unitBucket float64) (int, AllocationRecord) {
	t.step++
	probs := make([]float64, len(t.arms))
	var arm int
	if t.frozen {
		// Frozen: no exploration — deterministic fixed-proportion split.
		for i := range probs {
			probs[i] = t.fixed[i]
		}
		arm = pickFixed(t.fixed, unitBucket)
	} else {
		// Thompson sampling: sample each posterior, pick argmax.
		best := math.Inf(-1)
		for i := range t.arms {
			th := betaSample(t.rng, t.alpha[i], t.beta[i])
			probs[i] = th
			if th > best {
				best = th
				arm = i
			}
		}
	}
	t.count[arm]++
	rec := AllocationRecord{
		Step: t.step, Arm: arm, ArmName: t.arms[arm],
		Probabilities: probs, PosteriorMean: t.posteriorMeanLocked(), Frozen: t.frozen,
	}
	t.appendLog(rec)
	return arm, rec
}

func (t *Thompson) Update(arm int, reward float64) {
	if arm < 0 || arm >= len(t.arms) {
		return
	}
	if reward < 0 {
		reward = 0
	}
	if reward > 1 {
		reward = 1
	}
	t.alpha[arm] += reward
	t.beta[arm] += 1 - reward
}

func (t *Thompson) PosteriorMean() []float64 { return t.posteriorMeanLocked() }
func (t *Thompson) Counts() []int64          { return append([]int64(nil), t.count...) }
func (t *Thompson) Frozen() bool             { return t.frozen }
func (t *Thompson) FreezeReason() string     { return t.freezeReason }

func (t *Thompson) Freeze(reason string) {
	t.frozen = true
	t.freezeReason = reason
}

func (t *Thompson) Unfreeze() {
	t.frozen = false
	t.freezeReason = ""
}

func (t *Thompson) appendLog(rec AllocationRecord) {
	t.log = append(t.log, rec)
	if len(t.log) > t.maxLog {
		// Drop the oldest half to amortize eviction.
		drop := len(t.log) - t.maxLog
		t.log = append([]AllocationRecord(nil), t.log[drop:]...)
	}
}

func (t *Thompson) Log() []AllocationRecord { return append([]AllocationRecord(nil), t.log...) }

// ApplySafety is the guard/SRM hook (docs/技术方案.md §10.3/§5.3): freeze
// exploration when the split is untrustworthy (SRM alarm) or a guardrail is
// breached; the caller passes the health signals it computed elsewhere. Returns
// whether the bandit is frozen after the call.
func (t *Thompson) ApplySafety(srmHealthy, guardrailsOK bool, reason string) bool {
	if !srmHealthy || !guardrailsOK {
		if !t.frozen {
			if reason == "" {
				reason = "safety"
				if !srmHealthy {
					reason = "SRM alarm"
				} else if !guardrailsOK {
					reason = "guardrail breach"
				}
			}
			t.Freeze(reason)
		}
	}
	return t.frozen
}

// ---------------------------------------------------------------------------
// ε-greedy (reference policy)
// ---------------------------------------------------------------------------

// EpsilonGreedy is an ε-greedy bandit over empirical posterior means: with
// probability ε it explores a uniformly random arm, otherwise it exploits the
// arm with the highest posterior mean. It shares the same Beta posterior and
// safety-freeze semantics as Thompson.
type EpsilonGreedy struct {
	*Thompson
	epsilon float64
}

// NewEpsilonGreedy builds an ε-greedy bandit. epsilon must be in [0,1).
func NewEpsilonGreedy(arms []string, fixedProportions []float64, priorAlpha, priorBeta, epsilon float64, seed uint64) (*EpsilonGreedy, error) {
	if epsilon < 0 || epsilon >= 1 {
		return nil, fmt.Errorf("bandit: epsilon must be in [0,1), got %v", epsilon)
	}
	th, err := NewThompson(arms, fixedProportions, priorAlpha, priorBeta, seed)
	if err != nil {
		return nil, err
	}
	return &EpsilonGreedy{Thompson: th, epsilon: epsilon}, nil
}

// Allocate overrides Thompson sampling with ε-greedy selection (frozen behavior
// is inherited unchanged).
func (e *EpsilonGreedy) Allocate(unitBucket float64) (int, AllocationRecord) {
	e.step++
	k := len(e.arms)
	probs := make([]float64, k)
	pm := e.posteriorMeanLocked()
	var arm int
	if e.frozen {
		for i := range probs {
			probs[i] = e.fixed[i]
		}
		arm = pickFixed(e.fixed, unitBucket)
	} else if e.rng.Float64() < e.epsilon {
		arm = e.rng.IntN(k)
		for i := range probs {
			probs[i] = pm[i]
		}
	} else {
		best := math.Inf(-1)
		for i := 0; i < k; i++ {
			probs[i] = pm[i]
			if pm[i] > best {
				best = pm[i]
				arm = i
			}
		}
	}
	e.count[arm]++
	rec := AllocationRecord{
		Step: e.step, Arm: arm, ArmName: e.arms[arm],
		Probabilities: probs, PosteriorMean: pm, Frozen: e.frozen,
	}
	e.appendLog(rec)
	return arm, rec
}
