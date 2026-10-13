// Package gffimport translates go-feature-flag (gff) v0.7.x flag documents
// into the GoLab DSL (docs/技术方案.md §5.2) and provides the legacy-gff-v1
// compatibility evaluation mode.
//
// Import policy (§5.2, normative):
//
//   - Only the whitelisted gff fields are accepted: variations, disable,
//     targeting (rules with name/query/variation/percentage/disable),
//     defaultRule (variation or percentage), version, trackEvents,
//     bucketingKey, metadata. Any other field (experimentation,
//     scheduledRollout, progressiveRollout, ...) is a hard error — the
//     importer NEVER silently drops configuration (禁止静默丢字段).
//   - gff nikunjy query strings are convertible only in the simple binary
//     forms `<field> <op> <value>` and `<field> in [<v1> <v2> ...]`;
//     anything richer (and/or/parentheses/contains/matches...) is rejected.
//   - Percentage splits are copied numerically but REBASED onto the GoLab
//     fixed-bucket algorithm golab-fnv1a-10000-v1 (§6). The two algorithms
//     do not promise identical per-user assignments (两种算法不承诺同一用户
//     的分配相同), so every import carries that warning and migrating a
//     running experiment requires a new epoch (§5.2).
//
// The legacy mode (legacy.go) keeps evaluating old gff documents with the
// original gff algorithm via gff core, for compatibility and contrast tests
// only — GoLab native evaluation never calls gff's Evaluate (§2 设计决定).
package gffimport

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/model"
)

// DefaultSalt is the bucketing salt used when Options.Salt is empty. It
// domain-separates imported flags from natively-authored ones so that a
// later salt/algorithm/unit change follows the new-epoch rule (§6).
const DefaultSalt = "imported-gff-v1"

// Options controls one import.
type Options struct {
	// Workspace and Environment name the destination scope. The GoLab DSL
	// flag document itself does not embed them (bucketing coordinates are
	// taken from the published snapshot at evaluation time, §6); they are
	// used for diagnostics/warnings and audit context.
	Workspace   string
	Environment string
	// FlagKey overrides the flag key. When empty, the key is taken from the
	// single-key map wrapper of the gff multi-flag file format; a bare flag
	// object without Options.FlagKey is an error.
	FlagKey string
	// Salt overrides DefaultSalt for the bucketing configuration.
	Salt string
	// Epoch is reserved for experiment migration bookkeeping. A plain
	// imported flag binds no experiment, and non-experiment flags always
	// hash with epoch 0 (§6), so Epoch does not alter the produced
	// document; the control plane records it when the import feeds an
	// experiment migration (§5.2: 切换算法须新建实验 epoch).
	Epoch int64
}

// gff field whitelists (docs §5.2 — 导入器只支持明确列出的 gff 字段).
var (
	flagWhitelist = map[string]bool{
		"variations": true, "disable": true, "targeting": true,
		"defaultRule": true, "version": true, "trackEvents": true,
		"bucketingKey": true, "metadata": true,
	}
	ruleWhitelist = map[string]bool{
		"name": true, "query": true, "variation": true,
		"percentage": true, "disable": true,
	}
	defaultRuleWhitelist = map[string]bool{
		"variation": true, "percentage": true,
	}
)

// ImportFlag strictly converts ONE gff flag document (YAML or JSON bytes)
// into a validated GoLab model.Flag. The document is either a single flag
// object or a one-key map {"flag-name": {...}} (the gff multi-flag file
// format); a multi-key map is rejected because GoLab imports one flag at a
// time. The returned []string collects warnings (never nil-on-success
// semantics: warnings do not imply failure). The produced flag always
// passes model.ValidateFlag; if it would not, the error is returned instead
// (docs/技术方案.md §5.2: 严格解析并报告不支持字段；禁止静默丢字段).
func ImportFlag(doc []byte, opts Options) (*model.Flag, []string, error) {
	var top any
	if err := yaml.Unmarshal(doc, &top); err != nil {
		return nil, nil, fmt.Errorf("gffimport: cannot parse document: %w", err)
	}
	topMap, ok := top.(map[string]any)
	if !ok || len(topMap) == 0 {
		return nil, nil, fmt.Errorf("gffimport: document must be a non-empty YAML/JSON mapping")
	}

	key := opts.FlagKey
	body := topMap
	if !hasWhitelisted(topMap, flagWhitelist) {
		// No gff flag field at the top level → multi-flag file format.
		if len(topMap) > 1 {
			keys := make([]string, 0, len(topMap))
			for k := range topMap {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, nil, fmt.Errorf(
				"gffimport: document contains %d flags (%s): import one flag at a time",
				len(topMap), strings.Join(keys, ", "))
		}
		wrapperKey, inner := onlyEntry(topMap)
		innerMap, isMap := inner.(map[string]any)
		if !isMap || !hasWhitelisted(innerMap, flagWhitelist) {
			// Not a flag wrapper → a flag object made only of unsupported
			// fields; report them against the top level.
			return nil, nil, unsupportedFieldsError("", topMap, flagWhitelist)
		}
		body = innerMap
		if key == "" {
			key = wrapperKey
		}
	}
	if key == "" {
		return nil, nil, fmt.Errorf(
			"gffimport: no flag key: set Options.FlagKey or use the single-key map form of the gff file format")
	}
	return convertFlag(key, body, opts)
}

func convertFlag(key string, body map[string]any, opts Options) (*model.Flag, []string, error) {
	if err := checkWhitelist("", body, flagWhitelist); err != nil {
		return nil, nil, err
	}

	var warnings []string
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key,
		Variations:    map[string]any{},
	}

	// variations (mandatory in practice: ValidateFlag rejects an empty set).
	varType, err := convertVariations(body, flag)
	if err != nil {
		return nil, nil, err
	}
	flag.Type = varType

	// disable / version / trackEvents / metadata / bucketingKey.
	if v, ok := body["disable"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, nil, fmt.Errorf("gffimport: disable must be a bool, got %T", v)
		}
		flag.Disable = b
	}
	if v, ok := body["version"]; ok {
		s, err := stringifyScalar("version", v)
		if err != nil {
			return nil, nil, err
		}
		flag.Version = s
	}
	if v, ok := body["trackEvents"]; ok {
		if _, ok := v.(bool); !ok {
			return nil, nil, fmt.Errorf("gffimport: trackEvents must be a bool, got %T", v)
		}
		warnings = append(warnings, fmt.Sprintf(
			"gff trackEvents=%v accepted but not representable in GoLab DSL: exposure tracking is governed by the experiment binding (docs/技术方案.md §5.3, §8)", v))
	}
	if v, ok := body["metadata"]; ok {
		if _, ok := v.(map[string]any); !ok {
			return nil, nil, fmt.Errorf("gffimport: metadata must be a mapping, got %T", v)
		}
		warnings = append(warnings,
			"gff metadata accepted but dropped: the GoLab DSL flag document has no metadata field (docs/技术方案.md §5.2)")
	}
	bucketingKey := ""
	if v, ok := body["bucketingKey"]; ok {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, nil, fmt.Errorf("gffimport: bucketingKey must be a non-empty string, got %v", v)
		}
		bucketingKey = s
	}

	// defaultRule (required).
	hasPercentage := false
	drRaw, ok := body["defaultRule"]
	if !ok || drRaw == nil {
		return nil, nil, fmt.Errorf("gffimport: defaultRule is required")
	}
	drMap, ok := drRaw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("gffimport: defaultRule must be a mapping, got %T", drRaw)
	}
	if err := checkWhitelist("defaultRule", drMap, defaultRuleWhitelist); err != nil {
		return nil, nil, err
	}
	dr, pct, err := convertRule("defaultRule", drMap, defaultRuleWhitelist)
	if err != nil {
		return nil, nil, err
	}
	hasPercentage = hasPercentage || pct
	flag.DefaultRule = dr

	// targeting (optional list of rules).
	fieldSet := map[string]bool{}
	collectConstraintFields(dr, fieldSet)
	if v, ok := body["targeting"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return nil, nil, fmt.Errorf("gffimport: targeting must be a list of rules, got %T", v)
		}
		flag.Targeting = make([]model.Rule, 0, len(list))
		for i, item := range list {
			where := fmt.Sprintf("targeting[%d]", i)
			rm, ok := item.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("gffimport: %s must be a mapping, got %T", where, item)
			}
			if err := checkWhitelist(where, rm, ruleWhitelist); err != nil {
				return nil, nil, err
			}
			r, pct, err := convertRule(where, rm, ruleWhitelist)
			if err != nil {
				return nil, nil, err
			}
			if _, hasQuery := rm["query"]; hasQuery {
				warnQueryConverted := false
				for _, w := range warnings {
					if strings.Contains(w, "nikunjy") {
						warnQueryConverted = true
					}
				}
				if !warnQueryConverted {
					warnings = append(warnings,
						"gff nikunjy query expressions converted to GoLab constraints; matching of missing or differently-typed context fields may differ from gff")
				}
			}
			hasPercentage = hasPercentage || pct
			collectConstraintFields(r, fieldSet)
			flag.Targeting = append(flag.Targeting, *r)
		}
	}

	// Bucketing: required iff any percentage split exists (§5.2/§6). The
	// split values are copied 1:1 but the ALGORITHM changes: GoLab rebases
	// percentages onto golab-fnv1a-10000-v1 fixed buckets.
	if hasPercentage {
		unit := bucketingKey
		if unit == "" {
			unit = model.TargetingKeyField
		}
		salt := opts.Salt
		if salt == "" {
			salt = DefaultSalt
		}
		flag.Bucketing = &model.Bucketing{
			Algorithm: bucketing.AlgorithmID,
			Unit:      unit,
			Salt:      salt,
		}
		// "targetingKey" is implicitly registered (pkg/model/validate.go);
		// any other unit must appear in Fields.
		if unit != model.TargetingKeyField {
			fieldSet[unit] = true
		}
		warnings = append(warnings,
			"percentage rebased to GoLab algorithm; user assignments will differ from gff",
			"GoLab golab-fnv1a-10000-v1 fixed-bucket assignment does NOT reproduce gff assignments for the same users (docs/技术方案.md §2, §5.2: 两种算法不承诺同一用户的分配相同); migrating a running experiment requires a new epoch")
	}

	if len(fieldSet) > 0 {
		fields := make([]string, 0, len(fieldSet))
		for f := range fieldSet {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		flag.Fields = fields
	}

	// The importer must never produce an invalid GoLab flag.
	if err := model.ValidateFlag(flag, nil); err != nil {
		return nil, nil, fmt.Errorf("gffimport: imported flag %q is not valid GoLab DSL: %w", key, err)
	}
	return flag, warnings, nil
}

// convertVariations fills flag.Variations and infers the GoLab flag type:
// all bool → FlagBool, all string → FlagString, all numeric → FlagNumber,
// mixed / objects / arrays → FlagJSON. A null variation value is an error.
func convertVariations(body map[string]any, flag *model.Flag) (model.FlagType, error) {
	raw, ok := body["variations"]
	if !ok || raw == nil {
		return "", fmt.Errorf("gffimport: variations is required")
	}
	vm, ok := raw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("gffimport: variations must be a mapping, got %T", raw)
	}
	if len(vm) == 0 {
		return "", fmt.Errorf("gffimport: variations is empty")
	}
	kinds := map[string]bool{}
	for _, name := range sortedKeys(vm) {
		v := vm[name]
		if v == nil {
			return "", fmt.Errorf("gffimport: variation %q has a null value (gff and GoLab both reject null variations)", name)
		}
		flag.Variations[name] = v
		kinds[valueKind(v)] = true
	}
	switch {
	case len(kinds) == 1 && kinds["bool"]:
		return model.FlagBool, nil
	case len(kinds) == 1 && kinds["string"]:
		return model.FlagString, nil
	case len(kinds) == 1 && kinds["number"]:
		return model.FlagNumber, nil
	default:
		return model.FlagJSON, nil
	}
}

func valueKind(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case string:
		return "string"
	case int, int64, uint64, float64:
		return "number"
	default:
		return "json"
	}
}

// convertRule converts one gff rule map into a model.Rule. It reports
// whether the rule carries a percentage split. Exactly one of
// variation/percentage must be set.
func convertRule(where string, rm map[string]any, whitelist map[string]bool) (*model.Rule, bool, error) {
	r := &model.Rule{}
	if v, ok := rm["name"]; ok {
		s, err := stringifyScalar(where+".name", v)
		if err != nil {
			return nil, false, err
		}
		r.Name = s
	}
	if v, ok := rm["disable"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, false, fmt.Errorf("gffimport: %s.disable must be a bool, got %T", where, v)
		}
		r.Disable = b
	}
	variation, hasVariation := "", false
	if v, ok := rm["variation"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, false, fmt.Errorf("gffimport: %s.variation must be a string, got %T", where, v)
		}
		variation, hasVariation = s, true
	}
	var percentage map[string]float64
	hasPercentage := false
	if v, ok := rm["percentage"]; ok {
		pm, ok := v.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("gffimport: %s.percentage must be a mapping, got %T", where, v)
		}
		percentage = make(map[string]float64, len(pm))
		for name, pv := range pm {
			f, ok := toFloat(pv)
			if !ok {
				return nil, false, fmt.Errorf("gffimport: %s.percentage[%q] must be numeric, got %T", where, name, pv)
			}
			percentage[name] = f
		}
		hasPercentage = true
	}
	if hasVariation == hasPercentage {
		return nil, false, fmt.Errorf("gffimport: %s: exactly one of variation/percentage must be set", where)
	}
	if hasPercentage {
		r.Percentage = percentage
	} else {
		r.Variation = variation
	}
	// query is only meaningful on targeting rules (ignored by gff itself in
	// defaultRule, and rejected there by the whitelist).
	if v, ok := rm["query"]; ok {
		if _, allowed := whitelist["query"]; !allowed {
			return nil, false, fmt.Errorf("gffimport: %s: query is not supported", where)
		}
		qs, ok := v.(string)
		if !ok {
			return nil, false, fmt.Errorf("gffimport: %s.query must be a string, got %T", where, v)
		}
		constraints, err := parseQuery(qs)
		if err != nil {
			return nil, false, fmt.Errorf("gffimport: %s: %w", where, err)
		}
		r.Constraints = constraints
	}
	return r, hasPercentage, nil
}

func collectConstraintFields(r *model.Rule, fields map[string]bool) {
	for _, c := range r.Constraints {
		fields[c.Field] = true
	}
}

// --- gff nikunjy query subset ---------------------------------------------

// queryOps maps the gff/nikunjy operator spellings (word and symbolic
// forms) onto the closed GoLab operator set (§5.2).
var queryOps = map[string]string{
	"eq": model.OpEq, "==": model.OpEq,
	"ne": model.OpNe, "!=": model.OpNe,
	"gt": model.OpGt, ">": model.OpGt,
	"gte": model.OpGte, ">=": model.OpGte,
	"lt": model.OpLt, "<": model.OpLt,
	"lte": model.OpLte, "<=": model.OpLte,
}

type tokenKind int

const (
	tokWord tokenKind = iota
	tokStr
	tokOpen
	tokClose
)

type qToken struct {
	kind tokenKind
	text string
}

// parseQuery converts the supported subset of gff query expressions:
// `<field> <op> <value>` (ops eq/ne/gt/gte/lt/lte in word or symbolic form,
// value a quoted string, number or bool literal) and
// `<field> in [<v1> <v2> ...]`. Everything else — and/or, parentheses,
// contains, matches, ... — is rejected with the raw string quoted, because
// silently approximating an unconverted audience would change who is
// targeted (docs/技术方案.md §5.2: 禁止静默丢字段).
func parseQuery(raw string) ([]model.Constraint, error) {
	q := strings.TrimSpace(raw)
	if q == "" {
		return nil, nil
	}
	unsupported := fmt.Errorf(
		"gffimport: unsupported query expression: %q (only `<field> <op> <value>` with eq/ne/gt/gte/lt/lte and `<field> in [<v1> ...]` are convertible)", raw)
	toks, err := tokenizeQuery(q)
	if err != nil {
		return nil, unsupported
	}
	if len(toks) < 3 || toks[0].kind != tokWord || !isFieldName(toks[0].text) {
		return nil, unsupported
	}
	field := toks[0].text

	// <field> in [ v1 v2 ... ]
	if toks[1].kind == tokWord && strings.EqualFold(toks[1].text, "in") {
		if len(toks) < 4 || toks[2].kind != tokOpen || toks[len(toks)-1].kind != tokClose {
			return nil, unsupported
		}
		var values []any
		for _, t := range toks[3 : len(toks)-1] {
			if t.kind == tokOpen || t.kind == tokClose {
				return nil, unsupported
			}
			v, err := tokenValue(t)
			if err != nil {
				return nil, unsupported
			}
			values = append(values, v)
		}
		if len(values) == 0 {
			return nil, unsupported
		}
		return []model.Constraint{{Field: field, Op: model.OpIn, Values: values}}, nil
	}

	// <field> <op> <value>
	if len(toks) != 3 || toks[1].kind != tokWord {
		return nil, unsupported
	}
	op, ok := queryOps[strings.ToLower(toks[1].text)]
	if !ok {
		return nil, unsupported
	}
	v, err := tokenValue(toks[2])
	if err != nil {
		return nil, unsupported
	}
	return []model.Constraint{{Field: field, Op: op, Value: v}}, nil
}

func tokenizeQuery(q string) ([]qToken, error) {
	var toks []qToken
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '[':
			toks = append(toks, qToken{kind: tokOpen})
			i++
		case c == ']':
			toks = append(toks, qToken{kind: tokClose})
			i++
		case c == '\'' || c == '"':
			quote := c
			i++
			var sb strings.Builder
			closed := false
			for i < len(q) {
				if q[i] == '\\' && i+1 < len(q) {
					sb.WriteByte(q[i+1])
					i += 2
					continue
				}
				if q[i] == quote {
					closed = true
					i++
					break
				}
				sb.WriteByte(q[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("gffimport: unterminated quote in query")
			}
			toks = append(toks, qToken{kind: tokStr, text: sb.String()})
		default:
			start := i
			for i < len(q) && !strings.ContainsRune(" \t\n\r[]\"'", rune(q[i])) {
				i++
			}
			toks = append(toks, qToken{kind: tokWord, text: q[start:i]})
		}
	}
	return toks, nil
}

// isFieldName accepts plain and dotted identifiers ("tenant", "user.email").
func isFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == '.' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// tokenValue decodes a value token: quoted strings stay strings; true/false
// become bools; numbers become int64 when integral (so `age gt 30` imports
// as 30, not 30.0) and float64 otherwise.
func tokenValue(t qToken) (any, error) {
	if t.kind == tokStr {
		return t.text, nil
	}
	if t.kind != tokWord {
		return nil, fmt.Errorf("gffimport: not a value token")
	}
	switch t.text {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	f, err := strconv.ParseFloat(t.text, 64)
	if err != nil {
		return nil, err
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return int64(f), nil
	}
	return f, nil
}

// --- generic helpers -------------------------------------------------------

func hasWhitelisted(m map[string]any, whitelist map[string]bool) bool {
	for k := range m {
		if whitelist[k] {
			return true
		}
	}
	return false
}

// checkWhitelist enforces the strict field policy: unknown gff fields are a
// hard error naming every offender (§5.2: 严格解析并报告不支持字段).
func checkWhitelist(where string, m map[string]any, whitelist map[string]bool) error {
	var unknown []string
	for k := range m {
		if !whitelist[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	prefix := "gffimport: "
	if where != "" {
		prefix = fmt.Sprintf("gffimport: %s: ", where)
	}
	return fmt.Errorf("%sunsupported gff field(s): %s — the GoLab importer accepts only: %s (docs/技术方案.md §5.2)",
		prefix, strings.Join(unknown, ", "), strings.Join(sortedWhitelist(whitelist), ", "))
}

// unsupportedFieldsError reports a top level that is not a recognizable gff
// flag object at all (only non-whitelisted fields).
func unsupportedFieldsError(where string, m map[string]any, whitelist map[string]bool) error {
	return checkWhitelist(where, m, whitelist)
}

func sortedWhitelist(whitelist map[string]bool) []string {
	out := make([]string, 0, len(whitelist))
	for k := range whitelist {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func onlyEntry(m map[string]any) (string, any) {
	for k, v := range m {
		return k, v
	}
	return "", nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stringifyScalar renders gff scalar fields that GoLab stores as strings
// (version, rule name). YAML/JSON may encode them numerically.
func stringifyScalar(where string, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case uint64:
		return strconv.FormatUint(t, 10), nil
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10), nil
		}
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("gffimport: %s must be a scalar, got %T", where, v)
	}
}

// toFloat coerces percentage values (YAML int, JSON float64, ...) to float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}
