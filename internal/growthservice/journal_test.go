package growthservice

import (
	"github.com/saker-ai/grayling/internal/growth"
	"testing"
)

func TestSimilarHypothesesPreserveFailuresAndVersions(t *testing.T) {
	a := "Clearer onboarding task guidance improves first completion"
	b := "Clearer onboarding task guidance improves first task completion"
	if !similarHypothesis(a, b) || similarHypothesis(a, "Clearer onboarding task guidance does not improve first completion") || similarHypothesis("upgrade conversion", "retention conversion") {
		t.Fatal("unsafe lexical grouping")
	}
	records := []growth.LearningRecord{{OriginalHypothesis: a, ProposalID: "failure"}, {OriginalHypothesis: b, ProposalID: "followup"}, {OriginalHypothesis: b, ProposalID: "new-version"}}
	records[2].LinkedExperiment.PrimaryMetric.Version = 2
	groups := groupLearning(records)
	if len(groups) != 2 {
		t.Fatal("similarity or version boundary")
	}
	count := 0
	for _, group := range groups {
		count += len(group.Records)
	}
	if count != 3 {
		t.Fatal("failure removed")
	}
	records[1].EnvironmentID = "other"
	if len(groupLearning(records)) != 3 {
		t.Fatal("cross-environment merge")
	}
}
