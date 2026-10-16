package model_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
	"gopkg.in/yaml.v3"
)

func metadataFlag() *model.Flag {
	return &model.Flag{SchemaVersion: 1, Key: "checkout", Type: model.FlagBool,
		Variations: map[string]any{"off": false, "on": true}, DefaultRule: &model.Rule{Variation: "off"}}
}
func metadataSegment() *model.Segment {
	return &model.Segment{SchemaVersion: 1, Key: "employees", Constraints: []model.Constraint{
		{Field: "targetingKey", Op: model.OpEq, Value: "employee-1"},
	}}
}

func TestDisplayMetadataValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, displayName, description string
		wantError                      bool
	}{
		{name: "optional"},
		{name: "Chinese", displayName: "结算灰度策略", description: "仅向内部用户开放。\n观察指标后扩大比例。"},
		{name: "Unicode boundary", displayName: strings.Repeat("策", 128), description: strings.Repeat("说", 2048)},
		{name: "name too long", displayName: strings.Repeat("策", 129), wantError: true},
		{name: "description too long", description: strings.Repeat("说", 2049), wantError: true},
		{name: "invalid name UTF8", displayName: string([]byte{0xff}), wantError: true},
		{name: "invalid description UTF8", description: string([]byte{0xff}), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, s := metadataFlag(), metadataSegment()
			f.DisplayName, f.Description = tc.displayName, tc.description
			s.DisplayName, s.Description = tc.displayName, tc.description
			if err := model.ValidateFlag(f, nil); (err != nil) != tc.wantError {
				t.Fatalf("flag validation: %v", err)
			}
			if err := model.ValidateSegment(s, nil); (err != nil) != tc.wantError {
				t.Fatalf("segment validation: %v", err)
			}
		})
	}
	f, s := metadataFlag(), metadataSegment()
	f.Key, s.Key = "中文策略", "内部用户"
	if model.ValidateFlag(f, nil) == nil || model.ValidateSegment(s, nil) == nil {
		t.Fatal("metadata must not relax stable ASCII key validation")
	}
}

func TestDisplayMetadataSerializationAndHash(t *testing.T) {
	t.Parallel()
	// The pre-metadata schema-1 encodings must remain byte-for-byte stable.
	f, s := metadataFlag(), metadataSegment()
	legacyFlag := `{"schemaVersion":1,"key":"checkout","type":"bool","variations":{"off":false,"on":true},"defaultRule":{"variation":"off"}}`
	legacySegment := `{"schemaVersion":1,"key":"employees","constraints":[{"field":"targetingKey","op":"eq","value":"employee-1"}]}`
	for _, tc := range []struct {
		name     string
		document any
		legacy   string
	}{
		{"flag", f, legacyFlag}, {"segment", s, legacySegment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := model.CanonicalJSON(tc.document)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.legacy {
				t.Fatalf("absent metadata changed legacy document/hash: %s", data)
			}
		})
	}
	oldFlagHash, _ := model.HashFlag(f)
	oldSegmentHash, _ := model.HashSegment(s)
	f.DisplayName, f.Description = "结算策略", "逐步开放新结算流程"
	s.DisplayName, s.Description = "内部用户", "员工白名单"
	flagHash, _ := model.HashFlag(f)
	segmentHash, _ := model.HashSegment(s)
	if flagHash == oldFlagHash || segmentHash == oldSegmentHash {
		t.Fatal("metadata changes must participate in approval/publication hashes")
	}
	for _, codec := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{"JSON", json.Marshal, json.Unmarshal}, {"YAML", yaml.Marshal, yaml.Unmarshal},
	} {
		t.Run(codec.name, func(t *testing.T) {
			flagBytes, err := codec.marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			segmentBytes, err := codec.marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var restoredFlag model.Flag
			var restoredSegment model.Segment
			if err := codec.unmarshal(flagBytes, &restoredFlag); err != nil {
				t.Fatal(err)
			}
			if err := codec.unmarshal(segmentBytes, &restoredSegment); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f, &restoredFlag) || !reflect.DeepEqual(s, &restoredSegment) {
				t.Fatal("Chinese metadata did not survive serialization")
			}
			gotFlagHash, _ := model.HashFlag(&restoredFlag)
			gotSegmentHash, _ := model.HashSegment(&restoredSegment)
			if gotFlagHash != flagHash || gotSegmentHash != segmentHash {
				t.Fatal("metadata roundtrip changed approval/publication hashes")
			}
		})
	}
}

func TestDisplayMetadataDoesNotChangeEvaluation(t *testing.T) {
	t.Parallel()
	f, s := metadataFlag(), metadataSegment()
	f.Targeting = []model.Rule{{Segments: []string{s.Key}, Variation: "on"}}
	snapshot := &model.Snapshot{SchemaVersion: 1, Workspace: "ws", Environment: "prod", ManifestRevision: 1,
		Flags: map[string]*model.Flag{f.Key: f}, Segments: map[string]*model.Segment{s.Key: s}}
	evaluate := func() []model.Result {
		e, err := evaluation.NewEvaluator(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return []model.Result{e.Evaluate(f.Key, model.EvaluationContext{TargetingKey: "employee-1"}), e.Evaluate(f.Key, model.EvaluationContext{TargetingKey: "external-1"})}
	}
	before := evaluate()
	f.DisplayName, f.Description = "结算策略", "员工先体验"
	s.DisplayName, s.Description = "内部用户", "只匹配员工账号"
	after := evaluate()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("metadata changed evaluation: before %+v, after %+v", before, after)
	}
	if after[0].Value != true || after[1].Value != false {
		t.Fatal("test must cover both matched and default results")
	}
}
