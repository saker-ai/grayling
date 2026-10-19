package admin_test

import (
	"errors"
	"testing"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/platform"
)

func TestAgentManagementRBAC(t *testing.T) {
	t.Parallel()
	for _, permission := range []admin.Permission{admin.PermAgentCredentialManage, admin.PermAgentQuotaManage, admin.PermAgentAuditRead, admin.PermAgentRevocationManage} {
		for _, role := range []platform.Role{platform.RoleOwner, platform.RolePublisher, platform.RoleAnalyst, platform.RoleViewer} {
			actor := admin.Actor{Kind: platform.TokenAdmin, Role: role, WorkspaceID: "workspace"}
			err := admin.Authorize(actor, "workspace", permission)
			if (role == platform.RoleOwner) != (err == nil) {
				t.Fatalf("role %s permission %s: %v", role, permission, err)
			}
			if err := admin.Authorize(actor, "outsider", permission); !errors.Is(err, platform.ErrAccessDenied) {
				t.Fatal("role escaped workspace")
			}
		}
		for _, kind := range []platform.TokenKind{platform.TokenSDK, platform.TokenCollect} {
			if err := admin.Authorize(admin.Actor{Kind: kind, Role: platform.RoleOwner, WorkspaceID: "workspace"}, "workspace", permission); !errors.Is(err, platform.ErrAccessDenied) {
				t.Fatal("data-plane audience managed machines")
			}
		}
	}
}
