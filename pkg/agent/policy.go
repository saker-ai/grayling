package agent

import (
	"encoding/json/v2"
	"errors"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
)

// Policy keeps the compiled evaluator private so callers cannot mutate its
// snapshot through Evaluator.Snapshot or a returned JSON variation.
type Policy struct{ evaluator *evaluation.Evaluator }

func CompilePolicy(payload []byte, scope Scope) (*Policy, error) {
	if len(payload) == 0 || len(payload) > MaxResourceBytes {
		return nil, ErrResource
	}
	var snapshot model.Snapshot
	if err := json.Unmarshal(payload, &snapshot, json.RejectUnknownMembers(true)); err != nil {
		return nil, ErrResource
	}
	if snapshot.ManifestRevision < 0 {
		return nil, ErrResource
	}
	if snapshot.Workspace != scope.WorkspaceID || snapshot.Environment != scope.EnvironmentID {
		return nil, ErrScope
	}
	for key, flag := range snapshot.Flags {
		if flag == nil || key != flag.Key {
			return nil, ErrResource
		}
	}
	for key, segment := range snapshot.Segments {
		if segment == nil || key != segment.Key {
			return nil, ErrResource
		}
	}
	evaluator, err := evaluation.NewEvaluator(&snapshot)
	if err != nil {
		return nil, errors.Join(ErrResource, err)
	}
	return &Policy{evaluator: evaluator}, nil
}
func VerifyPolicy(v *Verifier, r *agentv1.SignedResource, scope Scope, key string, mark Watermark, now time.Time) (*Policy, Claims, error) {
	c, err := v.Verify(r, scope, agentv1.ResourceKind_RESOURCE_KIND_POLICY, key, mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	policy, err := CompilePolicy(r.Payload, scope)
	if err != nil {
		return nil, Claims{}, err
	}
	return policy, c, nil
}
func (p *Policy) Evaluate(key string, context model.EvaluationContext) model.Result {
	result := p.evaluator.Evaluate(key, context)
	result.Value = cloneValue(result.Value)
	return result
}
func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(value))
		for k, v := range value {
			copy[k] = cloneValue(v)
		}
		return copy
	case []any:
		copy := make([]any, len(value))
		for i, v := range value {
			copy[i] = cloneValue(v)
		}
		return copy
	default:
		return value
	}
}
