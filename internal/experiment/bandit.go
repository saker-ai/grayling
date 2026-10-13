package experiment

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/stats"
	"github.com/saker-ai/grayling/pkg/model"
)

// BanditSection is an adaptive-allocation report. It deliberately has no
// fixed-sample p-value, CI, or ship verdict.
type BanditSection struct {
	PolicyVersion       string    `json:"policyVersion"`
	StateVersion        int64     `json:"stateVersion"`
	FeedbackWatermark   time.Time `json:"feedbackWatermark"`
	Trials              []int64   `json:"trials"`
	Successes           []int64   `json:"successes"`
	PosteriorMean       []float64 `json:"posteriorMean"`
	Pending             int64     `json:"pending"`
	MissingFeedback     int64     `json:"missingFeedback"`
	FallbackAssignments int64     `json:"fallbackAssignments"`
	SRMAlarm            bool      `json:"srmAlarm"`
	Frozen              bool      `json:"frozen"`
	FreezeReason        string    `json:"freezeReason,omitempty"`
}

type banditUnit struct {
	arm         int
	at          time.Time
	probability float64
	fallback    bool
	converted   bool
}

// BuildBanditReport replays accepted raw events. A unit's first randomized
// exposure owns attribution; a later arm change is a data-health error.
func BuildBanditReport(store *platform.Store, exp *platform.Experiment, now time.Time) (*Report, error) {
	if exp.Bandit == nil || len(exp.Variants) != 2 {
		return nil, fmt.Errorf("bandit: expected registered two-arm policy")
	}
	events, err := store.BanditEvents(exp, now)
	if err != nil {
		return nil, err
	}
	return replayBanditReport(exp, events, now), nil
}

func replayBanditReport(exp *platform.Experiment, events []*platform.Event, now time.Time) *Report {
	watermark := now.UTC().Add(-exp.Bandit.FeedbackDelay)
	windowStart := exp.CreatedAt
	if exp.StartedAt != nil {
		windowStart = *exp.StartedAt
	}
	rep := &Report{ExperimentID: exp.ID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
		State: exp.State, GeneratedAt: now.UTC(), AnalysisMethod: "bandit-beta-bernoulli",
		RegistrationVersion: exp.RegistrationVersion, PrimaryMetricRef: exp.PrimaryMetricRef,
		Control: exp.Variants[0], Treatment: exp.Variants[1],
		Window: platform.AnalysisWindowStatus{Version: "bandit-feedback-v1", Start: windowStart,
			End: now.UTC(), Watermark: watermark, Backend: "raw-events", Recomputable: true, Complete: true}}
	section := &BanditSection{PolicyVersion: exp.Bandit.PolicyVersion, FeedbackWatermark: watermark,
		Trials: make([]int64, 2), Successes: make([]int64, 2), PosteriorMean: make([]float64, 2)}
	rep.Bandit = section
	issueSet := make(map[string]bool)
	addIssue := func(issue string) {
		if !issueSet[issue] {
			issueSet[issue] = true
			rep.HealthIssues = append(rep.HealthIssues, issue)
		}
	}
	rep.ExposureMode = exp.ExposureMode
	rep.Diagnostics = buildDiagnostics(exp, events, rep.Window, now)
	if exp.ExposureMode == "onRender" {
		for _, issue := range rep.Diagnostics.Issues {
			addIssue(issue)
		}
	}
	units := make(map[string]*banditUnit)
	seenVariant := [2]map[string]bool{{}, {}}
	matureVariant := [2]map[string]bool{{}, {}}
	var expected, variance, observed float64
	exposures := make([]*platform.Event, 0)
	for _, ev := range events {
		if ev.Type == platform.EventExposure {
			exposures = append(exposures, ev)
		}
	}
	slices.SortFunc(exposures, func(a, b *platform.Event) int {
		if c := a.ClientTime.Compare(b.ClientTime); c != 0 {
			return c
		}
		return strings.Compare(a.EventID, b.EventID)
	})
	for _, ev := range exposures {
		if ev.ClientTime.After(now) || (exp.StartedAt != nil && ev.ClientTime.Before(*exp.StartedAt)) {
			continue
		}
		arm := -1
		for i, v := range exp.Variants {
			if ev.Variant == v {
				arm = i
				break
			}
		}
		if arm < 0 {
			continue
		}
		if ev.Source != "bandit" && ev.Source != "split" && ev.Source != "bandit-fallback" {
			continue
		}
		if prior := units[ev.UnitHash]; prior != nil {
			if prior.arm != arm {
				addIssue("unit exposed to multiple bandit arms")
			}
			continue
		}
		prob := ev.AllocationProbability
		fallback := ev.Source != "bandit"
		if fallback && prob == 0 {
			prob = exp.Proportions[ev.Variant] / 100
		}
		if ev.PolicyVersion != "" && ev.PolicyVersion != exp.Bandit.PolicyVersion {
			addIssue("exposure policy version mismatch")
		}
		if prob <= 0 || prob > 1 || math.IsNaN(prob) {
			addIssue("exposure missing valid assignment probability")
			continue
		}
		units[ev.UnitHash] = &banditUnit{arm: arm, at: ev.ClientTime, probability: prob, fallback: fallback}
		seenVariant[arm][ev.UnitHash] = true
		if fallback {
			section.FallbackAssignments++
		}
		pTreatment := prob
		if arm == 0 {
			pTreatment = 1 - prob
		}
		expected += pTreatment
		variance += pTreatment * (1 - pTreatment)
		if arm == 1 {
			observed++
		}
	}
	for _, ev := range events {
		if ev.Type != platform.EventConversion || ev.MetricKey != exp.PrimaryMetric.Key || ev.Value <= 0 {
			continue
		}
		u := units[ev.UnitHash]
		if u == nil || ev.Variant != exp.Variants[u.arm] || ev.ClientTime.Before(u.at) ||
			!ev.ClientTime.Before(u.at.Add(exp.AnalysisWindow)) {
			continue
		}
		u.converted = true
	}
	for unit, u := range units {
		if u.at.After(watermark) {
			section.Pending++
			continue
		}
		matureVariant[u.arm][unit] = true
		section.Trials[u.arm]++
		if u.converted {
			section.Successes[u.arm]++
		} else {
			section.MissingFeedback++
		}
	}
	if variance > 0 && math.Abs(observed-expected) > 4*math.Sqrt(variance) {
		section.SRMAlarm = true
		addIssue("adaptive allocation SRM alarm")
	}
	if len(units) == 0 {
		addIssue("no randomized exposures recorded yet")
	}
	for i, v := range exp.Variants {
		section.PosteriorMean[i] = (exp.Bandit.PriorAlpha + float64(section.Successes[i])) /
			(exp.Bandit.PriorAlpha + exp.Bandit.PriorBeta + float64(section.Trials[i]))
		rep.PerVariant = append(rep.PerVariant, VariantReport{Variant: v, ExposedUnits: int64(len(seenVariant[i])),
			RandomizedUnits: int64(len(seenVariant[i])), Exposures: int64(len(seenVariant[i])),
			ConvertedUnits: section.Successes[i], PrimaryValue: section.PosteriorMean[i]})
	}
	// Guardrails use attributed raw events restricted to the bandit cohort.
	view := platform.BanditAnalysisView(exp, events, now)
	sets := map[string]map[string]bool{exp.Variants[0]: matureVariant[0], exp.Variants[1]: matureVariant[1]}
	for _, g := range exp.Guardrails {
		check := evaluateGuardrail(view, exp, g, exp.Variants[0], exp.Variants[1], sets)
		rep.Guardrails = append(rep.Guardrails, check)
		if check.Breached {
			addIssue("guardrail breach: " + g.MetricKey)
		} else if strings.Contains(check.Detail, "insufficient") || strings.Contains(check.Detail, "below guardrail min") {
			addIssue("guardrail feedback incomplete: " + g.MetricKey)
		}
	}
	if exp.StartedAt == nil {
		addIssue("missing experiment start time")
	}
	rep.Healthy = len(rep.HealthIssues) == 0
	section.Frozen = !rep.Healthy || section.Trials[0]+section.Trials[1] == 0
	if section.Frozen {
		section.FreezeReason = "waiting for mature feedback"
		if len(rep.HealthIssues) > 0 {
			section.FreezeReason = rep.HealthIssues[0]
		}
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s/%d/%d/%d/%d/%t/%s", section.PolicyVersion,
		section.Trials[0], section.Trials[1], section.Successes[0], section.Successes[1],
		section.Frozen, section.FreezeReason)
	// Stay inside IEEE-754's exact integer range for browser/TypeScript telemetry.
	section.StateVersion = int64(h.Sum64() & ((1 << 53) - 1))
	if section.StateVersion == 0 {
		section.StateVersion = 1
	}
	rep.Decision = stats.Decision{Verdict: stats.VerdictHold, Reasons: []string{"adaptive policy: no fixed-sample winner or automatic ship verdict"}}
	return rep
}

// PublishedBanditState is the immutable state to put in the next flag revision.
func PublishedBanditState(exp *platform.Experiment, section *BanditSection) *model.BanditBinding {
	return &model.BanditBinding{PolicyVersion: exp.Bandit.PolicyVersion, Strategy: exp.Bandit.Strategy,
		Variants: append([]string(nil), exp.Variants...), PriorAlpha: exp.Bandit.PriorAlpha,
		PriorBeta: exp.Bandit.PriorBeta, Epsilon: exp.Bandit.Epsilon,
		StateVersion: section.StateVersion, Watermark: section.FeedbackWatermark,
		Trials: append([]int64(nil), section.Trials...), Successes: append([]int64(nil), section.Successes...),
		Frozen: section.Frozen, FreezeReason: section.FreezeReason}
}
