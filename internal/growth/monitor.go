package growth

import (
	"context"
	"encoding/json/v2"
	"io"
	"time"
)

// MaxMonitorManifestBytes bounds the complete operator manifest.
const MaxMonitorManifestBytes = 16 << 10

// MonitorManifest is operator-owned. It links an actual experiment, never a
// proposal baseline. Credentials and destinations cannot be supplied here.
type MonitorManifest struct {
	WorkspaceID         string      `json:"workspaceId"`
	EnvironmentID       string      `json:"environmentId"`
	ExperimentID        string      `json:"experimentId"`
	Epoch               int64       `json:"epoch"`
	RegistrationVersion string      `json:"registrationVersion"`
	EvidenceVersion     string      `json:"evidenceVersion,omitempty"`
	PrimaryMetric       MetricRef   `json:"primaryMetric"`
	Guardrails          []MetricRef `json:"guardrails"`
}

func (m MonitorManifest) validate() error {
	if !validSegment(m.WorkspaceID) || !validSegment(m.EnvironmentID) || !validSegment(m.ExperimentID) || m.Epoch < 1 ||
		!fingerprint.MatchString(m.RegistrationVersion) || (m.EvidenceVersion != "" && !fingerprint.MatchString(m.EvidenceVersion)) || !validMetric(m.PrimaryMetric) || len(m.Guardrails) > 16 {
		return ErrConfig
	}
	seen := map[string]bool{m.PrimaryMetric.Key: true}
	for _, g := range m.Guardrails {
		if !validMetric(g) || seen[g.Key] {
			return ErrConfig
		}
		seen[g.Key] = true
	}
	return nil
}

// DecodeMonitorManifest rejects unknown/duplicate members and oversized JSON.
func DecodeMonitorManifest(r io.Reader) (*MonitorManifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxMonitorManifestBytes+1))
	if err != nil || len(data) > MaxMonitorManifestBytes {
		return nil, ErrConfig
	}
	var m *MonitorManifest
	if json.Unmarshal(data, &m, json.RejectUnknownMembers(true)) != nil || m == nil {
		return nil, ErrConfig
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// MonitorBlocker contains only implementation-owned codes, never server prose.
type MonitorBlocker struct {
	Code        string `json:"code"`
	Remediation string `json:"remediation"`
}

// MonitorSummary exports allowlisted evidence provenance and fixed blockers.
// PublicationAuthorized is always false.
type MonitorSummary struct {
	PrimaryMetric         MetricRef        `json:"primaryMetric"`
	Guardrails            []MetricRef      `json:"guardrails"`
	WorkspaceID           string           `json:"workspaceId"`
	EnvironmentID         string           `json:"environmentId"`
	ExperimentID          string           `json:"experimentId"`
	Epoch                 int64            `json:"epoch"`
	Status                Status           `json:"status"`
	ServerVerdict         string           `json:"serverVerdict"`
	PublicationAuthorized bool             `json:"publicationAuthorized"`
	EvidenceVersion       string           `json:"evidenceVersion"`
	SourcePath            string           `json:"sourcePath"`
	GeneratedAt           time.Time        `json:"generatedAt"`
	RegistrationVersion   string           `json:"registrationVersion"`
	AnalysisMethod        string           `json:"analysisMethod"`
	Window                Window           `json:"window"`
	Blockers              []MonitorBlocker `json:"blockers"`
}

// Monitor performs one read and returns a conservative snapshot. READY means
// evidence is inspectable; no status or server verdict authorizes publication.
func (r *Reader) Monitor(ctx context.Context, m MonitorManifest) (*MonitorSummary, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	if m.WorkspaceID != r.workspace || m.EnvironmentID != r.environment {
		return nil, ErrMismatch
	}
	e, err := r.Read(ctx, Query{Metrics: append([]MetricRef{m.PrimaryMetric}, m.Guardrails...), Experiments: []ExperimentRef{{ID: m.ExperimentID, Epoch: m.Epoch, EvidenceVersion: m.EvidenceVersion}}})
	if err != nil {
		return nil, err
	}
	s := &MonitorSummary{PrimaryMetric: m.PrimaryMetric, Guardrails: append([]MetricRef{}, m.Guardrails...), WorkspaceID: m.WorkspaceID, EnvironmentID: m.EnvironmentID, ExperimentID: m.ExperimentID, Epoch: m.Epoch, Status: Ready, SourcePath: "/api/v1/admin/experiments/" + m.ExperimentID + "/report/export?format=json", Blockers: []MonitorBlocker{}}
	add := func(code, action string) {
		for _, b := range s.Blockers {
			if b.Code == code {
				return
			}
		}
		s.Blockers = append(s.Blockers, MonitorBlocker{code, action})
		s.Status = Hold
	}
	if !historicalMetricsValid(e.Metrics) {
		add("metric_lifecycle_invalid", "review_metric_definitions")
	}
	if len(e.MissingPaths) > 0 {
		add("evidence_missing", "restore_evidence_access")
	}
	if len(e.Evidence) == 0 {
		s.Status = NeedsData
		return s, nil
	}
	p := e.Evidence[0].Report
	if p.RegistrationVersion != m.RegistrationVersion || p.PrimaryMetricRef == nil || *p.PrimaryMetricRef != m.PrimaryMetric || !exactMonitorGuards(p.GuardrailMetricRefs, m.Guardrails) {
		return nil, ErrMismatch
	}
	if !primaryAttributionValid(p, e.Metrics) {
		add("primary_attribution_invalid", "review_metric_definitions")
	}
	s.EvidenceVersion, s.GeneratedAt, s.RegistrationVersion = p.EvidenceVersion, p.GeneratedAt, p.RegistrationVersion
	s.Window = p.Window
	s.Window.Issues = nil // untrusted free-form text is never exported
	if knownVerdict(p.Decision.Verdict) {
		s.ServerVerdict = p.Decision.Verdict
	} else {
		add("verdict_invalid", "review_diagnostics")
	}
	if knownAnalysisMethod(p.AnalysisMethod) {
		s.AnalysisMethod = p.AnalysisMethod
	} else {
		add("analysis_method_invalid", "review_registration")
	}
	if !knownExposureMode(p.ExposureMode) {
		add("exposure_mode_invalid", "review_registration")
	}
	if p.GeneratedAt.IsZero() {
		add("generation_time_missing", "refresh_report")
	}
	if !srmValid(p) || !diagnosticsSRMValid(p.Diagnostics) {
		add("srm_review_required", "review_diagnostics")
	}
	// The shared count check includes epoch-wide rejections; HOLD requests
	// investigation and does not reinterpret the server verdict or infer bias.
	if (p.Diagnostics != nil && !diagnosticsCountsValid(p.Diagnostics, p.ExposureMode)) || !journeysValid(p) || !guardrailsValid(p) || !p.Healthy || p.PrimaryError != "" || len(p.HealthIssues) > 0 || len(p.Decision.GuardrailBreaches) > 0 {
		add("health_review_required", "review_diagnostics")
	}
	checkWindow := func(w Window) {
		if !validWindowMetadata(w, p.GeneratedAt) {
			add("window_metadata_invalid", "review_window")
		}
		if !w.Complete {
			add("window_incomplete", "wait_for_complete_window")
		}
		if !w.Recomputable {
			add("window_not_recomputable", "restore_recomputable_evidence")
		}
		if w.Watermark.Before(w.End) {
			add("watermark_late", "wait_for_watermark")
		}
		if len(w.Issues) > 0 {
			add("health_review_required", "review_diagnostics")
		}
	}
	checkWindow(p.Window)
	if p.Diagnostics == nil {
		add("diagnostics_missing", "refresh_report")
	} else {
		d := p.Diagnostics
		checkWindow(d.Window)
		if !diagnosticsExposureValid(d, p.ExposureMode) || d.GeneratedAt.IsZero() || !d.GeneratedAt.Equal(p.GeneratedAt) || !sameWindow(d.Window, p.Window) {
			add("diagnostics_metadata_invalid", "refresh_report")
		}
		if len(d.Issues) > 0 {
			add("health_review_required", "review_diagnostics")
		}
	}
	if !usableVariants(p.PerVariant) {
		add("aggregates_missing", "restore_aggregate_evidence")
	}
	if p.Decision.Verdict == "hold" {
		add("server_hold", "review_diagnostics")
	}
	if e.Status == Hold && s.Status == Ready {
		add("health_review_required", "review_diagnostics")
	}
	if len(e.MissingPaths) > 0 {
		s.Status = NeedsData
	}
	return s, nil
}

func exactMonitorGuards(got, want []MetricRef) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[MetricRef]bool, len(got))
	for _, g := range got {
		if seen[g] {
			return false
		}
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}
