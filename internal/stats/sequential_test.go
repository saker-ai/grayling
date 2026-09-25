package stats

import (
	"math"
	mrand "math/rand/v2"
	"testing"
)

// ---------------------------------------------------------------------------
// Sequential (mSPRT): numerical validation of the closed form against the
// defining Gaussian-mixture integral, then the peeking Type-I proof.
// ---------------------------------------------------------------------------

// mixtureLRNumeric evaluates the mixture likelihood ratio by direct numeric
// integration of its definition
//
//	Λ = ∫ exp(θ·d̂/V − θ²/(2V)) · N(θ; 0, τ²) dθ
//
// over a wide range with the trapezoid rule. Used to cross-check the closed
// form in mSPRTLogLambda (analytic solution vs independent quadrature).
func mixtureLRNumeric(diff, V, tau float64) float64 {
	L := 14 * tau
	const steps = 400000
	h := 2 * L / float64(steps)
	sum := 0.0
	for i := 0; i <= steps; i++ {
		th := -L + float64(i)*h
		lr := math.Exp(th*diff/V - th*th/(2*V))
		prior := math.Exp(-th*th/(2*tau*tau)) / (tau * math.Sqrt(2*math.Pi))
		w := h
		if i == 0 || i == steps {
			w = h / 2
		}
		sum += w * lr * prior
	}
	return sum
}

func TestMSPRTClosedFormMatchesNumericIntegration(t *testing.T) {
	cases := []struct{ diff, V, tau float64 }{
		{0.5, 0.04, 0.3},
		{0.0, 0.02, 0.2},
		{-0.31, 0.11, 0.5},
		{1.2, 0.5, 0.8},
	}
	for _, tc := range cases {
		closed := math.Exp(mSPRTLogLambda(tc.diff, tc.V, tc.tau))
		numeric := mixtureLRNumeric(tc.diff, tc.V, tc.tau)
		rel := math.Abs(closed-numeric) / numeric
		if rel > 1e-7 {
			t.Fatalf("Λ mismatch at diff=%v V=%v tau=%v: closed=%v numeric=%v (rel %v)",
				tc.diff, tc.V, tc.tau, closed, numeric, rel)
		}
	}
	t.Logf("mSPRT closed form matches numeric integration to <1e-7 relative")
}

// TestMSPRTConfidenceSequenceSelfConsistency checks that the confidence-sequence
// half-width u is exactly the point where the mixture likelihood ratio reaches
// 1/alpha: Λ(u) = 1/alpha ⟺ mSPRTLogLambda(u) = −log(alpha). This ties the CS
// boundary to the p-value definition (they must agree at the crossing).
func TestMSPRTConfidenceSequenceSelfConsistency(t *testing.T) {
	for _, V := range []float64{0.01, 0.05, 0.3} {
		for _, tau := range []float64{0.1, 0.3, 0.7} {
			for _, alpha := range []float64{0.01, 0.05, 0.1} {
				u := mSPRTConfidenceHalfWidth(V, tau, alpha)
				logLam := mSPRTLogLambda(u, V, tau)
				if math.Abs(logLam-math.Log(1/alpha)) > 1e-9 {
					t.Fatalf("CS boundary inconsistent at V=%v tau=%v alpha=%v: logΛ(u)=%v want %v",
						V, tau, alpha, logLam, math.Log(1/alpha))
				}
			}
		}
	}
}

func TestMSPRTKnownEdgeBehavior(t *testing.T) {
	a := meanStatsOf(1, 2, 3, 4, 5) // mean 3
	// diff = 0 → Λ = sqrt(V/(V+τ²)) < 1 → p = 1, not significant.
	res, err := SequentialMeanTest(a, a, 0.05, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.PValue != 1 || res.Significant {
		t.Fatalf("zero diff must give p=1, not significant: %+v", res)
	}
	if res.LogLambda >= 0 {
		t.Fatalf("zero diff must give logΛ < 0: %v", res.LogLambda)
	}
	// p monotone in |diff|: bigger separation → smaller p.
	b := meanStatsOf(6, 7, 8, 9, 10) // mean 8
	r1, _ := SequentialMeanTest(a, b, 0.05, 0.3)
	bigger := meanStatsOf(11, 12, 13, 14, 15) // mean 13, larger diff
	r2, _ := SequentialMeanTest(a, bigger, 0.05, 0.3)
	if !(r2.PValue < r1.PValue) {
		t.Fatalf("p must decrease with |diff|: %v then %v", r1.PValue, r2.PValue)
	}
	// Errors.
	if _, err := SequentialMeanTest(a, b, 0.05, 0); err == nil {
		t.Fatal("tau=0 must error")
	}
	if _, err := SequentialMeanTest(a, b, 0, 0.3); err == nil {
		t.Fatal("alpha=0 must error")
	}
	if _, err := SequentialMeanTest(meanStatsOf(1), b, 0.05, 0.3); err == nil {
		t.Fatal("N<2 must error")
	}
}

// drawMeanArmSeq streams per-arm normal draws into MeanStats blocks; the peeking
// simulation accumulates incrementally across looks.

func TestSequentialPeekingTypeIAndCoverage(t *testing.T) {
	const (
		iters  = 1500
		maxN   = 500
		step   = 25 // 20 looks — heavy peeking
		alpha  = 0.05
		tau    = 0.3
		effect = 0.0 // H0
	)
	rng := mrand.New(mrand.NewPCG(707, 707))
	seqReject, welchReject, csCover := 0, 0, 0
	for it := 0; it < iters; it++ {
		xa := make([]float64, maxN)
		xb := make([]float64, maxN)
		for i := 0; i < maxN; i++ {
			xa[i] = rng.NormFloat64()
			xb[i] = effect + rng.NormFloat64()
		}
		var ma, mb MeanStats
		everSeq, everWelch, alwaysCover := false, false, true
		for n := step; n <= maxN; n += step {
			for i := n - step; i < n; i++ {
				ma.N++
				ma.Sum += xa[i]
				ma.SumSquares += xa[i] * xa[i]
				mb.N++
				mb.Sum += xb[i]
				mb.SumSquares += xb[i] * xb[i]
			}
			if seq, err := SequentialMeanTest(ma, mb, alpha, tau); err == nil {
				if seq.Significant {
					everSeq = true
				}
				if !(seq.CSLow <= effect && effect <= seq.CSHigh) {
					alwaysCover = false
				}
			}
			// Fixed-sample Welch peeked at the same looks (the forbidden
			// behavior §10.3 warns about) — for contrast.
			if w, err := WelchTTest(ma, mb, alpha); err == nil && w.Significant {
				everWelch = true
			}
		}
		if everSeq {
			seqReject++
		}
		if everWelch {
			welchReject++
		}
		if alwaysCover {
			csCover++
		}
	}
	seqTypeI := float64(seqReject) / float64(iters)
	welchTypeI := float64(welchReject) / float64(iters)
	coverage := float64(csCover) / float64(iters)

	// Anytime validity: sequential Type-I stays ≤ alpha (+MC tolerance) DESPITE
	// 20 looks.
	if seqTypeI > alpha+0.02 {
		t.Fatalf("sequential peeking Type-I = %v, want <= %v (anytime validity broken)", seqTypeI, alpha+0.02)
	}
	// Contrast: fixed-sample Welch peeked at 20 looks MUST inflate well past
	// alpha — this is the failure mode the sequential test avoids.
	if welchTypeI < 2*alpha {
		t.Fatalf("expected fixed-sample peeking inflation >= %v, got %v (test not demonstrating the contrast)", 2*alpha, welchTypeI)
	}
	// Time-uniform coverage: the true effect is inside the CS at EVERY look in
	// >= 1−alpha of runs.
	if coverage < 0.93 {
		t.Fatalf("confidence-sequence time-uniform coverage = %v, want >= 0.93", coverage)
	}
	t.Logf("sequential peeking (20 looks): seqTypeI=%.4f (<=alpha) vs fixed-sample Welch peeking=%.4f (inflated); CS coverage=%.4f (iters=%d seed=707)",
		seqTypeI, welchTypeI, coverage, iters)
}

func TestSequentialPowerUnderH1(t *testing.T) {
	const (
		iters  = 1000
		maxN   = 500
		step   = 25
		alpha  = 0.05
		tau    = 0.3
		effect = 0.5 // H1: treatment mean shifted +0.5
	)
	rng := mrand.New(mrand.NewPCG(808, 808))
	reject := 0
	for it := 0; it < iters; it++ {
		xa := make([]float64, maxN)
		xb := make([]float64, maxN)
		for i := 0; i < maxN; i++ {
			xa[i] = rng.NormFloat64()
			xb[i] = effect + rng.NormFloat64()
		}
		var ma, mb MeanStats
		everSeq := false
		for n := step; n <= maxN; n += step {
			for i := n - step; i < n; i++ {
				ma.N++
				ma.Sum += xa[i]
				ma.SumSquares += xa[i] * xa[i]
				mb.N++
				mb.Sum += xb[i]
				mb.SumSquares += xb[i] * xb[i]
			}
			if seq, err := SequentialMeanTest(ma, mb, alpha, tau); err == nil && seq.Significant {
				everSeq = true
			}
		}
		if everSeq {
			reject++
		}
	}
	power := float64(reject) / float64(iters)
	if power < 0.6 {
		t.Fatalf("sequential power at effect=0.5, maxN=500 = %v, want > 0.6", power)
	}
	t.Logf("sequential power=%.4f (effect=0.5, 20 looks, iters=%d seed=808)", power, iters)
}
