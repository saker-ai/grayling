package platform

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/saker-ai/grayling/pkg/model"
)

func TestDisplayMetadataPersistenceAndReapproval(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"flag", "segment"} {
		for _, approved := range []bool{false, true} {
			stateName := "submitted"
			if approved {
				stateName = "approved"
			}
			t.Run(kind+"/"+stateName, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "metadata.json")
				store, err := NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				ws, err := store.CreateWorkspace("metadata", "中文工作区")
				if err != nil {
					t.Fatal(err)
				}
				env, err := store.CreateEnvironment(ws.ID, "prod", "生产环境")
				if err != nil {
					t.Fatal(err)
				}
				key := "stable-key"
				flag := &model.Flag{SchemaVersion: 1, Key: key, Type: model.FlagBool,
					DisplayName: "结算灰度", Description: "仅员工体验\n观察指标后放量",
					Variations: map[string]any{"on": true, "off": false}, DefaultRule: &model.Rule{Variation: "off"}}
				segment := &model.Segment{SchemaVersion: 1, Key: key, DisplayName: "内部用户", Description: "员工测试账号",
					Constraints: []model.Constraint{{Field: "targetingKey", Op: model.OpEq, Value: "employee-1"}}}
				save := func() {
					t.Helper()
					if kind == "flag" {
						_, err = store.SaveFlagDraft(ws.ID, env.ID, flag, RiskNormal, "author", 0)
					} else {
						_, err = store.SaveSegmentDraft(ws.ID, env.ID, segment, RiskNormal, "author")
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				submit := func() {
					t.Helper()
					if kind == "flag" {
						err = store.SubmitFlagDraft(ws.ID, env.ID, key, "author")
					} else {
						err = store.SubmitSegmentDraft(ws.ID, env.ID, key, "author")
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				approve := func() {
					t.Helper()
					if kind == "flag" {
						err = store.ApproveFlagDraft(ws.ID, env.ID, key, "reviewer", "checked")
					} else {
						err = store.ApproveSegmentDraft(ws.ID, env.ID, key, "reviewer")
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				// Normalize only the lifecycle fields asserted identically for both documents.
				inspect := func() (DraftState, string, string, string, string) {
					t.Helper()
					if kind == "flag" {
						d, err := store.GetFlagDraft(ws.ID, env.ID, key)
						if err != nil {
							t.Fatal(err)
						}
						return d.State, d.SubmittedHash, d.ApprovedHash, d.Content.DisplayName, d.Content.Description
					}
					d, err := store.GetSegmentDraft(ws.ID, env.ID, key)
					if err != nil {
						t.Fatal(err)
					}
					return d.State, d.SubmittedHash, d.ApprovedHash, d.Content.DisplayName, d.Content.Description
				}
				save()
				submit()
				if approved {
					approve()
				}
				beforeState, beforeSubmitted, beforeApproved, beforeName, beforeDescription := inspect()
				store, err = NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				gotState, gotSubmitted, gotApproved, gotName, gotDescription := inspect()
				if gotState != beforeState || gotSubmitted != beforeSubmitted || gotApproved != beforeApproved || gotName != beforeName || gotDescription != beforeDescription {
					t.Fatal("Chinese metadata and review hashes must survive JSON store restart")
				}
				flag.DisplayName, flag.Description = "新结算灰度", "更新后的发布说明"
				segment.DisplayName, segment.Description = "内部体验用户", "更新后的用户范围说明"
				save()
				gotState, gotSubmitted, gotApproved, _, _ = inspect()
				if gotState != DraftEditing || gotSubmitted != "" || gotApproved != "" {
					t.Fatal("editing metadata must invalidate existing submission and approval")
				}
				submit()
				_, nextSubmitted, _, _, _ := inspect()
				if nextSubmitted == beforeSubmitted {
					t.Fatal("metadata-only change must change the submitted hash")
				}
				approve()
				store, err = NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				gotState, gotSubmitted, gotApproved, gotName, gotDescription = inspect()
				if gotState != DraftApproved || gotApproved != nextSubmitted || gotSubmitted != nextSubmitted {
					t.Fatal("new metadata requires and preserves a fresh approval")
				}
				expectedName, expectedDescription := flag.DisplayName, flag.Description
				if kind == "segment" {
					expectedName, expectedDescription = segment.DisplayName, segment.Description
				}
				if gotName != expectedName || gotDescription != expectedDescription {
					t.Fatal("updated metadata lost after reapproval and restart")
				}
			})
		}
	}
}

func TestDisplayMetadataChangesInvalidateSubmittedHash(t *testing.T) {
	t.Parallel()
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metadata-hash", "Metadata")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	flag := &model.Flag{SchemaVersion: 1, Key: "checkout", Type: model.FlagBool, DisplayName: "结算策略",
		Variations: map[string]any{"on": true}, DefaultRule: &model.Rule{Variation: "on"}}
	segment := &model.Segment{SchemaVersion: 1, Key: "employees", DisplayName: "内部用户",
		Constraints: []model.Constraint{{Field: "targetingKey", Op: model.OpEq, Value: "employee-1"}}}
	if _, err := store.SaveFlagDraft(ws.ID, env.ID, flag, RiskNormal, "author", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSegmentDraft(ws.ID, env.ID, segment, RiskNormal, "author"); err != nil {
		t.Fatal(err)
	}
	if err := store.SubmitFlagDraft(ws.ID, env.ID, flag.Key, "author"); err != nil {
		t.Fatal(err)
	}
	if err := store.SubmitSegmentDraft(ws.ID, env.ID, segment.Key, "author"); err != nil {
		t.Fatal(err)
	}
	// Model mutable in-memory documents getting changed behind the submitted hash.
	// Approval must detect metadata drift even when matching rules are identical.
	flag.Description = "提交后改变的说明"
	segment.DisplayName = "提交后改变的名字"
	if err := store.ApproveFlagDraft(ws.ID, env.ID, flag.Key, "reviewer", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed Flag metadata approved: %v", err)
	}
	if err := store.ApproveSegmentDraft(ws.ID, env.ID, segment.Key, "reviewer"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed Segment metadata approved: %v", err)
	}
}
