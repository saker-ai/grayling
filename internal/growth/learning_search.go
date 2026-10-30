package growth

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Search limits apply to every supplied file, including excluded scopes and duplicates.
const (
	MaxLearningSearchFiles = 64
	MaxLearningRecordBytes = 128 << 10
	MaxLearningSearchBytes = 4 << 20
)

// LearningSearchQuery requires an explicit scope and a case-sensitive literal
// substring in the original hypothesis or any follow-up. It is never executed.
type LearningSearchQuery struct {
	WorkspaceID   string
	EnvironmentID string
	Text          string
}

// LearningSearchResult contains caller-attested snapshots, not authenticated
// audit records, refreshed evidence, causal findings or publication authority.
type LearningSearchResult struct {
	SchemaVersion string           `json:"schemaVersion"`
	WorkspaceID   string           `json:"workspaceId"`
	EnvironmentID string           `json:"environmentId"`
	Matches       []LearningRecord `json:"matches"`
}

// Validate checks the explicit scope and bounded literal query without I/O.
func (q LearningSearchQuery) Validate() error {
	if !validSegment(q.WorkspaceID) || !validSegment(q.EnvironmentID) || !learningText(q.Text, MaxLearningFollowupBytes) {
		return ErrLearning
	}
	return nil
}

func learningText(s string, limit int) bool {
	if strings.TrimSpace(s) == "" || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return false
		}
	}
	return true
}

// DecodeLearningRecord bounds input and rejects unknown/duplicate JSON members,
// invalid UTF-8, trailing values and null. Validation is local and structural;
// the proposal fingerprint cannot be recomputed without the original proposal.
func DecodeLearningRecord(r io.Reader) (*LearningRecord, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxLearningRecordBytes+1))
	if err != nil || len(data) > MaxLearningRecordBytes {
		return nil, ErrLearning
	}
	var record *LearningRecord
	if json.Unmarshal(data, &record, json.RejectUnknownMembers(true)) != nil || record == nil || validateLearningRecord(*record) != nil {
		return nil, ErrLearning
	}
	return record, nil
}

func reportSource(id string) string {
	return "/api/v1/admin/experiments/" + id + "/report/export?format=json"
}

func validateLearningRecord(r LearningRecord) error {
	if r.SchemaVersion != LearningRecordSchemaVersion || !validSegment(r.ProposalID) || r.ProposalVersion < 1 || !fingerprint.MatchString(r.ProposalFingerprint) || !validSegment(r.WorkspaceID) || !validSegment(r.EnvironmentID) || !learningText(r.OriginalHypothesis, 4096) || !validSegment(r.Recorder) || r.RecordedAt.IsZero() || r.BusinessWinEstablished || r.PublicationAuthorized || len(r.Followups) < 1 || len(r.Followups) > MaxLearningFollowups {
		return ErrLearning
	}
	for _, f := range r.Followups {
		if !learningText(f, MaxLearningFollowupBytes) {
			return ErrLearning
		}
	}
	m, o := r.LinkedExperiment, r.Observation
	if m.validate() != nil || m.WorkspaceID != r.WorkspaceID || m.EnvironmentID != r.EnvironmentID || o.WorkspaceID != r.WorkspaceID || o.EnvironmentID != r.EnvironmentID || o.ExperimentID != m.ExperimentID || o.Epoch != m.Epoch || o.PrimaryMetric != m.PrimaryMetric || !exactMonitorGuards(o.Guardrails, m.Guardrails) || o.PublicationAuthorized {
		return ErrLearning
	}
	if o.Status != Ready && o.Status != Hold && o.Status != NeedsData {
		return ErrLearning
	}
	if len(o.Blockers) > 16 || (o.Status == Ready && len(o.Blockers) != 0) || (o.Status != Ready && len(o.Blockers) == 0) {
		return ErrLearning
	}
	// These are the fixed code/remediation pairs exported by Monitor, not prose
	// supplied by a report or an executable instruction.
	blockers := map[string]string{
		"primary_attribution_invalid":  "review_metric_definitions",
		"metric_lifecycle_invalid":     "review_metric_definitions",
		"evidence_missing":             "restore_evidence_access",
		"verdict_invalid":              "review_diagnostics",
		"exposure_mode_invalid":        "review_registration",
		"analysis_method_invalid":      "review_registration",
		"generation_time_missing":      "refresh_report",
		"srm_review_required":          "review_diagnostics",
		"health_review_required":       "review_diagnostics",
		"window_metadata_invalid":      "review_window",
		"window_incomplete":            "wait_for_complete_window",
		"window_not_recomputable":      "restore_recomputable_evidence",
		"watermark_late":               "wait_for_watermark",
		"diagnostics_missing":          "refresh_report",
		"diagnostics_metadata_invalid": "refresh_report",
		"aggregates_missing":           "restore_aggregate_evidence",
		"server_hold":                  "review_diagnostics",
	}
	seen := map[string]bool{}
	for _, b := range o.Blockers {
		if blockers[b.Code] == "" || blockers[b.Code] != b.Remediation || seen[b.Code] {
			return ErrLearning
		}
		seen[b.Code] = true
	}
	if (o.Status == NeedsData) != seen["evidence_missing"] {
		return ErrLearning
	}
	if o.EvidenceVersion == "" {
		// A requested evidence pin is not observed provenance when the report is absent.
		if o.Status != NeedsData || !seen["evidence_missing"] || o.SourcePath != "" || !o.GeneratedAt.IsZero() || o.RegistrationVersion != "" || o.AnalysisMethod != "" || o.ServerVerdict != "" || !emptyLearningWindow(o.Window) {
			return ErrLearning
		}
	} else {
		if !fingerprint.MatchString(o.EvidenceVersion) || o.RegistrationVersion != m.RegistrationVersion || (m.EvidenceVersion != "" && o.EvidenceVersion != m.EvidenceVersion) || o.SourcePath != reportSource(m.ExperimentID) || len(o.Window.Issues) != 0 {
			return ErrLearning
		}
		if (o.ServerVerdict == "" && !seen["verdict_invalid"]) || (o.ServerVerdict != "" && !knownVerdict(o.ServerVerdict)) || (o.ServerVerdict == "hold" && !seen["server_hold"]) {
			return ErrLearning
		}
		switch o.AnalysisMethod {
		case "two-proportion-z", "welch", "ratio-delta", "quantile", "cuped", "bandit-beta-bernoulli":
		default:
			if o.AnalysisMethod != "" || !seen["analysis_method_invalid"] {
				return ErrLearning
			}
		}
		w := o.Window
		if o.GeneratedAt.IsZero() && !seen["generation_time_missing"] {
			return ErrLearning
		}
		if !validWindowMetadata(w, o.GeneratedAt) && !seen["window_metadata_invalid"] {
			return ErrLearning
		}
		if (!w.Complete && !seen["window_incomplete"]) || (!w.Recomputable && !seen["window_not_recomputable"]) || (w.Watermark.Before(w.End) && !seen["watermark_late"]) {
			return ErrLearning
		}
	}
	return validateLearningBaselines(r.ProposalBaselineReferences)
}

// Baselines are copied claims; validate their local identity without requiring
// proposal readiness or comparing them to the linked experiment.
func validateLearningBaselines(refs []ProposalEvidenceRef) error {
	for _, b := range refs {
		if !validSegment(b.ExperimentID) || b.Epoch < 1 || !fingerprint.MatchString(b.EvidenceVersion) || b.SourcePath != reportSource(b.ExperimentID) {
			return ErrLearning
		}
	}
	return nil
}

func emptyLearningWindow(w Window) bool {
	return w.Version == "" && w.Start.IsZero() && w.End.IsZero() && w.Watermark.IsZero() && !w.Complete && !w.Recomputable && w.Backend == "" && len(w.Issues) == 0
}

// SearchLearning validates every input before filtering by scope or literal
// text. It returns independent snapshots, sorted by their canonical JSON bytes,
// collapsing identical records. File paths and other scopes are never returned.
// No Reader, clock, credential, network, persistence or statistical decision is used.
func SearchLearning(records []LearningRecord, q LearningSearchQuery) (*LearningSearchResult, error) {
	if q.Validate() != nil || len(records) < 1 || len(records) > MaxLearningSearchFiles {
		return nil, ErrLearning
	}
	encoded := make([][]byte, 0, len(records))
	total := 0
	for _, r := range records {
		if validateLearningRecord(r) != nil {
			return nil, ErrLearning
		}
		data, err := json.Marshal(r)
		total += len(data)
		if err != nil || len(data) > MaxLearningRecordBytes || total > MaxLearningSearchBytes {
			return nil, ErrLearning
		}
		if r.WorkspaceID != q.WorkspaceID || r.EnvironmentID != q.EnvironmentID {
			continue
		}
		match := strings.Contains(r.OriginalHypothesis, q.Text)
		for _, f := range r.Followups {
			match = match || strings.Contains(f, q.Text)
		}
		if match {
			encoded = append(encoded, data)
		}
	}
	sort.Slice(encoded, func(i, j int) bool { return bytes.Compare(encoded[i], encoded[j]) < 0 })
	result := &LearningSearchResult{SchemaVersion: "growth.learning-search.v1", WorkspaceID: q.WorkspaceID, EnvironmentID: q.EnvironmentID, Matches: []LearningRecord{}}
	for i, data := range encoded {
		if i > 0 && bytes.Equal(data, encoded[i-1]) {
			continue
		}
		var r LearningRecord
		if json.Unmarshal(data, &r) != nil {
			return nil, ErrLearning
		}
		result.Matches = append(result.Matches, r)
	}
	return result, nil
}
