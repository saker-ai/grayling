package growthservice

import "github.com/saker-ai/grayling/internal/growth"

// Preserve booleans, exact numerical facts and pins, replacing untrusted prose
// with a fixed marker. Nonempty error/issue lists remain nonempty for all gates.
func sanitize(e *growth.Envelope) *growth.Envelope {
	if e == nil {
		return nil
	}
	issues := func(v []string) []string {
		if len(v) == 0 {
			return nil
		}
		return []string{"server_issue"}
	}
	for i := range e.Evidence {
		p := &e.Evidence[i].Report
		p.HealthIssues = issues(p.HealthIssues)
		p.Window.Issues = issues(p.Window.Issues)
		p.Decision.Reasons = issues(p.Decision.Reasons)
		if p.PrimaryError != "" {
			p.PrimaryError = "server_error"
		}
		for j := range p.Decision.GuardrailBreaches {
			p.Decision.GuardrailBreaches[j].Metric = ""
			p.Decision.GuardrailBreaches[j].Detail = "server_breach"
		}
		if p.Diagnostics != nil {
			p.Diagnostics.Issues = issues(p.Diagnostics.Issues)
			p.Diagnostics.Window.Issues = issues(p.Diagnostics.Window.Issues)
		}
		for j := range p.PerVariant {
			if !identifier.MatchString(p.PerVariant[j].Variant) {
				p.PerVariant[j].Variant = "redacted"
			}
		}
	}
	return e
}
