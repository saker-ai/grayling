package evaluation

import (
	"fmt"

	"github.com/godeps/grayling/pkg/model"
)

// BoolVariation resolves a bool flag. On any failure (missing flag, type
// mismatch, missing bucket key, corrupt snapshot) it returns the caller's
// default with Failed=true and an explicit error code — the OpenFeature-style
// contract required by §8.
func (e *Evaluator) BoolVariation(key string, ctx model.EvaluationContext, def bool) (bool, model.Result) {
	res := e.Evaluate(key, ctx)
	if res.Failed || res.Value == nil {
		return def, res
	}
	if v, ok := res.Value.(bool); ok {
		return v, res
	}
	return def, failType(res, "bool")
}

// StringVariation resolves a string flag.
func (e *Evaluator) StringVariation(key string, ctx model.EvaluationContext, def string) (string, model.Result) {
	res := e.Evaluate(key, ctx)
	if res.Failed || res.Value == nil {
		return def, res
	}
	if v, ok := res.Value.(string); ok {
		return v, res
	}
	return def, failType(res, "string")
}

// NumberVariation resolves a number flag as float64 (integer variation
// values coerce exactly).
func (e *Evaluator) NumberVariation(key string, ctx model.EvaluationContext, def float64) (float64, model.Result) {
	res := e.Evaluate(key, ctx)
	if res.Failed || res.Value == nil {
		return def, res
	}
	if n, ok := model.Number(res.Value); ok {
		return n, res
	}
	return def, failType(res, "number")
}

// JSONVariation resolves a json flag; the value is returned as-is (decoded
// JSON: map[string]any, []any, string, float64, bool or nil).
func (e *Evaluator) JSONVariation(key string, ctx model.EvaluationContext, def any) (any, model.Result) {
	res := e.Evaluate(key, ctx)
	if res.Failed {
		return def, res
	}
	if res.Value == nil {
		return def, res
	}
	return res.Value, res
}

// failType marks a result as a type-mismatch failure and suppresses any
// exposure: a value the caller cannot use was never "served".
func failType(res model.Result, want string) model.Result {
	res.Reason = model.ReasonError
	res.ErrorCode = model.ErrTypeMismatch
	res.ErrorDetails = fmt.Sprintf("value %v does not match requested type %s", briefValue(res.Value), want)
	res.Failed = true
	res.TrackEvents = false
	res.Experiment = nil
	return res
}

func briefValue(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
}
