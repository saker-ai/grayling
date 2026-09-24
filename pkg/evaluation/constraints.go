package evaluation

import (
	"net/netip"
	"regexp"
	"time"

	"github.com/blang/semver/v4"
	"github.com/godeps/golab/pkg/model"
)

// MaxRegexInputBytes bounds regex evaluation cost per constraint (§5.2).
// Longer inputs simply do not match. Go's RE2 engine is linear-time, so
// compiled pattern + bounded input = bounded cost.
const MaxRegexInputBytes = 4096

type operandKind int

const (
	operandNone operandKind = iota
	operandScalar
	operandNumber
	operandTime
	operandSemver
	operandCIDR
	operandRegex
	operandSet
)

type compiledConstraint struct {
	field string
	op    string
	kind  operandKind
	num   float64
	time  time.Time
	sem   semver.Version
	pfx   netip.Prefix
	re    *regexp.Regexp
	set   []any
	scal  any
}

func compileConstraint(c *model.Constraint) (compiledConstraint, error) {
	cc := compiledConstraint{field: c.Field, op: c.Op}
	switch c.Op {
	case model.OpEq, model.OpNe:
		if !isScalarValue(c.Value) {
			return cc, constraintError(c, "operand must be scalar")
		}
		cc.kind, cc.scal = operandScalar, c.Value
	case model.OpIn, model.OpNotIn:
		if len(c.Values) == 0 {
			return cc, constraintError(c, "requires values")
		}
		for _, v := range c.Values {
			if !isScalarValue(v) {
				return cc, constraintError(c, "values must be scalars")
			}
		}
		cc.kind, cc.set = operandSet, c.Values
	case model.OpGt, model.OpGte, model.OpLt, model.OpLte:
		n, ok := model.Number(c.Value)
		if !ok {
			return cc, constraintError(c, "operand must be numeric")
		}
		cc.kind, cc.num = operandNumber, n
	case model.OpTimeBefore, model.OpTimeAfter:
		t, ok := model.ParseTimeOperand(c.Value)
		if !ok {
			return cc, constraintError(c, "operand must be RFC3339 or epoch seconds")
		}
		cc.kind, cc.time = operandTime, t
	case model.OpRegex:
		pattern, ok := c.Value.(string)
		if !ok {
			return cc, constraintError(c, "operand must be a string")
		}
		if len(pattern) > model.MaxRegexLen {
			return cc, constraintError(c, "pattern exceeds max length")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return cc, constraintError(c, "regex does not compile: "+err.Error())
		}
		cc.kind, cc.re = operandRegex, re
	case model.OpInCIDR:
		cidr, ok := c.Value.(string)
		if !ok {
			return cc, constraintError(c, "operand must be a string")
		}
		pfx, err := netip.ParsePrefix(cidr)
		if err != nil {
			return cc, constraintError(c, "invalid CIDR: "+err.Error())
		}
		cc.kind, cc.pfx = operandCIDR, pfx.Masked()
	case model.OpSemverGt, model.OpSemverGte, model.OpSemverLt, model.OpSemverLte, model.OpSemverEq:
		s, ok := c.Value.(string)
		if !ok {
			return cc, constraintError(c, "operand must be a string")
		}
		sv, err := model.ParseSemver(s)
		if err != nil {
			return cc, constraintError(c, "invalid semver: "+err.Error())
		}
		cc.kind, cc.sem = operandSemver, sv
	default:
		return cc, constraintError(c, "unknown operator")
	}
	return cc, nil
}

type constraintCompileError struct {
	field, op, msg string
}

func (e *constraintCompileError) Error() string {
	return "constraint " + e.field + " " + e.op + ": " + e.msg
}

func constraintError(c *model.Constraint, msg string) error {
	return &constraintCompileError{field: c.Field, op: c.Op, msg: msg}
}

func isScalarValue(v any) bool {
	switch v.(type) {
	case bool, string:
		return true
	}
	_, ok := model.Number(v)
	return ok
}

// matchConstraint evaluates one compiled constraint. Semantics (normative):
//
//   - A field missing from the context matches NOTHING, including ne/not_in.
//     Absence is not a value; rules that must fire for anonymous contexts
//     need explicit design, not negation.
//   - Type mismatches (e.g. numeric op on a string) never match.
//   - Regex inputs longer than MaxRegexInputBytes never match.
func matchConstraint(cc *compiledConstraint, ctx model.EvaluationContext) bool {
	raw, ok := ctx.Field(cc.field)
	if !ok {
		return false
	}
	switch cc.kind {
	case operandScalar:
		eq := model.EqualScalars(raw, cc.scal)
		if cc.op == model.OpNe {
			// Both sides are known-present; ne is exact negation.
			return isScalarValue(raw) && !eq
		}
		return eq
	case operandSet:
		if !isScalarValue(raw) {
			return false
		}
		found := false
		for _, v := range cc.set {
			if model.EqualScalars(raw, v) {
				found = true
				break
			}
		}
		if cc.op == model.OpNotIn {
			return !found
		}
		return found
	case operandNumber:
		n, ok := model.Number(raw)
		if !ok {
			return false
		}
		switch cc.op {
		case model.OpGt:
			return n > cc.num
		case model.OpGte:
			return n >= cc.num
		case model.OpLt:
			return n < cc.num
		case model.OpLte:
			return n <= cc.num
		}
		return false
	case operandTime:
		t, ok := model.ParseTimeOperand(raw)
		if !ok {
			return false
		}
		if cc.op == model.OpTimeBefore {
			return t.Before(cc.time)
		}
		return t.After(cc.time)
	case operandRegex:
		s, ok := raw.(string)
		if !ok || len(s) > MaxRegexInputBytes {
			return false
		}
		return cc.re.MatchString(s)
	case operandCIDR:
		s, ok := raw.(string)
		if !ok {
			return false
		}
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return false
		}
		return cc.pfx.Contains(addr.Unmap())
	case operandSemver:
		s, ok := raw.(string)
		if !ok {
			return false
		}
		v, err := model.ParseSemver(s)
		if err != nil {
			return false
		}
		cmp := v.Compare(cc.sem)
		switch cc.op {
		case model.OpSemverGt:
			return cmp > 0
		case model.OpSemverGte:
			return cmp >= 0
		case model.OpSemverLt:
			return cmp < 0
		case model.OpSemverLte:
			return cmp <= 0
		case model.OpSemverEq:
			return cmp == 0
		}
		return false
	}
	return false
}
