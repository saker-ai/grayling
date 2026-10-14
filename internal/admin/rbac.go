// Package admin is the Grayling control plane: RBAC, the draft→review→publish
// pipeline over the gonacos config substrate, rollback, deletion with
// tombstones, experiment lifecycle operations and audit (docs/技术方案.md
// §5.3, §9, §11).
package admin

import (
	"fmt"

	"github.com/saker-ai/grayling/internal/platform"
)

// Actor is the authenticated principal of one admin request. The API layer
// derives it from the presented token ONLY — never from request parameters
// (§9: 每次请求先鉴权，再从凭证确定 workspace/environment).
type Actor struct {
	TokenID     string
	Name        string
	Kind        platform.TokenKind
	Role        platform.Role
	WorkspaceID string // token scope; empty on global owner tokens
}

// IsGlobalOwner reports whether the actor is an unsscoped owner token.
func (a Actor) IsGlobalOwner() bool {
	return a.Kind == platform.TokenAdmin && a.Role == platform.RoleOwner && a.WorkspaceID == ""
}

// Permission is one admin-API capability.
type Permission string

const (
	PermWorkspaceAdmin  Permission = "workspace.admin" // create workspaces/environments (global owner)
	PermTokenManage     Permission = "token.manage"    // mint/revoke credentials (owner)
	PermFlagRead        Permission = "flag.read"
	PermFlagWrite       Permission = "flag.write" // save/submit drafts
	PermFlagApprove     Permission = "flag.approve"
	PermFlagPublish     Permission = "flag.publish"     // publish/rollback/delete + experiment transitions
	PermExperimentWrite Permission = "experiment.write" // create/modify experiment contracts
	PermReportRead      Permission = "report.read"
	PermAIWrite         Permission = "ai.write"
	PermAIContentRead   Permission = "ai.content.read" // raw prompt/AI content (§9: 单独授权)
	PermQuotaManage     Permission = "quota.manage"
)

// Authorize enforces object-level RBAC. The workspace scope comes from the
// token; global owners pass everywhere. SDK/collect tokens are never valid
// admin actors.
func Authorize(actor Actor, workspaceID string, perm Permission) error {
	if actor.Kind != platform.TokenAdmin {
		return fmt.Errorf("%w: %s tokens cannot call the admin API", platform.ErrAccessDenied, actor.Kind)
	}
	if actor.IsGlobalOwner() {
		return nil
	}
	if workspaceID != "" && actor.WorkspaceID != workspaceID {
		return fmt.Errorf("%w: token is scoped to a different workspace", platform.ErrAccessDenied)
	}
	switch perm {
	case PermWorkspaceAdmin:
		return fmt.Errorf("%w: %s requires a global owner token", platform.ErrAccessDenied, perm)
	case PermTokenManage, PermQuotaManage, PermAIContentRead:
		if actor.Role != platform.RoleOwner {
			return fmt.Errorf("%w: %s requires the owner role", platform.ErrAccessDenied, perm)
		}
		return nil
	case PermFlagRead, PermReportRead:
		// every admin role may read within its workspace scope
		return nil
	case PermFlagWrite, PermFlagApprove, PermFlagPublish:
		if actor.Role != platform.RoleOwner && actor.Role != platform.RolePublisher {
			return fmt.Errorf("%w: %s requires owner or publisher role", platform.ErrAccessDenied, perm)
		}
		return nil
	case PermExperimentWrite, PermAIWrite:
		if actor.Role != platform.RoleOwner && actor.Role != platform.RolePublisher &&
			actor.Role != platform.RoleAnalyst {
			return fmt.Errorf("%w: %s requires owner, publisher or analyst role", platform.ErrAccessDenied, perm)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown permission %q", platform.ErrAccessDenied, perm)
}

// RoleCan is a UI helper for the console (renders only permitted actions).
func RoleCan(role platform.Role, perm Permission) bool {
	err := Authorize(Actor{Kind: platform.TokenAdmin, Role: role, WorkspaceID: "w"}, "w", perm)
	return err == nil
}
