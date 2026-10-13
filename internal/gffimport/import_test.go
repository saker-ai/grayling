package gffimport

import (
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/pkg/bucketing"
	"github.com/saker-ai/grayling/pkg/model"
)

// realisticYAML exercises variations, defaultRule percentage, targeting with
// query + percentage, bucketingKey, version, disable, trackEvents, metadata.
const realisticYAML = `
variations:
  control: false
  candidate: true
version: "17"
disable: false
trackEvents: true
bucketingKey: user_id
metadata:
  description: checkout router experiment
defaultRule:
  percentage:
    control: 70
    candidate: 30
targeting:
  - name: acme
    query: tenant eq "acme"
    variation: candidate
  - name: beta-ramp
    query: beta == true
    percentage:
      control: 50
      candidate: 50
  - name: eu-orgs
    query: org in ["globex" "initech"]
    disable: false
    variation: candidate
`

func TestImportRealisticFlag(t *testing.T) {
	f, warnings, err := ImportFlag([]byte(realisticYAML), Options{
		Workspace: "ws", Environment: "prod", FlagKey: "checkout-router",
	})
	if err != nil {
		t.Fatalf("ImportFlag: %v", err)
	}
	// The importer must never produce an invalid GoLab flag.
	if err := model.ValidateFlag(f, nil); err != nil {
		t.Fatalf("imported flag fails model.ValidateFlag: %v", err)
	}
	if f.SchemaVersion != model.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", f.SchemaVersion, model.SchemaVersion)
	}
	if f.Key != "checkout-router" {
		t.Errorf("Key = %q", f.Key)
	}
	if f.Type != model.FlagBool {
		t.Errorf("Type = %q, want bool", f.Type)
	}
	if f.Version != "17" || f.Disable {
		t.Errorf("Version/Disable = %q/%v", f.Version, f.Disable)
	}
	// Bucketing: unit from gff bucketingKey, GoLab algorithm, default salt.
	if f.Bucketing == nil {
		t.Fatal("Bucketing is nil despite percentage rules")
	}
	if f.Bucketing.Algorithm != bucketing.AlgorithmID {
		t.Errorf("Algorithm = %q", f.Bucketing.Algorithm)
	}
	if f.Bucketing.Unit != "user_id" || f.Bucketing.Salt != DefaultSalt {
		t.Errorf("Unit/Salt = %q/%q", f.Bucketing.Unit, f.Bucketing.Salt)
	}
	// Fields = constraint fields ∪ bucketing unit, deduped and sorted.
	wantFields := []string{"beta", "org", "tenant", "user_id"}
	if !reflect.DeepEqual(f.Fields, wantFields) {
		t.Errorf("Fields = %v, want %v", f.Fields, wantFields)
	}
	// defaultRule percentage copied 1:1.
	if got := f.DefaultRule.Percentage; got["control"] != 70 || got["candidate"] != 30 {
		t.Errorf("defaultRule.Percentage = %v", got)
	}
	if len(f.Targeting) != 3 {
		t.Fatalf("Targeting has %d rules, want 3", len(f.Targeting))
	}
	acme := f.Targeting[0]
	if acme.Name != "acme" || acme.Variation != "candidate" || len(acme.Constraints) != 1 {
		t.Fatalf("targeting[0] = %+v", acme)
	}
	c := acme.Constraints[0]
	if c.Field != "tenant" || c.Op != model.OpEq || c.Value != "acme" {
		t.Errorf("constraint = %+v, want tenant eq acme", c)
	}
	beta := f.Targeting[1].Constraints[0]
	if beta.Field != "beta" || beta.Op != model.OpEq || beta.Value != true {
		t.Errorf("constraint = %+v, want beta eq true (symbolic == form)", beta)
	}
	if pct := f.Targeting[1].Percentage; pct["control"] != 50 || pct["candidate"] != 50 {
		t.Errorf("targeting[1].Percentage = %v", pct)
	}
	org := f.Targeting[2].Constraints[0]
	if org.Field != "org" || org.Op != model.OpIn ||
		!reflect.DeepEqual(org.Values, []any{"globex", "initech"}) {
		t.Errorf("constraint = %+v, want org in [globex initech]", org)
	}
	// Mandatory algorithm-divergence warning plus rebase/trackEvents/metadata.
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"percentage rebased", "不承诺同一用户", "trackEvents", "metadata"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q; got:\n%s", want, joined)
		}
	}
}

func TestImportUnsupportedFields(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // field name that must appear in the error
	}{
		{
			name: "flag-level experimentation",
			doc: `
variations: {a: true, b: false}
defaultRule: {variation: a}
experimentation:
  start: 2024-01-01T00:00:00Z
  end: 2024-06-01T00:00:00Z
`,
			want: "experimentation",
		},
		{
			name: "flag-level scheduledRollout",
			doc: `
variations: {a: true, b: false}
defaultRule: {variation: a}
scheduledRollout:
  - date: 2024-01-01T00:00:00Z
    disable: true
`,
			want: "scheduledRollout",
		},
		{
			name: "rule-level progressiveRollout",
			doc: `
variations: {a: true, b: false}
defaultRule: {variation: a}
targeting:
  - name: ramp
    progressiveRollout:
      initial: {variation: a, percentage: 0, date: 2024-01-01T00:00:00Z}
      end: {variation: b, percentage: 100, date: 2024-02-01T00:00:00Z}
`,
			want: "progressiveRollout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ImportFlag([]byte(tc.doc), Options{FlagKey: "f1"})
			if err == nil {
				t.Fatal("want error for unsupported field, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "unsupported gff field") {
				t.Errorf("error %q is not the unsupported-field error", err)
			}
		})
	}
}

func TestImportComplexQueryRejected(t *testing.T) {
	doc := `
variations: {a: true, b: false}
defaultRule: {variation: a}
targeting:
  - name: complex
    query: tenant eq "acme" and age gt 30
    variation: b
`
	_, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1"})
	if err == nil {
		t.Fatal("want error for complex query, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported query expression") {
		t.Errorf("error %q is not the unsupported-query error", err)
	}
	// The raw query must be quoted in the error (%q form).
	if !strings.Contains(err.Error(), `tenant eq \"acme\" and age gt 30`) {
		t.Errorf("error %q does not quote the raw query", err)
	}

	for _, q := range []string{
		`name contains "acme"`,
		`(tenant eq "acme")`,
		`email matches ".*@acme.com"`,
		`a eq "x" or b eq "y"`,
		`tenant not_in ["acme"]`,
	} {
		doc := "variations: {a: true}\ndefaultRule: {variation: a}\ntargeting:\n  - query: '" + q + "'\n    variation: a\n"
		if _, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1"}); err == nil ||
			!strings.Contains(err.Error(), "unsupported query expression") {
			t.Errorf("query %q: want unsupported-query error, got %v", q, err)
		}
	}
}

func TestImportQueryOperatorForms(t *testing.T) {
	cases := []struct {
		query string
		op    string
		value any
	}{
		{`age gt 30`, model.OpGt, int64(30)},
		{`age > 30`, model.OpGt, int64(30)},
		{`age gte 30.5`, model.OpGte, 30.5},
		{`age >= 30`, model.OpGte, int64(30)},
		{`age lt 30`, model.OpLt, int64(30)},
		{`age < 30`, model.OpLt, int64(30)},
		{`age lte 30`, model.OpLte, int64(30)},
		{`age <= 30`, model.OpLte, int64(30)},
		{`tenant ne "acme"`, model.OpNe, "acme"},
		{`tenant != "acme"`, model.OpNe, "acme"},
		{`tenant eq 'acme'`, model.OpEq, "acme"},
		{`tenant == "acme"`, model.OpEq, "acme"},
		{`premium eq true`, model.OpEq, true},
		{`premium ne false`, model.OpNe, false},
	}
	for _, tc := range cases {
		// Escape the query into a YAML double-quoted scalar so queries may
		// freely contain single and double quotes.
		doc := "variations: {a: true, b: false}\ndefaultRule: {variation: a}\n" +
			"targeting:\n  - query: \"" + strings.ReplaceAll(tc.query, `"`, `\"`) + "\"\n    variation: b\n"
		f, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1"})
		if err != nil {
			t.Fatalf("query %q: %v", tc.query, err)
		}
		c := f.Targeting[0].Constraints[0]
		if c.Op != tc.op || !reflect.DeepEqual(c.Value, tc.value) {
			t.Errorf("query %q → %+v, want op %q value %v", tc.query, c, tc.op, tc.value)
		}
	}
}

func TestImportMultiFlagFile(t *testing.T) {
	two := `
flag-one:
  variations: {a: true}
  defaultRule: {variation: a}
flag-two:
  variations: {a: true}
  defaultRule: {variation: a}
`
	_, _, err := ImportFlag([]byte(two), Options{})
	if err == nil {
		t.Fatal("want error for multi-flag document")
	}
	if !strings.Contains(err.Error(), "one flag at a time") ||
		!strings.Contains(err.Error(), "flag-one") || !strings.Contains(err.Error(), "flag-two") {
		t.Errorf("error %q should mention both keys and one-flag-at-a-time", err)
	}

	one := `
only-flag:
  variations: {a: true}
  defaultRule: {variation: a}
`
	f, _, err := ImportFlag([]byte(one), Options{})
	if err != nil {
		t.Fatalf("single-key wrapper: %v", err)
	}
	if f.Key != "only-flag" {
		t.Errorf("Key = %q, want the map key only-flag", f.Key)
	}
	// Options.FlagKey overrides the map key.
	f, _, err = ImportFlag([]byte(one), Options{FlagKey: "override"})
	if err != nil || f.Key != "override" {
		t.Errorf("override: Key = %q, err = %v", f.Key, err)
	}
}

func TestImportTypeInference(t *testing.T) {
	cases := []struct {
		name       string
		variations string
		want       model.FlagType
	}{
		{"bool", "a: false\nb: true", model.FlagBool},
		{"string", "a: \"x\"\nb: 'y'", model.FlagString},
		{"number-int", "a: 1\nb: 2", model.FlagNumber},
		{"number-float", "a: 1.5\nb: 2.25", model.FlagNumber},
		{"mixed-bool-string", "a: true\nb: \"x\"", model.FlagJSON},
		{"mixed-number-string", "a: 1\nb: \"x\"", model.FlagJSON},
		{"object", "a: {k: v}\nb: {k: 2}", model.FlagJSON},
		{"array", "a: [1, 2]\nb: []", model.FlagJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "variations:\n  " + strings.ReplaceAll(tc.variations, "\n", "\n  ") +
				"\ndefaultRule: {variation: a}\n"
			f, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1"})
			if err != nil {
				t.Fatalf("%v", err)
			}
			if f.Type != tc.want {
				t.Errorf("Type = %q, want %q", f.Type, tc.want)
			}
		})
	}

	// Null variation values are rejected.
	doc := "variations: {a: true, b: null}\ndefaultRule: {variation: a}\n"
	if _, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1"}); err == nil ||
		!strings.Contains(err.Error(), "null") {
		t.Errorf("null variation: want error, got %v", err)
	}
}

func TestImportPercentageAndBucketingRules(t *testing.T) {
	// No percentage anywhere → no bucketing block is produced.
	doc := "variations: {a: true, b: false}\ndefaultRule: {variation: a}\n"
	f, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1", Salt: "ignored"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if f.Bucketing != nil {
		t.Errorf("Bucketing = %+v, want nil without percentages", f.Bucketing)
	}

	// Percentages that do not sum to 100 must fail model.ValidateFlag inside
	// the importer (bucketing.NewSplit rejects them).
	doc = "variations: {a: true, b: false}\ndefaultRule: {percentage: {a: 60, b: 30}}\n"
	if _, _, err := ImportFlag([]byte(doc), Options{FlagKey: "f1"}); err == nil {
		t.Error("want error for percentages summing to 90")
	}

	// Salt override.
	doc = "variations: {a: true, b: false}\ndefaultRule: {percentage: {a: 50, b: 50}}\n"
	f, _, err = ImportFlag([]byte(doc), Options{FlagKey: "f1", Salt: "custom-salt"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if f.Bucketing.Salt != "custom-salt" || f.Bucketing.Unit != model.TargetingKeyField {
		t.Errorf("Bucketing = %+v", f.Bucketing)
	}
	// targetingKey is implicit and must NOT be added to Fields.
	if len(f.Fields) != 0 {
		t.Errorf("Fields = %v, want empty (targetingKey is implicit)", f.Fields)
	}
}

func TestImportRoundTripYAMLAndJSON(t *testing.T) {
	yamlDoc := `
checkout-router:
  variations:
    control: false
    candidate: true
  version: "3"
  bucketingKey: user_id
  defaultRule:
    percentage:
      control: 70
      candidate: 30
  targeting:
    - name: acme
      query: tenant eq "acme"
      variation: candidate
    - name: ramp
      query: age gte 30
      percentage:
        control: 50
        candidate: 50
`
	jsonDoc := []byte(`{
  "checkout-router": {
    "variations": {"control": false, "candidate": true},
    "version": "3",
    "bucketingKey": "user_id",
    "defaultRule": {"percentage": {"control": 70, "candidate": 30}},
    "targeting": [
      {"name": "acme", "query": "tenant eq \"acme\"", "variation": "candidate"},
      {"name": "ramp", "query": "age gte 30", "percentage": {"control": 50, "candidate": 50}}
    ]
  }
}`)
	opts := Options{Workspace: "ws", Environment: "prod"}
	fy, wy, err := ImportFlag([]byte(yamlDoc), opts)
	if err != nil {
		t.Fatalf("yaml: %v", err)
	}
	fj, wj, err := ImportFlag(jsonDoc, opts)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if !reflect.DeepEqual(fy, fj) {
		t.Errorf("YAML and JSON imports differ:\n%+v\n%+v", fy, fj)
	}
	if !reflect.DeepEqual(wy, wj) {
		t.Errorf("warnings differ:\n%v\n%v", wy, wj)
	}
	if err := model.ValidateFlag(fj, nil); err != nil {
		t.Fatalf("json import fails validation: %v", err)
	}
	if fj.Key != "checkout-router" {
		t.Errorf("Key = %q", fj.Key)
	}
	if want := []string{"age", "tenant", "user_id"}; !reflect.DeepEqual(fj.Fields, want) {
		t.Errorf("Fields = %v, want %v", fj.Fields, want)
	}
}

func TestImportRejectsBadDocuments(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		opts Options
	}{
		{"not a mapping", "- a\n- b\n", Options{FlagKey: "f"}},
		{"empty", "", Options{FlagKey: "f"}},
		{"no key available", "variations: {a: true}\ndefaultRule: {variation: a}\n", Options{}},
		{"missing defaultRule", "variations: {a: true}\n", Options{FlagKey: "f"}},
		{"rule with both variation and percentage", `
variations: {a: true, b: false}
defaultRule: {variation: a}
targeting:
  - variation: b
    percentage: {a: 50, b: 50}
`, Options{FlagKey: "f"}},
		{"defaultRule with query", `
variations: {a: true}
defaultRule: {variation: a, query: tenant eq "acme"}
`, Options{FlagKey: "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ImportFlag([]byte(tc.doc), tc.opts); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}
