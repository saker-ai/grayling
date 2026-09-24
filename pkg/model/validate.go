package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/godeps/golab/pkg/bucketing"
)

// KeyPattern constrains flag/segment keys so they can be embedded in gonacos
// dataIds without escaping (§5.1: user strings are never spliced raw into
// substrate IDs — validated keys only).
var KeyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Limits bounding document cost (§5.2, §11).
const (
	MaxVariations      = 64
	MaxTargetingRules  = 64
	MaxConstraints     = 32
	MaxSegmentsPerRule = 16
	MaxSegmentDepth    = 8
	MaxRegexLen        = 512
	MaxSetValues       = 512
	MaxSaltLen         = 128
	MaxFields          = 64
)

// HashBytes returns the SHA-256 hex digest used for manifest entries and
// snapshot integrity.
func HashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// CanonicalJSON renders v as JSON with stable key ordering (Go's
// encoding/json sorts map keys) for hashing. Struct field order is fixed by
// the type definition; omitempty keeps the encoding minimal.
func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// HashFlag is the manifest hash of one flag document.
func HashFlag(f *Flag) (string, error) {
	b, err := CanonicalJSON(f)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// HashSegment is the manifest hash of one segment document.
func HashSegment(s *Segment) (string, error) {
	b, err := CanonicalJSON(s)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// ValidationError aggregates all problems found in one document so the
// admin API can return them in a single response.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "validation failed: " + strings.Join(e.Problems, "; ")
}

func (e *ValidationError) add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

// ValidateFlag checks one flag in the context of the segments it may
// reference. segments may be nil when the flag references none.
func ValidateFlag(f *Flag, segments map[string]*Segment) error {
	verr := &ValidationError{}
	if f == nil {
		return &ValidationError{Problems: []string{"flag is nil"}}
	}
	if f.SchemaVersion != SchemaVersion {
		verr.add("schemaVersion = %d, want %d", f.SchemaVersion, SchemaVersion)
	}
	if !KeyPattern.MatchString(f.Key) {
		verr.add("key %q does not match %s", f.Key, KeyPattern)
	}
	switch f.Type {
	case FlagBool, FlagString, FlagNumber, FlagJSON:
	default:
		verr.add("type %q not one of bool|string|number|json", f.Type)
	}

	registered := map[string]bool{TargetingKeyField: true}
	for _, field := range f.Fields {
		if field == "" {
			verr.add("fields contains empty entry")
			continue
		}
		registered[field] = true
	}
	if len(f.Fields) > MaxFields {
		verr.add("fields has %d entries, max %d", len(f.Fields), MaxFields)
	}

	if len(f.Variations) == 0 {
		verr.add("variations is empty")
	}
	if len(f.Variations) > MaxVariations {
		verr.add("variations has %d entries, max %d", len(f.Variations), MaxVariations)
	}
	for name, val := range f.Variations {
		if !KeyPattern.MatchString(name) {
			verr.add("variation name %q is not a valid identifier", name)
		}
		if err := CheckVariationType(f.Type, val); err != nil {
			verr.add("variation %q: %v", name, err)
		}
	}

	needsBucketing := false
	if f.DefaultRule == nil {
		verr.add("defaultRule is required")
	} else {
		if len(f.DefaultRule.Percentage) > 0 {
			needsBucketing = true
		}
		validateRule("defaultRule", f, f.DefaultRule, registered, segments, verr)
	}
	if len(f.Targeting) > MaxTargetingRules {
		verr.add("targeting has %d rules, max %d", len(f.Targeting), MaxTargetingRules)
	}
	ruleNames := map[string]bool{}
	for i := range f.Targeting {
		r := &f.Targeting[i]
		where := fmt.Sprintf("targeting[%d]", i)
		if r.Name != "" {
			if ruleNames[r.Name] {
				verr.add("%s: duplicate rule name %q", where, r.Name)
			}
			ruleNames[r.Name] = true
		}
		if len(r.Percentage) > 0 {
			needsBucketing = true
		}
		validateRule(where, f, r, registered, segments, verr)
	}

	if f.Experiment != nil {
		needsBucketing = true
		validateExperimentBinding(f, verr)
	}
	if needsBucketing {
		if f.Bucketing == nil {
			verr.add("bucketing is required when percentage splits or experiments are used")
		} else {
			if f.Bucketing.Algorithm != bucketing.AlgorithmID {
				verr.add("bucketing.algorithm %q, want %q", f.Bucketing.Algorithm, bucketing.AlgorithmID)
			}
			if f.Bucketing.Unit == "" {
				verr.add("bucketing.unit is required")
			} else if !registered[f.Bucketing.Unit] {
				verr.add("bucketing.unit %q is not a registered field", f.Bucketing.Unit)
			}
			if len(f.Bucketing.Salt) > MaxSaltLen {
				verr.add("bucketing.salt exceeds %d bytes", MaxSaltLen)
			}
		}
	}

	if len(verr.Problems) > 0 {
		return verr
	}
	return nil
}

func validateExperimentBinding(f *Flag, verr *ValidationError) {
	exp := f.Experiment
	if !KeyPattern.MatchString(exp.ID) {
		verr.add("experiment.id %q is not a valid identifier", exp.ID)
	}
	if exp.Epoch < 0 {
		verr.add("experiment.epoch = %d, want >= 0", exp.Epoch)
	}
	switch exp.State {
	case ExperimentDraft, ExperimentReviewed, ExperimentScheduled, ExperimentRunning,
		ExperimentPaused, ExperimentStopped, ExperimentAnalyzed, ExperimentArchived:
	default:
		verr.add("experiment.state %q unknown", exp.State)
	}
	if exp.Eligibility < 0 || exp.Eligibility > 100 {
		verr.add("experiment.eligibility %v out of [0,100]", exp.Eligibility)
	}
	if exp.Layer != "" {
		if !KeyPattern.MatchString(exp.Layer) {
			verr.add("experiment.layer %q is not a valid identifier", exp.Layer)
		}
		if exp.LayerSlot == nil {
			verr.add("experiment.layerSlot is required when layer is set")
		} else if exp.LayerSlot.Start < 0 || exp.LayerSlot.End > bucketing.Buckets ||
			exp.LayerSlot.Start >= exp.LayerSlot.End {
			verr.add("experiment.layerSlot [%d,%d) invalid", exp.LayerSlot.Start, exp.LayerSlot.End)
		}
	}
	// Experiment-bound flags must have a deterministic fallback so that
	// non-enrolled units and paused experiments never hit a split (§6).
	if f.DefaultRule != nil && f.DefaultRule.Variation == "" {
		verr.add("defaultRule.variation is required for experiment-bound flags (non-enrolled fallback)")
	}
}

func validateRule(where string, f *Flag, r *Rule, registered map[string]bool,
	segments map[string]*Segment, verr *ValidationError) {

	hasVariation := r.Variation != ""
	hasPercentage := len(r.Percentage) > 0
	if hasVariation == hasPercentage {
		verr.add("%s: exactly one of variation/percentage must be set", where)
	}
	if hasVariation {
		if _, ok := f.Variations[r.Variation]; !ok {
			verr.add("%s: unknown variation %q", where, r.Variation)
		}
	}
	if hasPercentage {
		if _, err := bucketing.NewSplit(r.Percentage); err != nil {
			verr.add("%s: percentage: %v", where, err)
		}
		for name := range r.Percentage {
			if _, ok := f.Variations[name]; !ok {
				verr.add("%s: percentage references unknown variation %q", where, name)
			}
		}
	}
	if len(r.Constraints) > MaxConstraints {
		verr.add("%s: %d constraints, max %d", where, len(r.Constraints), MaxConstraints)
	}
	for i := range r.Constraints {
		validateConstraint(fmt.Sprintf("%s.constraints[%d]", where, i), &r.Constraints[i], registered, verr)
	}
	if len(r.Segments) > MaxSegmentsPerRule {
		verr.add("%s: %d segments, max %d", where, len(r.Segments), MaxSegmentsPerRule)
	}
	for _, ref := range r.Segments {
		if !KeyPattern.MatchString(ref) {
			verr.add("%s: segment ref %q is not a valid key", where, ref)
			continue
		}
		seg, ok := segments[ref]
		if !ok {
			verr.add("%s: segment %q not found in snapshot scope", where, ref)
			continue
		}
		validateSegmentTree(where, seg, registered, segments, map[string]bool{}, 0, verr)
	}
}

func validateSegmentTree(where string, seg *Segment, registered map[string]bool,
	segments map[string]*Segment, visiting map[string]bool, depth int, verr *ValidationError) {
	if depth > MaxSegmentDepth {
		verr.add("%s: segment nesting exceeds depth %d", where, MaxSegmentDepth)
		return
	}
	if visiting[seg.Key] {
		verr.add("%s: segment cycle at %q", where, seg.Key)
		return
	}
	visiting[seg.Key] = true
	defer delete(visiting, seg.Key)

	if seg.SchemaVersion != SchemaVersion {
		verr.add("%s: segment %q schemaVersion = %d", where, seg.Key, seg.SchemaVersion)
	}
	if len(seg.Constraints) == 0 && len(seg.Segments) == 0 {
		verr.add("%s: segment %q matches everything (no constraints, no nested segments)", where, seg.Key)
	}
	for i := range seg.Constraints {
		validateConstraint(fmt.Sprintf("%s.segment[%s].constraints[%d]", where, seg.Key, i),
			&seg.Constraints[i], registered, verr)
	}
	for _, ref := range seg.Segments {
		nested, ok := segments[ref]
		if !ok {
			verr.add("%s: segment %q references missing segment %q", where, seg.Key, ref)
			continue
		}
		validateSegmentTree(where, nested, registered, segments, visiting, depth+1, verr)
	}
}

func validateConstraint(where string, c *Constraint, registered map[string]bool, verr *ValidationError) {
	if c.Field == "" {
		verr.add("%s: empty field", where)
	} else if !registered[c.Field] {
		verr.add("%s: field %q is not registered (§5.2 closed field set)", where, c.Field)
	}
	if !Operators[c.Op] {
		verr.add("%s: unknown operator %q", where, c.Op)
		return
	}
	switch c.Op {
	case OpIn, OpNotIn:
		if len(c.Values) == 0 || len(c.Values) > MaxSetValues {
			verr.add("%s: %s requires 1..%d values", where, c.Op, MaxSetValues)
		}
		for _, v := range c.Values {
			if !isScalar(v) {
				verr.add("%s: %s values must be scalars", where, c.Op)
				break
			}
		}
	case OpRegex:
		pattern, ok := c.Value.(string)
		if !ok {
			verr.add("%s: regex value must be a string", where)
			break
		}
		if len(pattern) > MaxRegexLen {
			verr.add("%s: regex exceeds %d bytes", where, MaxRegexLen)
			break
		}
		if _, err := regexp.Compile(pattern); err != nil {
			verr.add("%s: regex does not compile: %v", where, err)
		}
	case OpInCIDR:
		cidr, ok := c.Value.(string)
		if !ok || !validCIDR(cidr) {
			verr.add("%s: in_cidr value must be a valid CIDR string", where)
		}
	case OpSemverGt, OpSemverGte, OpSemverLt, OpSemverLte, OpSemverEq:
		s, ok := c.Value.(string)
		if !ok || !validSemver(s) {
			verr.add("%s: %s value must be a valid semver string", where, c.Op)
		}
	case OpTimeBefore, OpTimeAfter:
		if !validTimeOperand(c.Value) {
			verr.add("%s: %s value must be RFC3339 or numeric epoch seconds", where, c.Op)
		}
	case OpGt, OpGte, OpLt, OpLte:
		if !isNumber(c.Value) {
			verr.add("%s: %s value must be numeric", where, c.Op)
		}
	case OpEq, OpNe:
		if !isScalar(c.Value) {
			verr.add("%s: %s value must be a scalar", where, c.Op)
		}
	}
}

func CheckVariationType(t FlagType, v any) error {
	switch t {
	case FlagBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("value %v is not a bool", brief(v))
		}
	case FlagString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("value %v is not a string", brief(v))
		}
	case FlagNumber:
		if !isNumber(v) {
			return fmt.Errorf("value %v is not a number", brief(v))
		}
	case FlagJSON:
		// any JSON value accepted
	}
	return nil
}

func brief(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
}

// ValidateSegment validates a standalone segment document against its own
// field declarations; cross-checks against referencing flags happen in
// ValidateFlag.
func ValidateSegment(s *Segment, segments map[string]*Segment) error {
	verr := &ValidationError{}
	if s == nil {
		return &ValidationError{Problems: []string{"segment is nil"}}
	}
	if s.SchemaVersion != SchemaVersion {
		verr.add("schemaVersion = %d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if !KeyPattern.MatchString(s.Key) {
		verr.add("key %q does not match %s", s.Key, KeyPattern)
	}
	// Standalone validation registers all referenced fields; flag-level
	// validation re-checks them against the flag's allowlist.
	registered := map[string]bool{TargetingKeyField: true}
	for i := range s.Constraints {
		registered[s.Constraints[i].Field] = true
	}
	collectSegmentFields(s, segments, registered, map[string]bool{}, 0)
	for i := range s.Constraints {
		validateConstraint(fmt.Sprintf("constraints[%d]", i), &s.Constraints[i], registered, verr)
	}
	if len(s.Constraints) == 0 && len(s.Segments) == 0 {
		verr.add("segment matches everything (no constraints, no nested segments)")
	}
	for _, ref := range s.Segments {
		nested, ok := segments[ref]
		if !ok {
			verr.add("references missing segment %q", ref)
			continue
		}
		validateSegmentTree("segment", nested, registered, segments, map[string]bool{s.Key: true}, 0, verr)
	}
	if len(verr.Problems) > 0 {
		return verr
	}
	return nil
}

func collectSegmentFields(s *Segment, segments map[string]*Segment, registered, seen map[string]bool, depth int) {
	if depth > MaxSegmentDepth || seen[s.Key] {
		return
	}
	seen[s.Key] = true
	for i := range s.Constraints {
		registered[s.Constraints[i].Field] = true
	}
	for _, ref := range s.Segments {
		if nested, ok := segments[ref]; ok {
			collectSegmentFields(nested, segments, registered, seen, depth+1)
		}
	}
}
