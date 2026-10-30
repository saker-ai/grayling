package growth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"strconv"
	"time"
)

const GoalSchemaVersion = "growth.goal.v1"
const GoalVerificationSchemaVersion = "growth.goal-verification.v1"
const MaxGoalBytes = 64 << 10

var ErrGoal = errors.New("growth: invalid goal artifact")
var ErrGoalVerification = errors.New("growth: invalid or inconsistent goal artifacts")

// GrowthGoal is caller-attested planning data, not authenticated ownership,
// baseline evidence, readiness, or permission to execute or publish.
type GrowthGoal struct {
	SchemaVersion  string           `json:"schemaVersion"`
	ID             string           `json:"id"`
	Version        int64            `json:"version"`
	WorkspaceID    string           `json:"workspaceId"`
	EnvironmentID  string           `json:"environmentId"`
	Owner          string           `json:"owner"`
	Objective      string           `json:"objective"`
	PrimaryMetric  ProposalMetric   `json:"primaryMetric"`
	Guardrails     []ProposalMetric `json:"guardrails"`
	BaselineWindow BaselineWindow   `json:"baselineWindow"`
}

// BaselineWindow is only a caller's observation interval. It carries no data
// quality, watermark, sample size, target, or statistical adequacy assertion.
type BaselineWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type GoalVerification struct {
	SchemaVersion         string `json:"schemaVersion"`
	WorkspaceID           string `json:"workspaceId"`
	EnvironmentID         string `json:"environmentId"`
	GoalID                string `json:"goalId"`
	GoalVersion           int64  `json:"goalVersion"`
	GoalFingerprint       string `json:"goalFingerprint"`
	ProposalID            string `json:"proposalId"`
	ProposalVersion       int64  `json:"proposalVersion"`
	ProposalFingerprint   string `json:"proposalFingerprint"`
	PublicationAuthorized bool   `json:"publicationAuthorized"`
}

// DecodeGoal bounds the actual read, rejecting unknown/duplicate members at
// every depth, null roots, invalid UTF-8, malformed and trailing JSON.
func DecodeGoal(r io.Reader) (*GrowthGoal, error) {
	if r == nil {
		return nil, ErrGoal
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxGoalBytes+1))
	if err != nil || len(data) > MaxGoalBytes {
		return nil, ErrGoal
	}
	var g *GrowthGoal
	if json.Unmarshal(data, &g, json.RejectUnknownMembers(true)) != nil || g == nil || ValidateGoal(*g) != nil {
		return nil, ErrGoal
	}
	return g, nil
}

// validGoalMetrics reuses metric identity and canonical metric-version paths.
// A metric key may occupy only one role/version in this planning contract.
func validGoalMetrics(workspace string, primary ProposalMetric, guardrails []ProposalMetric) bool {
	if len(guardrails) < 1 || len(guardrails) > 20 {
		return false
	}
	seen := map[string]bool{}
	for _, m := range append([]ProposalMetric{primary}, guardrails...) {
		path := "/api/v1/admin/workspaces/" + workspace + "/metrics/" + m.Ref.Key + "/versions/" + strconv.FormatInt(m.Ref.Version, 10)
		if !validMetric(m.Ref) || seen[m.Ref.Key] || m.SourcePath != path {
			return false
		}
		seen[m.Ref.Key] = true
	}
	return true
}

func ValidateGoal(g GrowthGoal) error {
	data, err := json.Marshal(g)
	if err != nil || len(data) > MaxGoalBytes || g.SchemaVersion != GoalSchemaVersion || !validSegment(g.ID) || g.Version < 1 || !validSegment(g.WorkspaceID) || !validSegment(g.EnvironmentID) || !validSegment(g.Owner) || !learningText(g.Objective, 4096) || g.BaselineWindow.Start.IsZero() || g.BaselineWindow.End.IsZero() || !g.BaselineWindow.End.After(g.BaselineWindow.Start) || !validGoalMetrics(g.WorkspaceID, g.PrimaryMetric, g.Guardrails) {
		return ErrGoal
	}
	return nil
}

// GoalFingerprint hashes complete canonical goal JSON, preserving array order
// and exact int64 versions, following ProposalFingerprint's integer semantics.
func GoalFingerprint(g GrowthGoal) (string, error) {
	if ValidateGoal(g) != nil {
		return "", ErrGoal
	}
	data, err := json.Marshal(g, json.Deterministic(true))
	if err != nil {
		return "", ErrGoal
	}
	value := jsontext.Value(data)
	if value.Canonicalize(jsontext.CanonicalizeRawInts(false)) != nil {
		return "", ErrGoal
	}
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyGoal is pure and requires operator scope supplied separately from the
// artifacts. Success means consistency only, never readiness, approval,
// trustworthy baseline facts, statistical adequacy, or publication authority.
// Unpinned proposals retain ID-only linkage; supplied pins match both values.
func VerifyGoal(g GrowthGoal, p Proposal, workspace, environment string) (*GoalVerification, error) {
	if !validSegment(workspace) || !validSegment(environment) || ValidateGoal(g) != nil || p.SchemaVersion != ProposalSchemaVersion || !validSegment(p.ID) || p.Version < 1 || p.GoalID != g.ID || g.WorkspaceID != workspace || p.WorkspaceID != workspace || g.EnvironmentID != environment || p.EnvironmentID != environment || !validGoalMetrics(workspace, p.PrimaryMetric, p.Guardrails) || p.PrimaryMetric != g.PrimaryMetric || len(p.Guardrails) != len(g.Guardrails) {
		return nil, ErrGoalVerification
	}
	pins := map[ProposalMetric]bool{}
	for _, m := range g.Guardrails {
		pins[m] = true
	}
	for _, m := range p.Guardrails {
		if !pins[m] {
			return nil, ErrGoalVerification
		}
	}
	gf, err := GoalFingerprint(g)
	if err != nil || !validGoalPin(p.GoalPin) || (p.GoalPin != nil && (p.GoalPin.Version != g.Version || p.GoalPin.Fingerprint != gf)) {
		return nil, ErrGoalVerification
	}
	pf, err := ProposalFingerprint(p)
	if err != nil {
		return nil, ErrGoalVerification
	}
	return &GoalVerification{SchemaVersion: GoalVerificationSchemaVersion, WorkspaceID: workspace, EnvironmentID: environment, GoalID: g.ID, GoalVersion: g.Version, GoalFingerprint: gf, ProposalID: p.ID, ProposalVersion: p.Version, ProposalFingerprint: pf}, nil
}
