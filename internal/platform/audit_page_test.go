package platform

import "testing"

func TestAuditPageFiltersAcrossCursor(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateWorkspace("audit-a", "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateWorkspace("audit-b", "B")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ ws, action, object string }{
		{a.ID, "flag.publish", "old"},
		{b.ID, "flag.publish", "hidden"},
		{a.ID, "flag.publish", "middle"},
		{a.ID, "token.revoke", "skip"},
		{a.ID, "flag.publish", "new"},
	} {
		if err := s.Audit("operator", RoleOwner, entry.action, entry.ws, "", entry.object, ""); err != nil {
			t.Fatal(err)
		}
	}
	first := s.AuditPage(a.ID, 0, 2, "publish")
	if len(first) != 2 || first[0].Object != "new" || first[1].Object != "middle" {
		t.Fatalf("first page: %+v", first)
	}
	second := s.AuditPage(a.ID, first[1].ID, 2, "publish")
	if len(second) != 1 || second[0].Object != "old" {
		t.Fatalf("second page: %+v", second)
	}
}
