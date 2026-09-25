// Package judge implements the LLM-as-judge online evaluation pipeline
// (docs/技术方案.md §10.4). Judges are PLUGGABLE via the Provider interface;
// tests run against a deterministic local MockProvider so the pipeline is fully
// exercised WITHOUT a real LLM key. A real HTTP provider (provider_http.go) is
// a config-only integration point — it refuses to run without an endpoint and
// API key and never fakes a model response.
//
// §10.4 requirements realized here:
//   - Pre-registered sampling probability; each sampled item records its
//     INCLUSION PROBABILITY so design-weighted statistics are possible
//     (不能仅把不同采样率的评分直接平均).
//   - POSITION RANDOMIZATION for pairwise comparison: the presented order of
//     (A, B) is randomized per item and the result de-randomized, which removes
//     systematic position bias from the aggregate (proved in judge_test.go).
//   - Recording of rubric / judge model / version with every score.
//   - Offline CALIBRATION against a human-labelled set (calibrate.go): accuracy,
//     Cohen's κ, position bias, length bias, and a Calibrated flag. An
//     uncalibrated judge is DIAGNOSTIC ONLY and must not drive auto-rollout or
//     rollback (guardrail.go enforces this at the decision-gate boundary).
//
// The package is deterministic under a fixed seed and depends only on the
// standard library (plus internal/stats for the decision-gate glue).
package judge

import (
	"context"
	"fmt"
	"math"
	mrand "math/rand/v2"
)

// PairwiseRequest is one pairwise comparison as PRESENTED to the judge (the
// pipeline, not the provider, owns position randomization).
type PairwiseRequest struct {
	Rubric         string            `json:"rubric"`
	Prompt         string            `json:"prompt"`
	ResponseFirst  string            `json:"responseFirst"`
	ResponseSecond string            `json:"responseSecond"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// PairwiseScore is the judge's verdict in PRESENTED order.
type PairwiseScore struct {
	// Winner is "first", "second" or "tie" relative to the presented order.
	Winner    string `json:"winner"`
	Rationale string `json:"rationale,omitempty"`
	Model     string `json:"model,omitempty"`
	Version   string `json:"version,omitempty"`
}

// SingleRequest is one rubric-scoring task for a single response.
type SingleRequest struct {
	Rubric   string            `json:"rubric"`
	Prompt   string            `json:"prompt"`
	Response string            `json:"response"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SingleScore is a scalar/label rubric score for one response.
type SingleScore struct {
	Value     float64 `json:"value"` // e.g. 0..1 quality
	Label     string  `json:"label,omitempty"`
	Rationale string  `json:"rationale,omitempty"`
	Model     string  `json:"model,omitempty"`
	Version   string  `json:"version,omitempty"`
}

// Provider is the pluggable LLM-judge adapter interface. Implementations:
// MockProvider (deterministic local, for tests) and HTTPProvider (real endpoint,
// config-only integration point). A provider returns verdicts in PRESENTED
// order; it never sees which candidate is "A" or "B".
type Provider interface {
	Name() string
	ScorePairwise(ctx context.Context, req PairwiseRequest) (PairwiseScore, error)
	ScoreSingle(ctx context.Context, req SingleRequest) (SingleScore, error)
}

// ---------------------------------------------------------------------------
// MockProvider — deterministic local judge (no real LLM, no key required)
// ---------------------------------------------------------------------------

// MockProvider is a deterministic, fully-local judge used to exercise the
// pipeline in tests without a real model. It scores responses by a Quality map
// (text → latent quality) and can be given a PositionBias and ErrorRate so
// calibration and position-randomization behavior are testable.
//
// IMPORTANT: MockProvider is NOT a language model and does not claim to judge
// text semantically — it exists so the pipeline (sampling, randomization,
// weighting, calibration, guardrail wiring) is verified end to end offline. Real
// judging requires configuring HTTPProvider with an actual endpoint/key.
type MockProvider struct {
	// Quality maps a response text to its latent quality (higher is better).
	// Texts absent from the map are treated as quality 0.
	Quality map[string]float64
	// PositionBias ∈ [0,1] is the probability the judge outputs the
	// FIRST-presented response as the winner regardless of quality (0.5 would be
	// a pure coin flip; values != 0.5 model a systematic position preference).
	// It is applied AFTER the quality winner is determined.
	PositionBias float64
	// ErrorRate ∈ [0,1] is the probability the judge flips away from its
	// (post-bias) pick — models judge noise so calibration can detect a bad
	// judge.
	ErrorRate float64
	// Model/Version are echoed into scores (recording §10.4 requires).
	Model   string
	Version string

	rng *mrand.Rand
}

// NewMockProvider builds a deterministic MockProvider (seed fixes the coin
// flips for position bias, ties and error).
func NewMockProvider(quality map[string]float64, positionBias, errorRate float64, seed uint64) *MockProvider {
	return &MockProvider{
		Quality:      quality,
		PositionBias: positionBias,
		ErrorRate:    errorRate,
		Model:        "mock-judge",
		Version:      "v0-local",
		rng:          mrand.New(mrand.NewPCG(seed, seed^0x5bf03635)),
	}
}

func (m *MockProvider) Name() string { return "mock" }

func (m *MockProvider) q(text string) float64 {
	if m.Quality == nil {
		return 0
	}
	return m.Quality[text]
}

// ScorePairwise returns the winner in presented order:
//  1. quality winner = argmax(Quality[first], Quality[second]); ties → coin flip
//  2. with probability PositionBias, override to "first" (position preference)
//  3. with probability ErrorRate, flip to the other side (judge noise)
func (m *MockProvider) ScorePairwise(_ context.Context, req PairwiseRequest) (PairwiseScore, error) {
	qf, qs := m.q(req.ResponseFirst), m.q(req.ResponseSecond)
	winner := "first"
	switch {
	case qf > qs:
		winner = "first"
	case qs > qf:
		winner = "second"
	default:
		if m.rng.Float64() < 0.5 {
			winner = "first"
		} else {
			winner = "second"
		}
	}
	if m.PositionBias > 0 && m.rng.Float64() < m.PositionBias {
		winner = "first"
	}
	if m.ErrorRate > 0 && m.rng.Float64() < m.ErrorRate {
		if winner == "first" {
			winner = "second"
		} else {
			winner = "first"
		}
	}
	return PairwiseScore{Winner: winner, Model: m.Model, Version: m.Version,
		Rationale: fmt.Sprintf("mock pairwise (qFirst=%.3f qSecond=%.3f)", qf, qs)}, nil
}

// ScoreSingle returns the latent quality of one response (clamped to [0,1] if it
// already lies in that range) as the rubric value.
func (m *MockProvider) ScoreSingle(_ context.Context, req SingleRequest) (SingleScore, error) {
	v := m.q(req.Response)
	label := "low"
	if v >= 0.5 {
		label = "high"
	}
	return SingleScore{Value: v, Label: label, Model: m.Model, Version: m.Version}, nil
}

// ---------------------------------------------------------------------------
// Pipeline — pre-registered sampling, position randomization, design weights
// ---------------------------------------------------------------------------

// JudgeItem is one candidate comparison to be (possibly) sampled and judged.
type JudgeItem struct {
	ID         string
	Prompt     string
	ResponseA  string  // the "control"/reference response
	ResponseB  string  // the "treatment"/candidate response
	LenA, LenB int     // response lengths (for length-bias analysis)
	InclProb   float64 // optional per-item inclusion probability (0 → use pipeline default)
}

// JudgedItem is the de-randomized result of one SAMPLED item, carrying its
// inclusion probability and design weight (§10.4: 统计时按设计加权).
type JudgedItem struct {
	ID             string  `json:"id"`
	Winner         string  `json:"winner"` // "A"|"B"|"tie" in ORIGINAL terms
	PresentedFirst string  `json:"presentedFirst"`
	RawWinner      string  `json:"rawWinner"` // "first"|"second"|"tie" as presented
	InclusionProb  float64 `json:"inclusionProb"`
	Weight         float64 `json:"weight"` // 1/InclusionProb
	LenA           int     `json:"lenA"`
	LenB           int     `json:"lenB"`
	JudgeModel     string  `json:"judgeModel"`
	JudgeVersion   string  `json:"judgeVersion"`
	Rubric         string  `json:"rubric"`
	Error          string  `json:"error,omitempty"`
}

// Pipeline judges a stream of items under a pre-registered sampling probability
// with per-item position randomization and inclusion-probability recording.
type Pipeline struct {
	provider   Provider
	rng        *mrand.Rand
	sampleProb float64
	rubric     string
}

// NewPipeline builds a judge pipeline. sampleProb ∈ (0,1] is the pre-registered
// inclusion probability (§10.4). seed makes sampling and randomization
// deterministic.
func NewPipeline(provider Provider, sampleProb float64, seed uint64, rubric string) (*Pipeline, error) {
	if provider == nil {
		return nil, fmt.Errorf("judge: nil provider")
	}
	if math.IsNaN(sampleProb) || sampleProb <= 0 || sampleProb > 1 {
		return nil, fmt.Errorf("judge: sampleProb must be in (0,1], got %v", sampleProb)
	}
	return &Pipeline{
		provider:   provider,
		rng:        mrand.New(mrand.NewPCG(seed, seed^0x2545f491)),
		sampleProb: sampleProb,
		rubric:     rubric,
	}, nil
}

// SampleProb returns the pre-registered inclusion probability.
func (p *Pipeline) SampleProb() float64 { return p.sampleProb }

// Run samples and judges items. Each item is included independently with its
// inclusion probability (item.InclProb if > 0, else the pipeline default); the
// presented (first, second) order is randomized 50/50 per included item and the
// verdict is de-randomized back to A/B. Included items are returned with their
// design weight (1/inclusion probability); skipped items are omitted.
func (p *Pipeline) Run(ctx context.Context, items []JudgeItem) []JudgedItem {
	out := make([]JudgedItem, 0, len(items))
	for _, it := range items {
		incl := p.sampleProb
		if it.InclProb > 0 {
			incl = it.InclProb
		}
		if incl > 1 {
			incl = 1
		}
		if p.rng.Float64() >= incl {
			continue // not sampled this pass
		}
		// Position randomization.
		first, second := it.ResponseA, it.ResponseB
		presentedFirst := "A"
		if p.rng.Float64() < 0.5 {
			first, second = it.ResponseB, it.ResponseA
			presentedFirst = "B"
		}
		res, err := p.provider.ScorePairwise(ctx, PairwiseRequest{
			Rubric: p.rubric, Prompt: it.Prompt,
			ResponseFirst: first, ResponseSecond: second,
		})
		ji := JudgedItem{
			ID: it.ID, PresentedFirst: presentedFirst, RawWinner: res.Winner,
			InclusionProb: incl, Weight: 1 / incl,
			LenA: it.LenA, LenB: it.LenB,
			JudgeModel: res.Model, JudgeVersion: res.Version, Rubric: p.rubric,
		}
		if ji.LenA == 0 {
			ji.LenA = len(it.ResponseA)
		}
		if ji.LenB == 0 {
			ji.LenB = len(it.ResponseB)
		}
		if err != nil {
			ji.Error = err.Error()
			out = append(out, ji)
			continue
		}
		// De-randomize the presented verdict back to original A/B terms.
		switch res.Winner {
		case "first":
			ji.Winner = presentedFirst
		case "second":
			if presentedFirst == "A" {
				ji.Winner = "B"
			} else {
				ji.Winner = "A"
			}
		default:
			ji.Winner = "tie"
		}
		out = append(out, ji)
	}
	return out
}

// WeightedWinRate computes the design-weighted P(B beats A) from judged items
// (ties count as half a win for each side), using each item's inclusion-prob
// weight (§10.4: 按设计加权). Returns (winRateB, totalWeight).
func WeightedWinRate(items []JudgedItem) (winRateB, totalWeight float64) {
	var wB, wTot float64
	for _, it := range items {
		if it.Error != "" {
			continue
		}
		w := it.Weight
		if w <= 0 || math.IsInf(w, 0) {
			continue
		}
		wTot += w
		switch it.Winner {
		case "B":
			wB += w
		case "tie":
			wB += 0.5 * w
		}
	}
	if wTot == 0 {
		return 0, 0
	}
	return wB / wTot, wTot
}
