package growth

import (
	"bytes"
	"encoding/json/v2"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestInitProposal(t *testing.T) {
	for _, version := range []int64{1, 1<<53 + 1, math.MaxInt64} {
		g, _ := goalFixture()
		g.Version = version
		g.PrimaryMetric.Ref.Version = version
		g.PrimaryMetric.SourcePath = strings.Replace(g.PrimaryMetric.SourcePath, "/versions/1", "/versions/"+strconv.FormatInt(version, 10), 1)
		extra := g.Guardrails[0]
		extra.Ref.Key = "latency"
		extra.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/latency/versions/1"
		g.Guardrails = append(g.Guardrails, extra)
		p, err := InitProposal(g, "new-proposal", version, "ws1", "env1")
		if err != nil {
			t.Fatal(err)
		}
		goalJSON, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		decodedGoal, err := DecodeGoal(bytes.NewReader(goalJSON))
		if err != nil {
			t.Fatal("goal round trip", err)
		}
		fp, _ := GoalFingerprint(g)
		expected := Proposal{SchemaVersion: ProposalSchemaVersion, ID: "new-proposal", Version: version, GoalID: g.ID, GoalPin: &GoalPin{Version: version, Fingerprint: fp}, WorkspaceID: "ws1", EnvironmentID: "env1", PrimaryMetric: g.PrimaryMetric, Guardrails: g.Guardrails}
		if !reflect.DeepEqual(*p, expected) {
			t.Fatal("unexpected claims or references", p)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeProposal(bytes.NewReader(data))
		roundTrip, marshalErr := json.Marshal(decoded)
		if err != nil || marshalErr != nil || !bytes.Equal(data, roundTrip) {
			t.Fatal("proposal round trip", err)
		}
		verification, err := VerifyGoal(g, *decoded, "ws1", "env1")
		if err != nil || verification.PublicationAuthorized {
			t.Fatal("verification", err)
		}
		goalFP, err := GoalFingerprint(*decodedGoal)
		if err != nil || goalFP != fp || goalFP != verification.GoalFingerprint {
			t.Fatal("goal fingerprint round trip", err)
		}
		before, err := ProposalFingerprint(*p)
		after, err2 := ProposalFingerprint(*decoded)
		if err != nil || err2 != nil || before != after || after != verification.ProposalFingerprint {
			t.Fatal("fingerprint round trip")
		}
		for _, evidence := range []*Envelope{nil, {Status: Ready}} {
			if ReviewProposal(*p, evidence).State != ProposalDraft {
				t.Fatal("initializer claimed readiness")
			}
			if prepared, err := CompileCreateRequest(*p, evidence, PreparationInputs{}); err == nil || prepared != nil {
				t.Fatal("preparation allowed")
			}
		}
		changed := g
		changed.Version--
		if _, err := VerifyGoal(changed, *p, "ws1", "env1"); err == nil {
			t.Fatal("version pin ignored")
		}
		changed = g
		changed.Objective += " changed"
		if _, err := VerifyGoal(changed, *p, "ws1", "env1"); err == nil {
			t.Fatal("content pin ignored")
		}
		p.Guardrails[0].Ref.Key = "changed"
		if g.Guardrails[0].Ref.Key == "changed" {
			t.Fatal("proposal aliases goal")
		}
		g.Guardrails[1].Ref.Key = "other"
		if p.Guardrails[1].Ref.Key == "other" {
			t.Fatal("goal aliases proposal")
		}
	}
}

func TestInitProposalRejectsInvalidInput(t *testing.T) {
	for name, mutate := range map[string]func(*GrowthGoal){
		"schema":     func(g *GrowthGoal) { g.SchemaVersion = "wrong" },
		"version":    func(g *GrowthGoal) { g.Version = 0 },
		"owner":      func(g *GrowthGoal) { g.Owner = "" },
		"objective":  func(g *GrowthGoal) { g.Objective = "" },
		"oversize":   func(g *GrowthGoal) { g.Objective = strings.Repeat("x", MaxGoalBytes) },
		"window":     func(g *GrowthGoal) { g.BaselineWindow.End = g.BaselineWindow.Start },
		"metric":     func(g *GrowthGoal) { g.PrimaryMetric.Ref.DefinitionHash = "bad" },
		"path":       func(g *GrowthGoal) { g.PrimaryMetric.SourcePath += "?bad" },
		"guardrails": func(g *GrowthGoal) { g.Guardrails = nil },
		"duplicate":  func(g *GrowthGoal) { g.Guardrails = append(g.Guardrails, g.PrimaryMetric) },
	} {
		t.Run(name, func(t *testing.T) {
			g, _ := goalFixture()
			mutate(&g)
			if p, err := InitProposal(g, "p", 1, "ws1", "env1"); err != ErrProposalInitialization || p != nil {
				t.Fatal("accepted invalid goal", p, err)
			}
		})
	}
	g, _ := goalFixture()
	for _, input := range []struct {
		id      string
		version int64
		ws, env string
	}{
		{"", 1, "ws1", "env1"}, {"../SECRET", 1, "ws1", "env1"}, {strings.Repeat("x", 300), 1, "ws1", "env1"},
		{"p", 0, "ws1", "env1"}, {"p", -1, "ws1", "env1"}, {"p", 1, "", "env1"}, {"p", 1, "ws1", ""},
		{"p", 1, "other", "env1"}, {"p", 1, "ws1", "other"}, {"p", 1, "../ws1", "env1"},
	} {
		if p, err := InitProposal(g, input.id, input.version, input.ws, input.env); err != ErrProposalInitialization || p != nil {
			t.Fatal("accepted invalid operator input", p, err)
		}
	}
}
