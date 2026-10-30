// Package growth provides an optional, unwired reader of aggregate evidence.
// It does not calculate statistics, authorize actions, or execute experiments.
package growth

import (
	"context"
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

type Diagnostics struct {
	ExperimentID string    `json:"experimentId"`
	Epoch        int64     `json:"epoch"`
	GeneratedAt  time.Time `json:"generatedAt"`
	Window       Window    `json:"window"`
	Issues       []string  `json:"issues,omitempty"`
}

type Variant struct {
	Variant         string   `json:"variant"`
	ExposedUnits    *int64   `json:"exposedUnits,omitempty"`
	RandomizedUnits *int64   `json:"randomizedUnits,omitempty"`
	Exposures       *int64   `json:"exposures,omitempty"`
	ConvertedUnits  *int64   `json:"convertedUnits,omitempty"`
	PrimaryValue    *float64 `json:"primaryValue,omitempty"`
}

type Report struct {
	ExperimentID        string       `json:"experimentId"`
	Epoch               int64        `json:"epoch"`
	EvidenceVersion     string       `json:"evidenceVersion"`
	GeneratedAt         time.Time    `json:"generatedAt"`
	RegistrationVersion string       `json:"registrationVersion"`
	AnalysisMethod      string       `json:"analysisMethod"`
	ExposureMode        string       `json:"exposureMode"`
	Window              Window       `json:"window"`
	Healthy             bool         `json:"healthy"`
	HealthIssues        []string     `json:"healthIssues,omitempty"`
	PrimaryError        string       `json:"primaryError,omitempty"`
	PrimaryMetricRef    *MetricRef   `json:"primaryMetricRef"`
	GuardrailMetricRefs []MetricRef  `json:"guardrailMetricRefs,omitempty"`
	Diagnostics         *Diagnostics `json:"diagnostics"`
	PerVariant          []Variant    `json:"perVariant"`
	Decision            Decision     `json:"decision"`
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
		if len(e.Metrics) != len(q.Metrics) || !historicalMetricsValid(e.Metrics) {
			status = Hold
		}
		if len(p.Decision.GuardrailBreaches) != 0 || !p.Healthy || p.PrimaryError != "" || len(p.HealthIssues) != 0 || !usableWindow(p.Window) || p.GeneratedAt.IsZero() ||
			p.PrimaryMetricRef == nil || p.Diagnostics == nil || !fingerprint.MatchString(p.RegistrationVersion) || !knownAnalysisMethod(p.AnalysisMethod) ||
			!usableVariants(p.PerVariant) || !knownVerdict(p.Decision.Verdict) || p.Decision.Verdict == "hold" {
			status = Hold
		}
		if d := p.Diagnostics; d != nil && (len(d.Issues) != 0 || d.GeneratedAt.IsZero() || !usableWindow(d.Window) || !sameWindow(d.Window, p.Window)) {
			status = Hold
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

// Historical observation permits deprecated frozen definitions, unlike new proposals.
func historicalMetricsValid(metrics []Metric) bool {
	for _, m := range metrics {
		if m.Status != "published" && m.Status != "deprecated" {
			return false
		}
	}
	return true
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

func knownVerdict(v string) bool {
	return v == "ship" || v == "rollback" || v == "continue" || v == "hold"
}

func sameWindow(a, b Window) bool {
	return a.Version == b.Version && a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Watermark.Equal(b.Watermark) && a.Backend == b.Backend && a.Complete == b.Complete && a.Recomputable == b.Recomputable
}

// Pointer aggregates distinguish absent data from a server-reported zero.
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
	}
	return true
}
