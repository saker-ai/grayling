package judge

import "math"

// Offline calibration of a judge against a HUMAN-labelled gold set
// (docs/技术方案.md §10.4: 记录人工校准集结果；校准不达标时质量指标只作诊断).
// The metrics are the standard inter-rater agreement statistics: raw agreement,
// Cohen's κ (chance-corrected agreement), plus position-bias and length-bias
// diagnostics that §10.4 requires to be disclosed.

// CalibrationItem is one gold-labelled comparison. Gold and Judge are in
// ORIGINAL A/B/tie terms (de-randomized). PresentedFirst and the lengths drive
// the bias diagnostics.
type CalibrationItem struct {
	ID    string `json:"id"`
	Gold  string `json:"gold"`  // human label: "A"|"B"|"tie"
	Judge string `json:"judge"` // judge label (de-randomized): "A"|"B"|"tie"
	// PresentedFirst is which candidate ("A"|"B") was shown in the first
	// position for this item (position randomization record).
	PresentedFirst string `json:"presentedFirst"`
	LenA           int    `json:"lenA"`
	LenB           int    `json:"lenB"`
}

// CalibrationConfig holds the pre-registered acceptance thresholds. Zero fields
// select defaults (MinN=30, MinKappa=0.6 "substantial", MaxPositionBias=0.10).
type CalibrationConfig struct {
	MinN            int     `json:"minN"`
	MinKappa        float64 `json:"minKappa"`
	MaxPositionBias float64 `json:"maxPositionBias"`
}

func (c CalibrationConfig) resolve() CalibrationConfig {
	if c.MinN <= 0 {
		c.MinN = 30
	}
	if c.MinKappa <= 0 {
		c.MinKappa = 0.6
	}
	if c.MaxPositionBias <= 0 {
		c.MaxPositionBias = 0.10
	}
	return c
}

// CalibrationReport is the outcome of Calibrate. Calibrated=false means the
// judge is DIAGNOSTIC ONLY — its quality metrics must not drive auto-rollout or
// rollback (docs/技术方案.md §10.4).
type CalibrationReport struct {
	N            int      `json:"n"`
	Accuracy     float64  `json:"accuracy"`     // observed agreement p_o
	CohenKappa   float64  `json:"cohenKappa"`   // chance-corrected agreement
	PositionBias float64  `json:"positionBias"` // (first−second)/(first+second); 0 = unbiased
	LengthBias   float64  `json:"lengthBias"`   // (longer−shorter)/(longer+shorter); 0 = unbiased
	Categories   []string `json:"categories"`
	Calibrated   bool     `json:"calibrated"`
	Reason       string   `json:"reason,omitempty"` // why uncalibrated (if any)
	// Thresholds echo the accepted config for traceability.
	MinN            int     `json:"minN"`
	MinKappa        float64 `json:"minKappa"`
	MaxPositionBias float64 `json:"maxPositionBias"`
}

// cohenKappa computes κ = (p_o − p_e)/(1 − p_e) over the label categories from
// paired gold/judge labels. p_e = Σ_c (n_gold_c/N)(n_judge_c/N). Degenerate
// cases (p_e = 1) return 1 for perfect observed agreement, else 0.
func cohenKappa(gold, judge []string) (kappa, po float64, cats []string) {
	n := len(gold)
	if n == 0 || n != len(judge) {
		return 0, 0, nil
	}
	catSet := map[string]bool{}
	for _, g := range gold {
		catSet[g] = true
	}
	for _, j := range judge {
		catSet[j] = true
	}
	for c := range catSet {
		cats = append(cats, c)
	}
	// Stable order for deterministic reporting.
	sortStrings(cats)

	agree := 0
	goldN := map[string]int{}
	judgeN := map[string]int{}
	for i := 0; i < n; i++ {
		goldN[gold[i]]++
		judgeN[judge[i]]++
		if gold[i] == judge[i] {
			agree++
		}
	}
	po = float64(agree) / float64(n)
	pe := 0.0
	for _, c := range cats {
		pe += (float64(goldN[c]) / float64(n)) * (float64(judgeN[c]) / float64(n))
	}
	if math.Abs(1-pe) < 1e-15 {
		if po == 1 {
			return 1, po, cats
		}
		return 0, po, cats
	}
	return (po - pe) / (1 - pe), po, cats
}

// sortStrings is a tiny insertion sort (avoids importing sort for one call).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Calibrate scores a judge against the gold set and decides whether it is
// calibrated well enough to drive automation (docs/技术方案.md §10.4).
func Calibrate(items []CalibrationItem, cfg CalibrationConfig) CalibrationReport {
	cfg = cfg.resolve()
	rep := CalibrationReport{
		N:               len(items),
		MinN:            cfg.MinN,
		MinKappa:        cfg.MinKappa,
		MaxPositionBias: cfg.MaxPositionBias,
	}
	if len(items) == 0 {
		rep.Reason = "no calibration items"
		return rep
	}
	gold := make([]string, len(items))
	judge := make([]string, len(items))
	for i, it := range items {
		gold[i] = it.Gold
		judge[i] = it.Judge
	}
	rep.CohenKappa, rep.Accuracy, rep.Categories = cohenKappa(gold, judge)

	// Position bias: among non-tie verdicts, does the judge favor the
	// first-presented candidate?
	var firstPick, secondPick int
	// Length bias: among non-tie verdicts with unequal lengths, does the judge
	// favor the longer response?
	var longerPick, shorterPick int
	for _, it := range items {
		if it.Judge != "tie" && it.PresentedFirst != "" {
			if it.Judge == it.PresentedFirst {
				firstPick++
			} else {
				secondPick++
			}
		}
		if it.Judge != "tie" && it.LenA != it.LenB {
			longer := "A"
			if it.LenB > it.LenA {
				longer = "B"
			}
			if it.Judge == longer {
				longerPick++
			} else {
				shorterPick++
			}
		}
	}
	if firstPick+secondPick > 0 {
		rep.PositionBias = float64(firstPick-secondPick) / float64(firstPick+secondPick)
	}
	if longerPick+shorterPick > 0 {
		rep.LengthBias = float64(longerPick-shorterPick) / float64(longerPick+shorterPick)
	}

	// Calibration decision (§10.4): enough data, acceptable κ, acceptable
	// position bias. Length bias is DISCLOSED but not a hard gate (it is
	// mitigated by rubric/length controls, and reported for transparency).
	switch {
	case rep.N < cfg.MinN:
		rep.Reason = "sample too small"
	case rep.CohenKappa < cfg.MinKappa:
		rep.Reason = "kappa below threshold"
	case math.Abs(rep.PositionBias) > cfg.MaxPositionBias:
		rep.Reason = "position bias above threshold"
	default:
		rep.Calibrated = true
	}
	return rep
}
