package main

import (
	"errors"
	"testing"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
)

func TestAuthorizeNativeConfigUsesCredentialScopeAndRevocation(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	workspaceA, err := store.CreateWorkspace("tenant-a", "Tenant A")
	if err != nil {
		t.Fatal(err)
	}
	envA, err := store.CreateEnvironment(workspaceA.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	workspaceB, err := store.CreateWorkspace("tenant-b", "Tenant B")
	if err != nil {
		t.Fatal(err)
	}
	envB, err := store.CreateEnvironment(workspaceB.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := store.CreateToken(platform.TokenSDK, "native-a", "", workspaceA.ID, envA.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	collector, err := store.CreateToken(platform.TokenCollect, "collector-a", "", workspaceA.ID, envA.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, secret, namespace, group, dataID string
		allowed                                bool
	}{
		{"login", sdk.Secret, "", "", "", true},
		{"own manifest", sdk.Secret, envA.NamespaceID, configstore.Group, configstore.ManifestDataID, true},
		{"own document", sdk.Secret, envA.NamespaceID, configstore.Group, configstore.FlagDataID("checkout", 1), true},
		{"other tenant", sdk.Secret, envB.NamespaceID, configstore.Group, configstore.ManifestDataID, false},
		{"other group", sdk.Secret, envA.NamespaceID, "DEFAULT_GROUP", configstore.ManifestDataID, false},
		{"other data", sdk.Secret, envA.NamespaceID, configstore.Group, "private-secret", false},
		{"wrong audience", collector.Secret, envA.NamespaceID, configstore.Group, configstore.ManifestDataID, false},
		{"invalid token", "unknown", envA.NamespaceID, configstore.Group, configstore.ManifestDataID, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorizeNativeConfig(store, tt.secret, tt.namespace, tt.group, tt.dataID)
			if tt.allowed && err != nil {
				t.Fatal(err)
			}
			if !tt.allowed && !errors.Is(err, platform.ErrAccessDenied) {
				t.Fatalf("error = %v, want access denied", err)
			}
		})
	}
	if err := store.RevokeToken(sdk.Token.ID); err != nil {
		t.Fatal(err)
	}
	if err := authorizeNativeConfig(store, sdk.Secret, envA.NamespaceID, configstore.Group, configstore.ManifestDataID); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("revoked token still usable: %v", err)
	}
}
