package growthservice

import (
	"github.com/saker-ai/grayling/internal/growth"
	"sort"
	"strings"
)

// Lexical grouping keeps every observation, including failures. It never merges
// metric versions or claims that similarly worded hypotheses are transferable.
type LearningGroup struct {
	Key        string                  `json:"key"`
	Hypothesis string                  `json:"hypothesis"`
	Records    []growth.LearningRecord `json:"records"`
	Limitation string                  `json:"limitation"`
}

func groupLearning(records []growth.LearningRecord) []LearningGroup {
	groups := map[string]*LearningGroup{}
	for _, record := range records {
		hypothesis := strings.ToLower(strings.Join(strings.Fields(record.OriginalHypothesis), " "))
		guards := append([]growth.MetricRef(nil), record.LinkedExperiment.Guardrails...)
		sort.Slice(guards, func(i, j int) bool { return guards[i].Key < guards[j].Key })
		key := digest(struct {
			Text, Workspace, Environment string
			Primary                      growth.MetricRef
			Guardrails                   []growth.MetricRef
		}{hypothesis, record.WorkspaceID, record.EnvironmentID, record.LinkedExperiment.PrimaryMetric, guards})
		group := groups[key]
		if group == nil {
			group = &LearningGroup{Key: key, Hypothesis: hypothesis, Records: []growth.LearningRecord{}, Limitation: "Lexical similarity only; no causal or cross-environment transferability established."}
			groups[key] = group
		}
		group.Records = append(group.Records, record)
	}
	out := []LearningGroup{}
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
