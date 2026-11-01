package growthservice

import (
	"sort"
	"strings"
	"unicode"

	"github.com/saker-ai/grayling/internal/growth"
)

// Groups preserve every record, including failures, under immutable scope/metric
// roles. Lexical similarity is a search aid, never causal transfer or a verdict.
type LearningGroup struct {
	Key        string                  `json:"key"`
	Hypothesis string                  `json:"hypothesis"`
	Records    []growth.LearningRecord `json:"records"`
	Limitation string                  `json:"limitation"`
}

func hypothesisTokens(text string) map[string]bool {
	tokens := map[string]bool{}
	var word []rune
	flush := func() {
		if len(word) > 0 {
			tokens[string(word)] = true
			word = nil
		}
	}
	for _, r := range strings.ToLower(text) {
		if unicode.Is(unicode.Han, r) {
			flush()
			tokens[string(r)] = true
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}
func similarHypothesis(a, b string) bool {
	if strings.ToLower(strings.Join(strings.Fields(a), " ")) == strings.ToLower(strings.Join(strings.Fields(b), " ")) {
		return true
	}
	x, y := hypothesisTokens(a), hypothesisTokens(b)
	// Do not conflate a positive hypothesis with its explicit negation.
	for _, negation := range []string{"not", "no", "never", "without", "不", "未", "无"} {
		if x[negation] != y[negation] {
			return false
		}
	}
	shared := 0
	for k := range x {
		if y[k] {
			shared++
		}
	}
	union := len(x) + len(y) - shared
	return shared >= 4 && union > 0 && float64(shared)/float64(union) >= .8
}
func learningScope(record growth.LearningRecord) string {
	guards := append([]growth.MetricRef(nil), record.LinkedExperiment.Guardrails...)
	sort.Slice(guards, func(i, j int) bool { return guards[i].Key < guards[j].Key })
	return digest(struct {
		Workspace, Environment string
		Primary                growth.MetricRef
		Guardrails             []growth.MetricRef
	}{record.WorkspaceID, record.EnvironmentID, record.LinkedExperiment.PrimaryMetric, guards})
}
func groupLearning(records []growth.LearningRecord) []LearningGroup {
	ordered := append([]growth.LearningRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.OriginalHypothesis != b.OriginalHypothesis {
			return a.OriginalHypothesis < b.OriginalHypothesis
		}
		if a.ProposalID != b.ProposalID {
			return a.ProposalID < b.ProposalID
		}
		return a.ProposalVersion < b.ProposalVersion
	})
	out := []LearningGroup{}
	scopes := []string{}
	for _, record := range ordered {
		scope := learningScope(record)
		matched := -1
		for i, g := range out {
			if scope == scopes[i] && similarHypothesis(record.OriginalHypothesis, g.Hypothesis) {
				matched = i
				break
			}
		}
		if matched >= 0 {
			out[matched].Records = append(out[matched].Records, record)
			continue
		}
		hypothesis := strings.ToLower(strings.Join(strings.Fields(record.OriginalHypothesis), " "))
		out = append(out, LearningGroup{Key: digest(struct{ Scope, Hypothesis string }{scope, hypothesis}), Hypothesis: hypothesis, Records: []growth.LearningRecord{record}, Limitation: "Lexical grouping only (token overlap at least four and Jaccard at least .8); every observation retained. No causal, semantic or cross-environment transferability established."})
		scopes = append(scopes, scope)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
