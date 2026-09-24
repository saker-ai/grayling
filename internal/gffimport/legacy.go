package gffimport

import (
	"fmt"

	"gopkg.in/yaml.v3"

	gffeval "github.com/thomaspoignant/go-feature-flag/modules/core/evaluation"
	"github.com/thomaspoignant/go-feature-flag/modules/core/ffcontext"
	gffflag "github.com/thomaspoignant/go-feature-flag/modules/core/flag"
	gffmodel "github.com/thomaspoignant/go-feature-flag/modules/core/model"
)

// LegacyResult is the normalized outcome of one legacy-gff-v1 evaluation.
// It mirrors the fields of gff's model.VariationResult that GoLab surfaces
// (docs/技术方案.md §5.2: 可提供 legacy-gff-v1 兼容模式继续按 gff 原算法求值
// 旧实验). Reason/ErrorCode carry the gff (OpenFeature-aligned) spellings,
// NOT the GoLab native ones.
type LegacyResult struct {
	// Value is the resolved variation value, or the caller default on any
	// failure. Failed results must never be interpreted as an assignment.
	Value any
	// VariationType is the gff variation name ("SdkDefault" on failures).
	VariationType string
	// Reason is the gff resolution reason (SPLIT, TARGETING_MATCH, ...).
	Reason string
	// ErrorCode is empty on success (FLAG_NOT_FOUND, TYPE_MISMATCH, ...).
	ErrorCode string
	// Failed marks an evaluation that fell back to a default.
	Failed bool
	// Details carries the gff error message, if any.
	Details string
}

// LegacyEvaluator evaluates pre-import gff flag documents with the ORIGINAL
// gff algorithm (gff core v0.7.2). It exists for the legacy-gff-v1
// compatibility mode and for contrast tests against the GoLab native
// fixed-bucket evaluator; the two algorithms do NOT promise identical
// per-user assignments (§2 设计决定, §5.2).
type LegacyEvaluator struct {
	flags map[string]*gffflag.InternalFlag
}

// NewLegacyEvaluator parses gff flag documents (YAML or JSON bytes; gff
// structs carry yaml tags and YAML is a JSON superset) keyed by flag name.
// Every document must be a single, valid gff flag object (gff's own
// IsValid rules apply: non-empty same-typed variations, a default rule,
// valid rules). An empty map is rejected.
func NewLegacyEvaluator(docs map[string][]byte) (*LegacyEvaluator, error) {
	if len(docs) == 0 {
		return nil, fmt.Errorf("gffimport: legacy evaluator requires at least one flag document")
	}
	flags := make(map[string]*gffflag.InternalFlag, len(docs))
	for key, doc := range docs {
		var f gffflag.InternalFlag
		if err := yaml.Unmarshal(doc, &f); err != nil {
			return nil, fmt.Errorf("gffimport: legacy flag %q: %w", key, err)
		}
		if err := f.IsValid(); err != nil {
			return nil, fmt.Errorf("gffimport: legacy flag %q is not a valid gff flag: %w", key, err)
		}
		flags[key] = &f
	}
	return &LegacyEvaluator{flags: flags}, nil
}

// BoolVariation evaluates a bool flag with gff semantics. On any failure
// (unknown key, type mismatch, missing bucketing key) it returns the caller
// default and a Failed LegacyResult.
func (l *LegacyEvaluator) BoolVariation(key, targetingKey string, attrs map[string]any, def bool) (bool, LegacyResult) {
	return evaluateLegacy[bool](l, key, targetingKey, attrs, "bool", def)
}

// StringVariation evaluates a string flag with gff semantics.
func (l *LegacyEvaluator) StringVariation(key, targetingKey string, attrs map[string]any, def string) (string, LegacyResult) {
	return evaluateLegacy[string](l, key, targetingKey, attrs, "string", def)
}

// NumberVariation evaluates a numeric flag with gff semantics. Note the gff
// quirk that YAML integer variation values decode as Go int and therefore
// TYPE_MISMATCH against float64 — author numeric legacy docs with explicit
// floats (e.g. 100.0) when calling this method.
func (l *LegacyEvaluator) NumberVariation(key, targetingKey string, attrs map[string]any, def float64) (float64, LegacyResult) {
	return evaluateLegacy[float64](l, key, targetingKey, attrs, "double", def)
}

// JSONVariation evaluates a flag of arbitrary JSON shape with gff semantics.
func (l *LegacyEvaluator) JSONVariation(key, targetingKey string, attrs map[string]any, def any) (any, LegacyResult) {
	v, res := evaluateLegacy[any](l, key, targetingKey, attrs, "interface{}", def)
	return v, res
}

// evaluateLegacy wraps gff's generic evaluation.Evaluate. gff returns the
// SDK default inside the result on every failure path (including type
// mismatch, where it also returns a non-nil error), so the error is folded
// into the LegacyResult instead of being propagated separately.
func evaluateLegacy[T gffmodel.JSONType](l *LegacyEvaluator, key, targetingKey string,
	attrs map[string]any, expectedType string, def T) (T, LegacyResult) {

	f, ok := l.flags[key]
	if !ok {
		return def, LegacyResult{
			Value:         def,
			VariationType: gffflag.VariationSDKDefault,
			Reason:        gffflag.ReasonError,
			ErrorCode:     gffflag.ErrorCodeFlagNotFound,
			Failed:        true,
			Details:       fmt.Sprintf("flag %q not found in the legacy-gff-v1 set", key),
		}
	}

	builder := ffcontext.NewEvaluationContextBuilder(targetingKey)
	for name, value := range attrs {
		builder = builder.AddCustom(name, value)
	}
	ctx := builder.Build()

	// flag.Context carries the SDK default gff serves on internal errors
	// (disabled flags, missing bucketing key, misconfiguration).
	res, _ := gffeval.Evaluate[T](f, key, ctx, gffflag.Context{DefaultSdkValue: def}, expectedType, def)
	return res.Value, LegacyResult{
		Value:         res.Value,
		VariationType: res.VariationType,
		Reason:        res.Reason,
		ErrorCode:     res.ErrorCode,
		Failed:        res.Failed,
		Details:       res.ErrorDetails,
	}
}
