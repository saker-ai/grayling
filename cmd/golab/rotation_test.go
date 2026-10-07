package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/godeps/golab/internal/platform"
)

func TestRotationOwnerAuthorization(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "rotation-owner", platform.RoleOwner, "", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("rotation-test", "")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "rotation-viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, value string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(value+"\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := readRotationOwner(store, write("viewer", viewer.Secret, 0o600)); err == nil {
		t.Fatal("viewer authorized")
	}
	if _, err := readRotationOwner(store, write("world-readable", owner.Secret, 0o644)); err == nil {
		t.Fatal("world-readable token accepted")
	}
	ownerPath := write("owner", owner.Secret, 0o600)
	link := filepath.Join(t.TempDir(), "owner-link")
	if err := os.Symlink(ownerPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRotationOwner(store, link); err == nil {
		t.Fatal("symlinked token accepted")
	}
	got, err := readRotationOwner(store, ownerPath)
	if err != nil || got.ID != owner.Token.ID {
		t.Fatalf("owner authorization: %v", err)
	}
	if err := store.RevokeToken(owner.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := readRotationOwner(store, write("revoked", owner.Secret, 0o600)); err == nil {
		t.Fatal("revoked owner authorized")
	}
}
