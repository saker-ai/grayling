package bandit

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"

	"github.com/saker-ai/grayling/pkg/model"
)

// Distribution returns the *operational* assignment probabilities. Thompson
// uses a fixed Monte Carlo quadrature to estimate probability of being best;
// the evaluator samples this vector, so the recorded propensity is exact for
// this version of the routing policy (rather than a posterior theta sample).
func Distribution(b *model.BanditBinding) ([]float64, error) {
	if b == nil || len(b.Variants) < 2 || len(b.Trials) != len(b.Variants) || len(b.Successes) != len(b.Variants) ||
		b.PriorAlpha <= 0 || b.PriorBeta <= 0 {
		return nil, fmt.Errorf("bandit: invalid published state")
	}
	for i := range b.Variants {
		if b.Trials[i] < 0 || b.Successes[i] < 0 || b.Successes[i] > b.Trials[i] {
			return nil, fmt.Errorf("bandit: invalid counts at arm %d", i)
		}
	}
	prob := make([]float64, len(b.Variants))
	switch b.Strategy {
	case "epsilon-greedy":
		if b.Epsilon < 0 || b.Epsilon >= 1 {
			return nil, fmt.Errorf("bandit: invalid epsilon")
		}
		best, bestMean := 0, math.Inf(-1)
		for i := range b.Variants {
			mean := (b.PriorAlpha + float64(b.Successes[i])) /
				(b.PriorAlpha + b.PriorBeta + float64(b.Trials[i]))
			if mean > bestMean {
				best, bestMean = i, mean
			}
			prob[i] = b.Epsilon / float64(len(prob))
		}
		prob[best] += 1 - b.Epsilon
	case "thompson":
		if b.Epsilon < 0 || b.Epsilon >= 1 {
			return nil, fmt.Errorf("bandit: invalid exploration floor")
		}
		const draws = 8192
		rng := rand.New(rand.NewPCG(0x476f4c6162, 0x62616e646974))
		for range draws {
			best, bestSample := 0, math.Inf(-1)
			for i := range b.Variants {
				v := betaSample(rng, b.PriorAlpha+float64(b.Successes[i]),
					b.PriorBeta+float64(b.Trials[i]-b.Successes[i]))
				if v > bestSample {
					best, bestSample = i, v
				}
			}
			prob[best]++
		}
		for i := range prob {
			prob[i] = (1-b.Epsilon)*(prob[i]/draws) + b.Epsilon/float64(len(prob))
		}
	default:
		return nil, fmt.Errorf("bandit: unknown strategy %q", b.Strategy)
	}
	return prob, nil
}

// Assign maps a unit to the immutable policy state. The hash includes the
// state version, so every replica with the same snapshot makes the same choice.
func Assign(b *model.BanditBinding, unit string, probabilities []float64) (int, float64) {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%d\x00%s", b.PolicyVersion, b.StateVersion, unit)
	bucket := float64(h.Sum64()>>11) / (1 << 53)
	arm := pickFixed(probabilities, bucket)
	return arm, probabilities[arm]
}
