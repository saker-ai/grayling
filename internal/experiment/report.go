// Package experiment builds the analysis report for a pre-registered
// experiment from PostgreSQL raw events or local unit-level aggregates
// (docs/技术方案.md §10).
//
// Legacy reports are two-arm, single-primary fixed-sample analyses. Explicit
// Holm families also support multiple mean/proportion metrics and variants.
// The analysis population is the RANDOMIZED subset — units exposed through
// the percentage split; deterministic targeting/default arms are reported
// for visibility but never enter the primary statistics or SRM.
//
// The report is pure: it reads aggregates and never mutates experiment
// state. Transitioning to Analyzed/Archived is an explicit admin action.
package experiment

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/stats"
	"github.com/saker-ai/grayling/pkg/model"
)

// Report is the full analysis output for one experiment epoch.
type Report struct {
	ExperimentID             string                        `json:"experimentId"`
	Epoch                    int64                         `json:"epoch"`
	FlagKey                  string                        `json:"flagKey"`
	State                    model.ExperimentState         `json:"state"`
	GeneratedAt              time.Time                     `json:"generatedAt"`
	Window                   platform.AnalysisWindowStatus `json:"window"`
	PrimaryAttributionWindow time.Duration                 `json:"primaryAttributionWindow,omitzero"`
	ExposureMode             string                        `json:"exposureMode,omitempty"`
	Diagnostics              *Diagnostics                  `json:"diagnostics,omitempty"`

	AnalysisMethod string  `json:"analysisMethod"`
	Alpha          float64 `json:"alpha"`
	MDE            float64 `json:"mde"`
	Control        string  `json:"control"`
	Treatment      string  `json:"treatment"`

	// Healthy is false when any data-quality gate failed; an unhealthy
	// report must never drive ship/rollback automation (§5.3: 统计不可用、
	// SRM、推送异常时暂停，不自动宣称成功).
	Healthy      bool     `json:"healthy"`
	HealthIssues []string `json:"healthIssues,omitempty"`

	SRM                 *stats.SRMResult       `json:"srm,omitempty"`
	PerVariant          []VariantReport        `json:"perVariant"`
	Primary             *PrimaryResult         `json:"primary,omitempty"`
	PrimaryError        string                 `json:"primaryError,omitempty"`
	Guardrails          []stats.GuardrailCheck `json:"guardrails,omitempty"`
	Decision            stats.Decision         `json:"decision"`
	MinRuntimeSatisfied bool                   `json:"minRuntimeSatisfied"`

	// ---- P5 views, presented SEPARATELY from the frequentist decision ----
	// docs/技术方案.md §10.3: 不能将"默认 Bayesian"和"固定频率派 95% CI"混成同一
	// 决策规则. Neither Sequential nor Bayesian ever drives the ship/rollback
	// gate above; they are parallel decision-support views.
	//
	// Sequential is the always-valid (mSPRT) view for continuous monitoring: its
	// p-value/confidence sequence stay valid under repeated looks (§10.3), so it
	// is the correct lens for the rollout engine's per-tick peeking, while the
	// fixed-sample Primary remains the pre-registered decision.
	Sequential *stats.SequentialResult `json:"sequential,omitempty"`
	// Bayesian is the posterior / expected-loss / probability-of-superiority
	// view (mean or proportion family depending on the metric type).
	Bayesian            *BayesianSection     `json:"bayesian,omitempty"`
	Bandit              *BanditSection       `json:"bandit,omitempty"`
	RegistrationVersion string               `json:"registrationVersion,omitempty"`
	PrimaryMetricRef    *platform.MetricRef  `json:"primaryMetricRef,omitempty"`
	GuardrailMetricRefs []platform.MetricRef `json:"guardrailMetricRefs,omitempty"`
	Holm                *HolmSection         `json:"holm,omitempty"`
	Journeys            []JourneySection     `json:"journeys,omitempty"`
}

// BayesianSection carries the Bayesian posterior summary, kept structurally
// separate from the frequentist Primary/Decision so the two paradigms are never
// conflated (docs/技术方案.md §10.3).
type BayesianSection struct {
	Mean       *stats.BayesianMeanResult       `json:"mean,omitempty"`
	Proportion *stats.BayesianProportionResult `json:"proportion,omitempty"`
}

// VariantReport exposes per-arm volumes. RandomizedUnits is the SRM and
// primary-analysis denominator; ExposedUnits additionally counts units that
// reached the arm through deterministic rules (reported for visibility).
type VariantReport struct {
	Variant         string  `json:"variant"`
	ExposedUnits    int64   `json:"exposedUnits"`
	RandomizedUnits int64   `json:"randomizedUnits"`
	Exposures       int64   `json:"exposures"`
	ConvertedUnits  int64   `json:"convertedUnits"` // among randomized units
	PrimaryValue    float64 `json:"primaryValue"`   // conversion rate or mean of per-unit means
}

// PrimaryResult carries the pre-registered test outcome in a uniform shape
// plus the native stats struct for detail. Type is one of "welch",
// "two-proportion-z", "ratio-delta", "quantile" or "cuped".
type PrimaryResult struct {
	Type               string                  `json:"type"` // "welch" | "two-proportion-z" | "ratio-delta" | "quantile" | "cuped"
	PValue             float64                 `json:"pValue"`
	Diff               float64                 `json:"diff"`
	RelativeDiff       float64                 `json:"relativeDiff"`
	RelativeMeaningful bool                    `json:"relativeMeaningful"`
	CILow              float64                 `json:"ciLow"`
	CIHigh             float64                 `json:"ciHigh"`
	Significant        bool                    `json:"significant"`
	Welch              *stats.WelchResult      `json:"welch,omitempty"`
	Proportion         *stats.ProportionResult `json:"proportion,omitempty"`
	Ratio              *stats.RatioResult      `json:"ratio,omitempty"`
	Quantile           *stats.QuantileResult   `json:"quantile,omitempty"`
	CUPED              *stats.CUPEDResult      `json:"cuped,omitempty"`
	// VarianceReduction is the CUPED sensitivity gain when Type == "cuped".
	VarianceReduction float64 `json:"varianceReduction,omitempty"`
}

// BuildReport assembles the analysis for one experiment. It fails when the
// experiment has != 2 variants (v1 fixed-sample scope) or aggregates are
// missing in ways that make statistics undefined; a "no data yet" report is
// returned with Healthy=false and verdict hold instead of an error when
// possible, so the console can render progress.
func BuildReport(store *platform.Store, experimentID string, now time.Time) (*Report, error) {
	exp, err := store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	version, err := platform.ExperimentRegistrationVersion(exp)
	registeredJourney := exp.PrimaryMetric.Type == "funnel" || exp.PrimaryMetric.Type == "retention"
	for _, g := range exp.Guardrails {
		registeredJourney = registeredJourney || g.Type == "funnel" || g.Type == "retention"
	}
	hasMetricRefs := exp.PrimaryMetricRef != nil
	for _, g := range exp.Guardrails {
		hasMetricRefs = hasMetricRefs || g.MetricRef != nil
	}
	if err != nil || ((exp.HolmFamily != nil || registeredJourney || hasMetricRefs) && version != exp.RegistrationVersion) {
		return nil, fmt.Errorf("experiment %s registration contract differs from persisted version", experimentID)
	}
	if exp.PrimaryMetricRef != nil {
		if err := store.ValidateFrozenMetricRef(exp.WorkspaceID, *exp.PrimaryMetricRef); err != nil {
			return nil, fmt.Errorf("experiment %s primary metric: %w", experimentID, err)
		}
	}
	for _, guard := range exp.Guardrails {
		if guard.MetricRef != nil {
			if err := store.ValidateFrozenMetricRef(exp.WorkspaceID, *guard.MetricRef); err != nil {
				return nil, fmt.Errorf("experiment %s guardrail metric: %w", experimentID, err)
			}
		}
	}
	if exp.AllocationMode == "bandit" {
		rep, err := BuildBanditReport(store, exp, now)
		if err == nil {
			return rep, nil
		}
		section := &BanditSection{PolicyVersion: exp.Bandit.PolicyVersion, Frozen: true,
			FreezeReason:      "feedback ledger unhealthy: " + err.Error(),
			FeedbackWatermark: now.UTC().Add(-exp.Bandit.FeedbackDelay),
			Trials:            make([]int64, len(exp.Variants)), Successes: make([]int64, len(exp.Variants)),
			PosteriorMean: make([]float64, len(exp.Variants))}
		if current := store.CurrentFlagContents(exp.WorkspaceID, exp.EnvironmentID)[exp.FlagKey]; current != nil && current.Content.Experiment != nil && current.Content.Experiment.Bandit != nil {
			state := current.Content.Experiment.Bandit
			section.StateVersion = state.StateVersion
			copy(section.Trials, state.Trials)
			copy(section.Successes, state.Successes)
		}
		return &Report{ExperimentID: exp.ID, Epoch: exp.Epoch, FlagKey: exp.FlagKey, State: exp.State,
			GeneratedAt: now.UTC(), AnalysisMethod: "bandit-beta-bernoulli", Healthy: false,
			RegistrationVersion: exp.RegistrationVersion, PrimaryMetricRef: exp.PrimaryMetricRef,
			HealthIssues: []string{section.FreezeReason}, Bandit: section,
			Decision: stats.Decision{Verdict: stats.VerdictHold}}, nil
	}
	if len(exp.Variants) != 2 && exp.HolmFamily == nil {
		return nil, fmt.Errorf("experiment %s has %d variants; v1 reports are fixed-sample two-arm (§10.3)",
			experimentID, len(exp.Variants))
	}
	control, treatment := exp.Variants[0], exp.Variants[1]
	analysis, window, err := store.AnalysisStore(exp, now)
	if err != nil {
		return nil, fmt.Errorf("experiment %s: build analysis window: %w", experimentID, err)
	}
	primaryWindow, err := frozenAttributionWindow(store, exp, exp.PrimaryMetricRef)
	if err != nil {
		return nil, err
	}
	analysisViews := map[time.Duration]*platform.Store{exp.AnalysisWindow: analysis}
	viewForWindow := func(duration time.Duration) *platform.Store {
		if view := analysisViews[duration]; view != nil {
			return view
		}
		view := analysis.ReplayAttributionWindow(exp, window, duration)
		analysisViews[duration] = view
		return view
	}
	primaryAnalysis := viewForWindow(primaryWindow)

	rep := &Report{
		ExperimentID: exp.ID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
		State: exp.State, GeneratedAt: now.UTC(),
		ExposureMode:   exp.ExposureMode,
		AnalysisMethod: exp.AnalysisMethod, Alpha: exp.Alpha, MDE: exp.MDE,
		Control: control, Treatment: treatment, Window: window,
		PrimaryAttributionWindow: primaryWindow,
		RegistrationVersion:      exp.RegistrationVersion,
		PrimaryMetricRef:         exp.PrimaryMetricRef,
	}
	if exp.ExposureMode == "onRender" {
		rep.Diagnostics = buildDiagnostics(exp, analysis.EventsForExperiment(exp.WorkspaceID,
			exp.EnvironmentID, exp.ID, exp.Epoch, ""), window, now)
		if err := attachCollectionCounts(store, exp, rep.Diagnostics); err != nil {
			return nil, err
		}
		rep.HealthIssues = append(rep.HealthIssues, rep.Diagnostics.Issues...)
	}
	for _, guard := range exp.Guardrails {
		if guard.MetricRef != nil {
			rep.GuardrailMetricRefs = append(rep.GuardrailMetricRefs, *guard.MetricRef)
		}
	}
	if window.Backend == "postgresql-raw" {
		rep.HealthIssues = append(rep.HealthIssues, window.Issues...)
	}
	journeyPrimary := exp.PrimaryMetric.Type == "funnel" || exp.PrimaryMetric.Type == "retention"
	journeyGuard := false
	for _, g := range exp.Guardrails {
		journeyGuard = journeyGuard || g.Type == "funnel" || g.Type == "retention"
	}
	var journeyEvents []*platform.Event
	if journeyPrimary || journeyGuard {
		journeyEvents, err = store.AnalysisEventSnapshot(exp, now)
		if err != nil {
			return nil, fmt.Errorf("experiment %s: journey raw events: %w", experimentID, err)
		}
		if !window.Recomputable {
			rep.HealthIssues = append(rep.HealthIssues, "journey decision requires recomputable PostgreSQL raw events")
		}
	}

	// ---- volumes ----
	allExposed := analysis.ExperimentAggregate(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, nil)
	splitSets := analysis.SplitExposedUnitSet(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)

	byVariant := map[string]*VariantReport{}
	order := exp.Variants
	for _, v := range order {
		vr := &VariantReport{Variant: v}
		if agg, ok := allExposed[v]; ok {
			vr.ExposedUnits = agg.ExposedUnits
			vr.Exposures = agg.Exposures
		}
		vr.RandomizedUnits = int64(len(splitSets[v]))
		byVariant[v] = vr
	}

	// ---- primary metric on the randomized population ----
	// Key is the primary metric: the numerator for ratio, the percentile'd
	// metric for quantile, the outcome for a CUPED-adjusted mean. Ratio and
	// CUPED fetch their second metric (denominator / covariate) as well.
	wsID, envID := exp.WorkspaceID, exp.EnvironmentID
	primaryAgg := primaryAnalysis.MetricAggregate(wsID, envID, exp.ID, exp.Epoch, exp.PrimaryMetric.Key)

	var (
		ctrlProp, treatProp   stats.ProportionStats
		ctrlMean, treatMean   stats.MeanStats
		ctrlRatio, treatRatio stats.RatioStats
		ctrlVals, treatVals   []float64
		convCtrl, convTreat   int64
	)
	switch exp.PrimaryMetric.Type {
	case "funnel", "retention":
		section := buildJourneyWithAttribution(exp, exp.PrimaryMetric.Type, exp.PrimaryMetric.Key, exp.PrimaryMetric.Funnel,
			exp.PrimaryMetric.Retention, journeyEvents, window, primaryWindow)
		rep.Journeys = append(rep.Journeys, section)
		ctrlProp, treatProp = journeyStats(section, control, treatment)
		convCtrl, convTreat = ctrlProp.Conversions, treatProp.Conversions
		for _, arm := range section.Arms {
			if arm.Excluded["attribution_window_short"] > 0 {
				rep.HealthIssues = append(rep.HealthIssues,
					fmt.Sprintf("%s attribution window cannot cover retention day for %s", section.Metric, arm.Variant))
			}
			if arm.Immature > 0 {
				rep.HealthIssues = append(rep.HealthIssues, fmt.Sprintf("%s %s has %d immature cohorts", section.Metric, arm.Variant, arm.Immature))
			}
		}
	case "proportion", "mean":
		cs, ts, cc, ct := buildMetricStats(exp.PrimaryMetric.Type, primaryAgg, splitSets, control, treatment)
		convCtrl, convTreat = cc, ct
		if exp.PrimaryMetric.Type == "proportion" {
			ctrlProp, _ = cs.(stats.ProportionStats)
			treatProp, _ = ts.(stats.ProportionStats)
		} else {
			ctrlMean, _ = cs.(stats.MeanStats)
			treatMean, _ = ts.(stats.MeanStats)
		}
	case "ratio":
		numKey := exp.PrimaryMetric.NumeratorKey
		if numKey == "" {
			numKey = exp.PrimaryMetric.Key
		}
		numAgg := primaryAgg
		if numKey != exp.PrimaryMetric.Key {
			numAgg = primaryAnalysis.MetricAggregate(wsID, envID, exp.ID, exp.Epoch, numKey)
		}
		denAgg := primaryAnalysis.MetricAggregate(wsID, envID, exp.ID, exp.Epoch, exp.PrimaryMetric.DenominatorKey)
		ctrlRatio, treatRatio = buildRatioStats(numAgg, denAgg, splitSets, control, treatment)
		convCtrl, convTreat = ctrlRatio.N, treatRatio.N
	case "quantile":
		ctrlVals, treatVals = buildQuantileValues(primaryAgg, splitSets, control, treatment)
		convCtrl, convTreat = int64(len(ctrlVals)), int64(len(treatVals))
	}

	byVariant[control].ConvertedUnits = convCtrl
	byVariant[treatment].ConvertedUnits = convTreat
	for _, variant := range exp.Variants[2:] {
		switch exp.PrimaryMetric.Type {
		case "proportion":
			_, value, _, count := buildMetricStats("proportion", primaryAgg, splitSets, control, variant)
			byVariant[variant].ConvertedUnits = count
			if n := byVariant[variant].RandomizedUnits; n > 0 {
				byVariant[variant].PrimaryValue = float64(value.(stats.ProportionStats).Conversions) / float64(n)
			}
		case "mean":
			_, value, _, count := buildMetricStats("mean", primaryAgg, splitSets, control, variant)
			byVariant[variant].ConvertedUnits = count
			if count > 0 {
				byVariant[variant].PrimaryValue = value.(stats.MeanStats).Mean()
			}
		case "ratio":
			_, count := buildRatioStats(primaryAgg, primaryAnalysis.MetricAggregate(wsID, envID, exp.ID, exp.Epoch, exp.PrimaryMetric.DenominatorKey), splitSets, control, variant)
			byVariant[variant].ConvertedUnits = count.N
		case "quantile":
			_, vals := buildQuantileValues(primaryAgg, splitSets, control, variant)
			byVariant[variant].ConvertedUnits = int64(len(vals))
		}
	}
	// PrimaryValue renders the per-arm point estimate for the console table.
	setPrimaryValue := func(vr *VariantReport, prop stats.ProportionStats, mean stats.MeanStats,
		ratio stats.RatioStats, vals []float64) {
		switch exp.PrimaryMetric.Type {
		case "proportion", "funnel", "retention":
			if prop.N > 0 {
				vr.PrimaryValue = float64(prop.Conversions) / float64(prop.N)
			}
		case "mean":
			if mean.N > 0 {
				vr.PrimaryValue = mean.Sum / float64(mean.N)
			}
		case "ratio":
			if ratio.SumX != 0 {
				vr.PrimaryValue = ratio.SumY / ratio.SumX
			}
		case "quantile":
			if len(vals) > 0 {
				s := append([]float64(nil), vals...)
				sort.Float64s(s)
				vr.PrimaryValue = stats.SampleQuantile(s, exp.PrimaryMetric.QuantileLevel)
			}
		}
	}
	setPrimaryValue(byVariant[control], ctrlProp, ctrlMean, ctrlRatio, ctrlVals)
	setPrimaryValue(byVariant[treatment], treatProp, treatMean, treatRatio, treatVals)
	for _, v := range order {
		rep.PerVariant = append(rep.PerVariant, *byVariant[v])
	}

	// ---- SRM: randomized units vs pre-registered proportions ----
	// Experiment proportions are percentages (sum 100); SRMTest expects
	// fractions (sum 1).
	observed := make([]int64, 0, len(order))
	expected := make([]float64, 0, len(order))
	var totalObserved int64
	for _, variant := range order {
		n := int64(len(splitSets[variant]))
		observed = append(observed, n)
		expected = append(expected, exp.Proportions[variant]/100)
		totalObserved += n
	}
	srm, srmErr := stats.SRMTest(observed, expected, stats.DefaultSRMThreshold)
	if srmErr != nil {
		if totalObserved == 0 {
			rep.Healthy = false
			rep.HealthIssues = append(rep.HealthIssues, "no randomized exposures recorded yet")
		} else {
			rep.HealthIssues = append(rep.HealthIssues, "SRM computation failed: "+srmErr.Error())
		}
	} else {
		rep.SRM = &srm
		if !srm.Healthy {
			pipeline := "assignment pipeline"
			if exp.ExposureMode == "onRender" {
				pipeline = "delivery pipeline"
			}
			rep.HealthIssues = append(rep.HealthIssues, fmt.Sprintf(
				"SRM alarm: observed %v vs expected %v (p=%.2g) — %s suspect",
				observed, expected, srm.PValue, pipeline))
		}
	}

	// ---- minimum runtime ----
	if started, ok := store.ExperimentStartedAt(exp.ID); ok {
		rep.MinRuntimeSatisfied = now.Sub(started) >= exp.MinRuntime
	} else {
		rep.MinRuntimeSatisfied = false
		if exp.State == model.ExperimentRunning {
			rep.HealthIssues = append(rep.HealthIssues, "experiment running without recorded start time")
		}
	}

	// ---- primary test (+ separate sequential/bayesian views) ----
	// The frequentist fixed-sample Primary below is what drives Decide. The
	// Sequential (always-valid) and Bayesian views are computed into their OWN
	// report fields and never enter the gate (docs/技术方案.md §10.3).
	var pValue, diff, ciLow, ciHigh float64
	var significant bool
	switch exp.PrimaryMetric.Type {
	case "proportion", "funnel", "retention":
		res, terr := stats.TwoProportionZTest(ctrlProp, treatProp, exp.Alpha)
		if terr != nil {
			rep.PrimaryError = terr.Error()
		} else {
			pr := res
			rep.Primary = &PrimaryResult{
				Type: "two-proportion-z", PValue: pr.PValue, Diff: pr.Diff,
				RelativeDiff: pr.RelativeDiff, RelativeMeaningful: pr.RelativeMeaningful,
				CILow: pr.CIDiffLow, CIHigh: pr.CIDiffHigh, Significant: pr.Significant,
				Proportion: &pr,
			}
			pValue, diff, ciLow, ciHigh = pr.PValue, pr.Diff, pr.CIDiffLow, pr.CIDiffHigh
			significant = pr.Significant
		}
		if bres, berr := stats.BayesianProportionTest(ctrlProp, treatProp, exp.Alpha, stats.BetaPrior{}); berr == nil {
			bp := bres
			rep.Bayesian = &BayesianSection{Proportion: &bp}
		}

	case "mean":
		// CUPED when a pre-experiment covariate is pre-registered and paired
		// data exists; otherwise (or on insufficient pairs) fall back to Welch.
		cupedDone := false
		if exp.PrimaryMetric.CovariateKey != "" {
			covAgg := primaryAnalysis.MetricAggregate(wsID, envID, exp.ID, exp.Epoch, exp.PrimaryMetric.CovariateKey)
			cp, tp := buildPairedStats(covAgg, primaryAgg, splitSets, control, treatment)
			if cp.N >= 2 && tp.N >= 2 {
				if cres, cerr := stats.CUPEDTest(cp, tp, exp.Alpha); cerr == nil {
					cr := cres
					rep.Primary = &PrimaryResult{
						Type: "cuped", PValue: cr.PValue, Diff: cr.Diff,
						RelativeDiff: cr.RelativeDiff, RelativeMeaningful: cr.RelativeMeaningful,
						CILow: cr.CIDiffLow, CIHigh: cr.CIDiffHigh, Significant: cr.Significant,
						CUPED: &cr, VarianceReduction: cr.VarianceReduction,
					}
					pValue, diff, ciLow, ciHigh = cr.PValue, cr.Diff, cr.CIDiffLow, cr.CIDiffHigh
					significant = cr.Significant
					cupedDone = true
				}
			}
		}
		if !cupedDone {
			res, terr := stats.WelchTTest(ctrlMean, treatMean, exp.Alpha)
			if terr != nil {
				rep.PrimaryError = terr.Error()
			} else {
				wr := res
				rep.Primary = &PrimaryResult{
					Type: "welch", PValue: wr.PValue, Diff: wr.Diff,
					RelativeDiff: wr.RelativeDiff, RelativeMeaningful: wr.RelativeMeaningful,
					CILow: wr.CIDiffLow, CIHigh: wr.CIDiffHigh, Significant: wr.Significant,
					Welch: &wr,
				}
				pValue, diff, ciLow, ciHigh = wr.PValue, wr.Diff, wr.CIDiffLow, wr.CIDiffHigh
				significant = wr.Significant
			}
		}
		// Sequential (always-valid) view — the mixing scale tau is the
		// pre-registered MDE (the effect scale worth detecting); fall back to a
		// fraction of the pooled SD when MDE is unset.
		tau := exp.MDE
		if tau <= 0 {
			if pooled := ctrlMean.Merge(treatMean); pooled.Variance() > 0 {
				tau = 0.2 * math.Sqrt(pooled.Variance())
			}
		}
		if tau > 0 {
			if sres, serr := stats.SequentialMeanTest(ctrlMean, treatMean, exp.Alpha, tau); serr == nil {
				sr := sres
				rep.Sequential = &sr
			}
		}
		// Bayesian view (mean family).
		if bres, berr := stats.BayesianMeanTest(ctrlMean, treatMean, exp.Alpha, stats.NormalPrior{}); berr == nil {
			bm := bres
			rep.Bayesian = &BayesianSection{Mean: &bm}
		}

	case "ratio":
		res, terr := stats.RatioDeltaTest(ctrlRatio, treatRatio, exp.Alpha)
		if terr != nil {
			rep.PrimaryError = terr.Error()
		} else {
			rr := res
			rep.Primary = &PrimaryResult{
				Type: "ratio-delta", PValue: rr.PValue, Diff: rr.Diff,
				RelativeDiff: rr.RelativeDiff, RelativeMeaningful: rr.RelativeMeaningful,
				CILow: rr.CIDiffLow, CIHigh: rr.CIDiffHigh, Significant: rr.Significant,
				Ratio: &rr,
			}
			pValue, diff, ciLow, ciHigh = rr.PValue, rr.Diff, rr.CIDiffLow, rr.CIDiffHigh
			significant = rr.Significant
		}

	case "quantile":
		res, terr := stats.QuantileTest(ctrlVals, treatVals, exp.PrimaryMetric.QuantileLevel, exp.Alpha)
		if terr != nil {
			rep.PrimaryError = terr.Error()
		} else {
			qr := res
			rel, relMeaningful := 0.0, false
			if math.Abs(qr.QuantileA) > 1e-12 {
				rel, relMeaningful = qr.Diff/qr.QuantileA, true
			}
			rep.Primary = &PrimaryResult{
				Type: "quantile", PValue: qr.PValue, Diff: qr.Diff,
				RelativeDiff: rel, RelativeMeaningful: relMeaningful,
				CILow: qr.CIDiffLow, CIHigh: qr.CIDiffHigh, Significant: qr.Significant,
				Quantile: &qr,
			}
			pValue, diff, ciLow, ciHigh = qr.PValue, qr.Diff, qr.CIDiffLow, qr.CIDiffHigh
			significant = qr.Significant
		}

	default:
		return nil, fmt.Errorf("experiment %s: unsupported primary metric type %q", experimentID, exp.PrimaryMetric.Type)
	}

	// ---- guardrails (operational rules, separate from significance) ----
	for _, g := range exp.Guardrails {
		guardWindow, err := frozenAttributionWindow(store, exp, g.MetricRef)
		if err != nil {
			return nil, err
		}
		guardAnalysis := viewForWindow(guardWindow)
		var journey *JourneySection
		if g.Type == "funnel" || g.Type == "retention" {
			section := buildJourneyWithAttribution(exp, g.Type, g.MetricKey, g.Funnel, g.Retention,
				journeyEvents, window, guardWindow)
			journey = &section
			rep.Journeys = append(rep.Journeys, section)
			for _, arm := range section.Arms {
				if arm.Excluded["attribution_window_short"] > 0 {
					rep.HealthIssues = append(rep.HealthIssues,
						fmt.Sprintf("guardrail %s attribution window cannot cover retention day for %s", section.Metric, arm.Variant))
				}
				if arm.Immature > 0 {
					rep.HealthIssues = append(rep.HealthIssues, fmt.Sprintf("guardrail %s %s has %d immature cohorts", section.Metric, arm.Variant, arm.Immature))
				}
			}
		}
		for _, variant := range exp.Variants[1:] {
			var check stats.GuardrailCheck
			if journey != nil {
				check = journeyGuardrail(*journey, g, control, variant)
				if int64(len(splitSets[control])) < g.MinSample || int64(len(splitSets[variant])) < g.MinSample {
					rep.HealthIssues = append(rep.HealthIssues, "journey guardrail randomized units below min sample: "+g.MetricKey)
				}
				if strings.HasPrefix(check.Detail, "journey denominator") || strings.HasPrefix(check.Detail, "journey baseline") {
					rep.HealthIssues = append(rep.HealthIssues, "journey guardrail unavailable: "+check.Detail)
				}
			} else {
				check = evaluateGuardrail(guardAnalysis, exp, g, control, variant, splitSets)
			}
			if len(exp.Variants) > 2 {
				check.Metric += ":" + variant
			}
			rep.Guardrails = append(rep.Guardrails, check)
		}
	}
	if exp.HolmFamily != nil {
		rep.Holm = buildHolmSection(analysis, exp, splitSets, rep.Primary)
		if rep.Holm.Error != "" {
			rep.HealthIssues = append(rep.HealthIssues, "Holm family incomplete: "+rep.Holm.Error)
		}
	}

	// ---- health & decision ----
	dataHealthy := len(rep.HealthIssues) == 0 && rep.PrimaryError == "" &&
		totalObserved > 0
	rep.Healthy = dataHealthy && (rep.SRM == nil || rep.SRM.Healthy)

	in := stats.DecisionInput{
		PrimaryPValue: pValue, PrimarySignificant: significant,
		PrimaryDiff: adjustDirection(diff, exp.PrimaryMetric.Direction),
		PrimaryCI:   [2]float64{ciLow, ciHigh},
		Alpha:       exp.Alpha, MDE: exp.MDE,
		MinSamplePerArm: exp.MinSamplePerArm,
		NA:              observed[0], NB: observed[1],
		Guardrails:          rep.Guardrails,
		MinRuntimeSatisfied: rep.MinRuntimeSatisfied,
		DataHealthy:         dataHealthy,
	}
	if journeyPrimary {
		in.NA = min(in.NA, ctrlProp.N)
		in.NB = min(in.NB, treatProp.N)
	}
	if exp.HolmFamily != nil {
		for _, count := range observed[2:] {
			in.NB = min(in.NB, count)
		}
	}
	if rep.Holm != nil && rep.Holm.Error == "" {
		// A confirmatory ship requires every pre-registered comparison to
		// reject in its favorable direction; negative discoveries roll back.
		allImproved := true
		for _, comparison := range rep.Holm.Comparisons {
			allImproved = allImproved && comparison.Rejected && comparison.DirectedDiff > 0
		}
		in.PrimaryPValue = rep.Holm.Comparisons[0].AdjustedP
		if allImproved && in.PrimaryPValue == exp.Alpha {
			// Decide's legacy single-test rule uses strict <. Holm uses <=
			// at its step threshold; preserve that exact boundary here.
			in.PrimaryPValue = math.Nextafter(exp.Alpha, 0)
		}
		in.PrimarySignificant = allImproved
	}
	if rep.SRM != nil {
		in.SRM = *rep.SRM
	} else {
		in.SRM = stats.SRMResult{Healthy: false} // unknown SRM never passes the gate
	}
	rep.Decision = stats.Decide(in)
	if rep.Holm != nil && rep.Holm.Error == "" && rep.Decision.Verdict == stats.VerdictContinue {
		for _, comparison := range rep.Holm.Comparisons {
			if comparison.Rejected && comparison.DirectedDiff < 0 {
				rep.Decision = stats.Decision{Verdict: stats.VerdictRollback,
					Reasons: []string{"Holm-adjusted confirmatory comparison regressed: " + comparison.ID}}
				break
			}
		}
	}
	return rep, nil
}

func frozenAttributionWindow(store *platform.Store, exp *platform.Experiment,
	ref *platform.MetricRef) (time.Duration, error) {
	if ref == nil {
		return exp.AnalysisWindow, nil
	}
	def, err := store.GetMetricDefinition(exp.WorkspaceID, *ref)
	if err != nil {
		return 0, fmt.Errorf("experiment %s metric %s: %w", exp.ID, ref.Key, err)
	}
	return def.AttributionWindow, nil
}

// adjustDirection flips the sign for "lower is better" metrics so the
// decision gate's diff>0 == improvement invariant holds.
func adjustDirection(diff float64, direction string) float64 {
	if direction == "decrease" {
		return -diff
	}
	return diff
}

// buildMetricStats converts per-unit aggregates restricted to the randomized
// population into the sufficient statistics of the requested metric type.
// Mean metrics aggregate PER UNIT first (mean of each unit's events), so a
// chatty user cannot dominate the test (§10.1: 请求级成本/延迟需先按随机化
// 单元聚合).
func buildMetricStats(metricType string, agg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) (any, any, int64, int64) {

	build := func(variant string) (any, int64) {
		set := splitSets[variant]
		ma := agg[variant]
		if metricType == "proportion" {
			converted := int64(0)
			if ma != nil {
				for unit := range ma.Units {
					if set[unit] {
						converted++
					}
				}
			}
			return stats.ProportionStats{N: int64(len(set)), Conversions: converted}, converted
		}
		ms := stats.MeanStats{}
		if ma != nil {
			for unit, ua := range ma.Units {
				if !set[unit] || ua.Count == 0 {
					continue
				}
				unitMean := ua.Sum / float64(ua.Count)
				ms.N++
				ms.Sum += unitMean
				ms.SumSquares += unitMean * unitMean
			}
		}
		return ms, ms.N
	}

	ctrlStats, convCtrl := build(control)
	treatStats, convTreat := build(treatment)
	return ctrlStats, treatStats, convCtrl, convTreat
}

// buildRatioStats assembles per-arm RatioStats for a ratio metric from the
// numerator and denominator per-unit aggregates, restricted to the randomized
// population (docs/技术方案.md §10.1: ratio 需每单元分子/分母及交叉乘积). Each
// split-exposed unit contributes its (denominator total, numerator total) pair;
// a unit present in only one of the two metrics contributes 0 for the missing
// side (totals over the randomized population).
func buildRatioStats(numAgg, denAgg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) (stats.RatioStats, stats.RatioStats) {

	build := func(variant string) stats.RatioStats {
		set := splitSets[variant]
		num, den := numAgg[variant], denAgg[variant]
		var rs stats.RatioStats
		for unit := range set {
			var x, y float64
			hasNum, hasDen := false, false
			if num != nil {
				if ua, ok := num.Units[unit]; ok && ua.Count > 0 {
					y, hasNum = ua.Sum, true
				}
			}
			if den != nil {
				if ua, ok := den.Units[unit]; ok && ua.Count > 0 {
					x, hasDen = ua.Sum, true
				}
			}
			if !hasNum && !hasDen {
				continue
			}
			rs.AddUnit(x, y)
		}
		return rs
	}
	return build(control), build(treatment)
}

// buildQuantileValues collects the per-unit values (each unit's mean of its
// events) for a quantile metric, restricted to the randomized population
// (docs/技术方案.md §10.1: 分位数推断需要保留单元明细). Returned slices are
// sorted ascending.
func buildQuantileValues(agg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) ([]float64, []float64) {

	build := func(variant string) []float64 {
		set := splitSets[variant]
		ma := agg[variant]
		var vals []float64
		if ma != nil {
			for unit, ua := range ma.Units {
				if !set[unit] || ua.Count == 0 {
					continue
				}
				vals = append(vals, ua.Sum/float64(ua.Count))
			}
		}
		sort.Float64s(vals)
		return vals
	}
	return build(control), build(treatment)
}

// buildPairedStats assembles per-arm PairedStats (pre-experiment covariate,
// in-experiment outcome) for CUPED, restricted to units that have BOTH a
// covariate and an outcome in the randomized population (docs/技术方案.md
// §10.1: CUPED 需实验前协变量和实验后结果的同单元配对数据). Each pair uses the
// unit means of the covariate and outcome metrics.
func buildPairedStats(covAgg, outAgg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) (stats.PairedStats, stats.PairedStats) {

	build := func(variant string) stats.PairedStats {
		set := splitSets[variant]
		cov, out := covAgg[variant], outAgg[variant]
		var ps stats.PairedStats
		if cov == nil || out == nil {
			return ps
		}
		for unit, cua := range cov.Units {
			if !set[unit] || cua.Count == 0 {
				continue
			}
			oua, ok := out.Units[unit]
			if !ok || oua.Count == 0 {
				continue
			}
			ps.AddPair(cua.Sum/float64(cua.Count), oua.Sum/float64(oua.Count))
		}
		return ps
	}
	return build(control), build(treatment)
}

// evaluateGuardrail checks one guardrail metric. A breach requires the
// minimum sample on both arms; below it the guardrail is reported as not
// breached with an explanatory detail (insufficient data must not fake
// safety OR panic — the decision gate holds separately).
func evaluateGuardrail(store *platform.Store, exp *platform.Experiment, g platform.GuardrailSpec,
	control, treatment string, splitSets map[string]map[string]bool) stats.GuardrailCheck {

	check := stats.GuardrailCheck{Metric: g.MetricKey}
	agg := store.MetricAggregate(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, g.MetricKey)

	ctrlV, treatV, ok := guardrailValues(g.Type, agg, splitSets, control, treatment)
	if !ok {
		check.Detail = "insufficient data for guardrail"
		return check
	}
	nCtrl := int64(len(splitSets[control]))
	nTreat := int64(len(splitSets[treatment]))
	if nCtrl < g.MinSample || nTreat < g.MinSample {
		check.Detail = fmt.Sprintf("below guardrail min sample (%d/%d < %d)", nCtrl, nTreat, g.MinSample)
		return check
	}
	if math.Abs(ctrlV) < 1e-12 {
		// Near-zero baseline: relative regression is undefined; only an
		// absolute move could breach and v1 has no absolute threshold —
		// report without breaching (§10.3: 基线接近零时只报绝对差).
		check.Detail = fmt.Sprintf("baseline ~0 (treatment %.4g); relative guardrail undefined", treatV)
		return check
	}
	relChange := (treatV - ctrlV) / math.Abs(ctrlV)
	// Regression = movement against the guardrail metric's good direction.
	// Direction defaults to "increase" (higher is better); cost/latency
	// guardrails must declare "decrease".
	regression := -relChange
	if g.Direction == "decrease" {
		regression = relChange
	}
	if regression > g.MaxRelativeRegression {
		check.Breached = true
		check.Detail = fmt.Sprintf("relative regression %.2f%% exceeds %.2f%% (control %.4g → treatment %.4g)",
			regression*100, g.MaxRelativeRegression*100, ctrlV, treatV)
	} else {
		check.Detail = fmt.Sprintf("relative change %+.2f%% within %.2f%% threshold",
			relChange*100, g.MaxRelativeRegression*100)
	}
	return check
}

// guardrailValues computes per-arm scalar values (rate or mean of per-unit
// means) restricted to the randomized population.
func guardrailValues(metricType string, agg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) (float64, float64, bool) {

	value := func(variant string) (float64, bool) {
		set := splitSets[variant]
		if len(set) == 0 {
			return 0, false
		}
		ma := agg[variant]
		if metricType == "proportion" {
			conv := int64(0)
			if ma != nil {
				for unit := range ma.Units {
					if set[unit] {
						conv++
					}
				}
			}
			return float64(conv) / float64(len(set)), true
		}
		var n int64
		var sum float64
		if ma != nil {
			for unit, ua := range ma.Units {
				if !set[unit] || ua.Count == 0 {
					continue
				}
				n++
				sum += ua.Sum / float64(ua.Count)
			}
		}
		if n == 0 {
			return 0, false
		}
		return sum / float64(n), true
	}

	c, okC := value(control)
	t, okT := value(treatment)
	return c, t, okC && okT
}
