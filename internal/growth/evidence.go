// Package growth provides an optional, unwired reader of aggregate evidence.
// It does not calculate statistics, authorize actions, or execute experiments.
package growth

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrConfig        = errors.New("growth: invalid configuration or reference")
	ErrTransport     = errors.New("growth: evidence request failed")
	ErrAuthorization = errors.New("growth: evidence access denied")
	ErrResponse      = errors.New("growth: invalid evidence response")
	ErrMismatch      = errors.New("growth: evidence scope or version mismatch")
)

type Status string

const (
	Ready     Status = "READY"
	NeedsData Status = "NEEDS_DATA"
	Hold      Status = "HOLD"
)

// Config fixes the trust boundary for the lifetime of a Reader. BaseURL must be
// an origin, without userinfo, path, query or fragment. Use HTTPS outside local
// tests. The transport and redirect policy are owned by the reader.
type Config struct {
	BaseURL, WorkspaceID, EnvironmentID, BearerToken string
	Timeout                                          time.Duration
	MaxResponseBytes                                 int64
}

type Reader struct {
	origin, workspace, environment, token string
	client                                http.Client
	limit                                 int64
}

var segment = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
var fingerprint = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validSegment(s string) bool { return segment.MatchString(s) && s != "." && s != ".." }

func NewReader(c Config) (*Reader, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		!validSegment(c.WorkspaceID) || !validSegment(c.EnvironmentID) || strings.TrimSpace(c.BearerToken) == "" ||
		strings.ContainsAny(c.BearerToken, "\r\n") {
		return nil, ErrConfig
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = 2 << 20
	}
	if c.Timeout < 0 || c.Timeout > 30*time.Second || c.MaxResponseBytes < 1 || c.MaxResponseBytes > 8<<20 {
		return nil, ErrConfig
	}
	return &Reader{origin: strings.TrimRight(c.BaseURL, "/"), workspace: c.WorkspaceID, environment: c.EnvironmentID,
		token: c.BearerToken, limit: c.MaxResponseBytes, client: http.Client{Timeout: c.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: c.Timeout,
			},
		}}, nil
}

// Close releases idle transport connections. It does not cancel active reads.
func (r *Reader) Close() { r.client.CloseIdleConnections() }

type MetricRef struct {
	Key            string `json:"key"`
	Version        int64  `json:"version"`
	DefinitionHash string `json:"definitionHash"`
}

// EvidenceVersion may be empty on first observation. When set, the snapshot
// must match it exactly; Epoch and metric hashes are always required.
type ExperimentRef struct {
	ID              string
	Epoch           int64
	EvidenceVersion string
}

type Query struct {
	Metrics     []MetricRef
	Experiments []ExperimentRef
}

// The wire and output types below are intentional field allowlists. Unknown
// server fields (including free-form metric descriptions and identities) never
// pass through to the envelope. Server issue/reason strings remain untrusted data.
type Metric struct {
	// AttributionWindow preserves catalog nanoseconds without rounding to hours.
	AttributionWindow int64 `json:"attributionWindow"`
	// Status is projected from the catalog; unknown values are redacted on read.
	Status      string `json:"status"`
	WorkspaceID string `json:"workspaceId"`
	Key         string `json:"key"`
	Version     int64  `json:"version"`
	Hash        string `json:"hash"`
	SourcePath  string `json:"sourcePath"`
}

type Window struct {
	Version      string    `json:"version"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Watermark    time.Time `json:"watermark"`
	Complete     bool      `json:"complete"`
	Recomputable bool      `json:"recomputable"`
	Backend      string    `json:"backend"`
	Issues       []string  `json:"issues,omitempty"`
}

type Decision struct {
	Verdict           string   `json:"verdict"`
	Reasons           []string `json:"reasons"`
	GuardrailBreaches []struct {
		Metric string `json:"metric"`
		Detail string `json:"detail"`
	} `json:"guardrailBreaches,omitempty"`
}

// DiagnosticVariant retains only the supplied aggregate delivery gap.
// Variant labels, identities and other flow statistics are deliberately omitted.
type DiagnosticVariant struct {
	Unexposed *int64 `json:"unexposed"`
}

type Diagnostics struct {
	PerVariant []DiagnosticVariant `json:"perVariant,omitempty"`
	// Optional server SRM results retain presence and only explicit health.
	AssignmentSRM SRM `json:"assignmentSrm,omitzero"`
	ExposureSRM   SRM `json:"exposureSrm,omitzero"`
	// ExposureMode must be recognized and match the report; unknown text is redacted.
	ExposureMode string `json:"exposureMode"`
	// Counts preserve exact server aggregates. Missing and null require review.
	CrossVariantUnits  *int64    `json:"crossVariantUnits"`
	AssignmentMismatch *int64    `json:"assignmentMismatch"`
	ExperimentID       string    `json:"experimentId"`
	Epoch              int64     `json:"epoch"`
	GeneratedAt        time.Time `json:"generatedAt"`
	Window             Window    `json:"window"`
	Issues             []string  `json:"issues,omitempty"`
}

// diagnosticsCountsValid trusts only explicit zero counts, independently of
// summary health, issue prose or verdict. No statistical inference is performed.
func diagnosticsCountsValid(d *Diagnostics, reportMode string) bool {
	if d == nil || d.CrossVariantUnits == nil || *d.CrossVariantUnits != 0 ||
		d.AssignmentMismatch == nil || *d.AssignmentMismatch != 0 {
		return false
	}
	// Validate supplied onRender evidence only; omitted coverage is not inferred.
	if reportMode == "onRender" {
		for _, entry := range d.PerVariant {
			if entry.Unexposed == nil || *entry.Unexposed != 0 {
				return false
			}
		}
	}
	return true
}

type Variant struct {
	Variant         string   `json:"variant"`
	ExposedUnits    *int64   `json:"exposedUnits,omitempty"`
	RandomizedUnits *int64   `json:"randomizedUnits,omitempty"`
	Exposures       *int64   `json:"exposures,omitempty"`
	ConvertedUnits  *int64   `json:"convertedUnits,omitempty"`
	PrimaryValue    *float64 `json:"primaryValue,omitempty"`
}

// SRM retains only server health, with omission distinct from explicit null.
// No test statistics, thresholds, or free-form server fields cross this boundary.
type SRM struct {
	Healthy        *bool `json:"healthy"`
	present        bool
	null           bool
	healthyPresent bool
}

func (s SRM) IsZero() bool { return !s.present && s.Healthy == nil }

func (s *SRM) UnmarshalJSON(data []byte) error {
	var wire *struct {
		Healthy jsontext.Value `json:"healthy"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = SRM{present: true, null: wire == nil}
	if wire != nil {
		s.healthyPresent = len(wire.Healthy) != 0
		if s.healthyPresent {
			return json.Unmarshal(wire.Healthy, &s.Healthy)
		}
	}
	return nil
}

func (s SRM) MarshalJSON() ([]byte, error) {
	if s.null {
		return []byte("null"), nil
	}
	if s.Healthy == nil && !s.healthyPresent {
		return []byte("{}"), nil
	}
	return json.Marshal(struct {
		Healthy *bool `json:"healthy"`
	}{s.Healthy})
}

// Diagnostics legitimately omit SRM when no optional test result is available.
// Supplied results require explicit health, independently of report SRM.
func diagnosticsSRMValid(d *Diagnostics) bool {
	return d == nil || ((d.AssignmentSRM.IsZero() || d.AssignmentSRM.valid()) &&
		(d.ExposureSRM.IsZero() || d.ExposureSRM.valid()))
}

func (s SRM) valid() bool { return !s.null && s.Healthy != nil && *s.Healthy }

// Only Bandit may omit SRM. An explicit null or incomplete/unhealthy object
// cannot be overridden by the report's summary health or decision verdict.
func srmValid(p Report) bool {
	if p.SRM.IsZero() {
		return p.AnalysisMethod == "bandit-beta-bernoulli"
	}
	return p.SRM.valid()
}

// GuardrailCheck retains only the server's explicit breach state. Missing or
// null booleans require review; names, thresholds and prose are not evidence.
type GuardrailCheck struct {
	Breached *bool `json:"breached"`
}

// Omitted, null and empty arrays remain compatible. Validate supplied checks
// without inferring coverage or recalculating the server's thresholds.
func guardrailsValid(p Report) bool {
	for _, check := range p.Guardrails {
		if check.Breached == nil || *check.Breached {
			return false
		}
	}
	return true
}

// JourneySection retains only supplied maturity evidence. Labels, definitions,
// population statistics and arbitrary exclusion keys never cross this boundary.
type JourneySection struct {
	Arms []JourneyArm `json:"arms"`
}

type JourneyArm struct {
	Immature *int64            `json:"immature"`
	Excluded JourneyExclusions `json:"excluded,omitzero"`
}

type JourneyExclusions struct {
	ImmatureCohort         *int64 `json:"immature_cohort,omitempty"`
	AttributionWindowShort *int64 `json:"attribution_window_short,omitempty"`
}

// Validate supplied sections only; do not infer coverage, cohort maturity from
// clocks, or retention/statistical results. A single supplied arm is usable.
func journeysValid(p Report) bool {
	for _, section := range p.Journeys {
		if len(section.Arms) == 0 {
			return false
		}
		for _, arm := range section.Arms {
			if arm.Immature == nil || *arm.Immature != 0 {
				return false
			}
			for _, count := range []*int64{arm.Excluded.ImmatureCohort, arm.Excluded.AttributionWindowShort} {
				if count != nil && *count != 0 {
					return false
				}
			}
		}
	}
	return true
}

type Report struct {
	Journeys            []JourneySection `json:"journeys,omitempty"`
	Guardrails          []GuardrailCheck `json:"guardrails,omitempty"`
	SRM                 SRM              `json:"srm,omitzero"`
	ExperimentID        string           `json:"experimentId"`
	Epoch               int64            `json:"epoch"`
	EvidenceVersion     string           `json:"evidenceVersion"`
	GeneratedAt         time.Time        `json:"generatedAt"`
	RegistrationVersion string           `json:"registrationVersion"`
	AnalysisMethod      string           `json:"analysisMethod"`
	ExposureMode        string           `json:"exposureMode"`
	Window              Window           `json:"window"`
	Healthy             bool             `json:"healthy"`
	HealthIssues        []string         `json:"healthIssues,omitempty"`
	PrimaryError        string           `json:"primaryError,omitempty"`
	PrimaryMetricRef    *MetricRef       `json:"primaryMetricRef"`
	GuardrailMetricRefs []MetricRef      `json:"guardrailMetricRefs,omitempty"`
	Diagnostics         *Diagnostics     `json:"diagnostics"`
	PerVariant          []Variant        `json:"perVariant"`
	Decision            Decision         `json:"decision"`

	// PrimaryAttributionWindow is the server analysis duration in exact nanoseconds.
	PrimaryAttributionWindow int64 `json:"primaryAttributionWindow"`
}

type Evidence struct {
	SourcePath string `json:"sourcePath"`
	Status     Status `json:"status"`
	Report     Report `json:"report"`
}

type Envelope struct {
	WorkspaceID   string     `json:"workspaceId"`
	EnvironmentID string     `json:"environmentId"`
	Status        Status     `json:"status"`
	Metrics       []Metric   `json:"metrics"`
	Evidence      []Evidence `json:"evidence"`
	MissingPaths  []string   `json:"missingPaths,omitempty"`
}

// get only accepts internally constructed paths. Errors never wrap a URL,
// credential, response body, decoder message, or transport error.
func (r *Reader) get(ctx context.Context, path string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.origin+path, nil)
	if err != nil {
		return false, ErrConfig
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, ErrTransport
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, ErrAuthorization
	case http.StatusNotFound:
		return true, nil
	case http.StatusOK:
	default:
		return false, ErrResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, ErrTransport
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if int64(len(data)) > r.limit || json.Unmarshal(data, out) != nil {
		return false, ErrResponse
	}
	return false, nil
}

func validMetric(m MetricRef) bool {
	return validSegment(m.Key) && m.Version > 0 && fingerprint.MatchString(m.DefinitionHash)
}
func metricID(m MetricRef) string { return m.Key + "/" + strconv.FormatInt(m.Version, 10) }

// Read performs only metric-version GETs and JSON report-export GETs. Any
// rejected scope/version or authorization invalidates the entire read: no
// partial evidence is returned. There are no retries or writes.
func (r *Reader) Read(ctx context.Context, q Query) (*Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(q.Metrics) > 100 || len(q.Experiments) > 100 {
		return nil, ErrConfig
	}
	refs := make(map[string]MetricRef)
	for _, m := range q.Metrics {
		if !validMetric(m) {
			return nil, ErrConfig
		}
		if _, ok := refs[metricID(m)]; ok {
			return nil, ErrConfig
		}
		refs[metricID(m)] = m
	}
	seen := make(map[string]bool)
	for _, exp := range q.Experiments {
		if !validSegment(exp.ID) || exp.Epoch < 1 || seen[exp.ID] ||
			(exp.EvidenceVersion != "" && !fingerprint.MatchString(exp.EvidenceVersion)) {
			return nil, ErrConfig
		}
		seen[exp.ID] = true
	}
	e := &Envelope{WorkspaceID: r.workspace, EnvironmentID: r.environment, Status: Ready,
		Metrics: []Metric{}, Evidence: []Evidence{}}
	if len(q.Metrics) == 0 {
		e.Status = NeedsData
		return e, nil
	}
	for _, m := range q.Metrics {
		path := "/api/v1/admin/workspaces/" + r.workspace + "/metrics/" + m.Key + "/versions/" + strconv.FormatInt(m.Version, 10)
		var got *Metric
		missing, err := r.get(ctx, path, &got)
		if err != nil {
			return nil, err
		}
		if missing {
			e.MissingPaths = append(e.MissingPaths, path)
			continue
		}
		if got == nil {
			return nil, ErrResponse
		}
		if got.WorkspaceID != r.workspace || got.Key != m.Key || got.Version != m.Version || got.Hash != m.DefinitionHash {
			return nil, ErrMismatch
		}
		switch got.Status {
		case "draft", "published", "deprecated":
		default:
			got.Status = ""
		}
		got.SourcePath = path
		e.Metrics = append(e.Metrics, *got)
	}
	for _, exp := range q.Experiments {
		path := "/api/v1/admin/experiments/" + exp.ID + "/report/export?format=json"
		var got *struct {
			WorkspaceID   string  `json:"workspaceId"`
			EnvironmentID string  `json:"environmentId"`
			Report        *Report `json:"report"`
		}
		missing, err := r.get(ctx, path, &got)
		if err != nil {
			return nil, err
		}
		if missing {
			e.MissingPaths = append(e.MissingPaths, path)
			continue
		}
		if got == nil {
			return nil, ErrResponse
		}
		if got.WorkspaceID != r.workspace || got.EnvironmentID != r.environment {
			return nil, ErrMismatch
		}
		if got.Report == nil {
			e.MissingPaths = append(e.MissingPaths, path)
			continue
		}
		p := got.Report
		if p.ExperimentID != exp.ID || p.Epoch != exp.Epoch || !fingerprint.MatchString(p.EvidenceVersion) ||
			(exp.EvidenceVersion != "" && p.EvidenceVersion != exp.EvidenceVersion) {
			return nil, ErrMismatch
		}
		if p.PrimaryMetricRef != nil {
			for _, m := range append([]MetricRef{*p.PrimaryMetricRef}, p.GuardrailMetricRefs...) {
				if want, ok := refs[metricID(m)]; !ok || !validMetric(m) || want != m {
					return nil, ErrMismatch
				}
			}
		} else if len(p.GuardrailMetricRefs) != 0 {
			return nil, ErrMismatch
		}
		if d := p.Diagnostics; d != nil && (d.ExperimentID != exp.ID || d.Epoch != exp.Epoch) {
			return nil, ErrMismatch
		}
		status := Ready
		if len(e.Metrics) != len(q.Metrics) || !historicalMetricsValid(e.Metrics) || !primaryAttributionValid(*p, e.Metrics) {
			status = Hold
		}
		if !diagnosticsCountsValid(p.Diagnostics, p.ExposureMode) || !journeysValid(*p) || !guardrailsValid(*p) || !srmValid(*p) || !diagnosticsSRMValid(p.Diagnostics) || len(p.Decision.GuardrailBreaches) != 0 || !p.Healthy || p.PrimaryError != "" || len(p.HealthIssues) != 0 || !usableWindow(p.Window) || !validWindowMetadata(p.Window, p.GeneratedAt) || p.GeneratedAt.IsZero() ||
			p.PrimaryMetricRef == nil || p.Diagnostics == nil || !fingerprint.MatchString(p.RegistrationVersion) || !knownAnalysisMethod(p.AnalysisMethod) || !knownExposureMode(p.ExposureMode) ||
			!usableVariants(p.PerVariant) || !knownVerdict(p.Decision.Verdict) || p.Decision.Verdict == "hold" {
			status = Hold
		}
		if d := p.Diagnostics; d != nil && (!diagnosticsExposureValid(d, p.ExposureMode) || len(d.Issues) != 0 || d.GeneratedAt.IsZero() || !d.GeneratedAt.Equal(p.GeneratedAt) || !usableWindow(d.Window) || !validWindowMetadata(d.Window, p.GeneratedAt) || !sameWindow(d.Window, p.Window)) {
			status = Hold
		}
		if d := p.Diagnostics; d != nil && !knownExposureMode(d.ExposureMode) {
			d.ExposureMode = ""
		}
		e.Evidence = append(e.Evidence, Evidence{SourcePath: path, Status: status, Report: *p})
		if status == Hold {
			e.Status = Hold
		}
	}
	if len(e.MissingPaths) != 0 || len(q.Metrics) == 0 || len(q.Experiments) == 0 {
		e.Status = NeedsData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e, nil
}

// Compare only the exact frozen primary definition, never another metric or
// rounded hours. Absent/null wire durations decode to zero and cannot be READY.
func primaryAttributionValid(p Report, metrics []Metric) bool {
	if p.PrimaryMetricRef == nil || p.PrimaryAttributionWindow <= 0 {
		return false
	}
	ref := *p.PrimaryMetricRef
	for _, m := range metrics {
		if m.Key == ref.Key && m.Version == ref.Version && m.Hash == ref.DefinitionHash {
			return m.AttributionWindow > 0 && p.PrimaryAttributionWindow == m.AttributionWindow
		}
	}
	return false
}

// Historical observation permits deprecated frozen definitions, unlike new proposals.
func historicalMetricsValid(metrics []Metric) bool {
	for _, m := range metrics {
		if m.Status != "published" && m.Status != "deprecated" {
			return false
		}
	}
	return true
}

// Window consistency is relative to the report, independent of the local clock.
// Missing generation time is handled separately by each caller.
func validWindowMetadata(w Window, generatedAt time.Time) bool {
	return w.Version != "" && !w.Start.IsZero() && w.End.After(w.Start) && !w.Watermark.IsZero() &&
		(generatedAt.IsZero() || !w.Watermark.After(generatedAt))
}

func usableWindow(w Window) bool {
	return w.Version != "" && !w.Start.IsZero() && w.End.After(w.Start) && !w.Watermark.IsZero() &&
		!w.Watermark.Before(w.End) && w.Complete && w.Recomputable && len(w.Issues) == 0
}

// knownAnalysisMethod recognizes report observation semantics, including Bandit.
// Draft compilation retains its separate fixed-split restrictions.
func knownAnalysisMethod(method string) bool {
	switch method {
	case "two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli":
		return true
	default:
		return false
	}
}

// Historical reports support both exposure semantics. New proposals separately
// require onRender; observation must not apply that creation restriction.
func knownExposureMode(mode string) bool {
	return mode == "onDecision" || mode == "onRender"
}

// Diagnostics must explicitly identify the same recognized exposure semantics.
func diagnosticsExposureValid(d *Diagnostics, reportMode string) bool {
	return d != nil && knownExposureMode(d.ExposureMode) && d.ExposureMode == reportMode
}

func knownVerdict(v string) bool {
	return v == "ship" || v == "rollback" || v == "continue" || v == "hold"
}

func sameWindow(a, b Window) bool {
	return a.Version == b.Version && a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Watermark.Equal(b.Watermark) && a.Backend == b.Backend && a.Complete == b.Complete && a.Recomputable == b.Recomputable
}

// Pointer aggregates distinguish absent data from a server-reported zero.
// Converted units are randomized; exposed units also include deterministic
// targeting, and each exposed unit requires at least one exposure event.
func usableVariants(variants []Variant) bool {
	if len(variants) < 2 {
		return false
	}
	seen := make(map[string]bool)
	for _, v := range variants {
		if v.Variant == "" || seen[v.Variant] || v.PrimaryValue == nil {
			return false
		}
		seen[v.Variant] = true
		for _, count := range []*int64{v.ExposedUnits, v.RandomizedUnits, v.Exposures, v.ConvertedUnits} {
			if count == nil || *count < 0 {
				return false
			}
		}
		if *v.ConvertedUnits > *v.RandomizedUnits || *v.RandomizedUnits > *v.ExposedUnits ||
			*v.ExposedUnits > *v.Exposures {
			return false
		}
	}
	return true
}
