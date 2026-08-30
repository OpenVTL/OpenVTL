package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Migration 003 must apply cleanly on a fresh DB and against a v2 DB —
// the gating logic is exercised by Open() itself.
func TestUsersSessionsRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("fresh db has %d users", n)
	}
	id, err := s.CreateUser(ctx, "op1", "hash1", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "OP1", "hash2", "readonly"); err == nil {
		t.Fatal("COLLATE NOCASE unique should reject case-variant duplicate")
	}
	u, err := s.UserByName(ctx, "op1")
	if err != nil || u.ID != id || u.Role != "admin" {
		t.Fatalf("UserByName: %+v err=%v", u, err)
	}

	// session round trip + expiry + slide
	exp := time.Now().Add(time.Hour)
	if err := s.CreateSession(ctx, "tok1", id, exp, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	su, gotExp, err := s.SessionUser(ctx, "tok1")
	if err != nil || su.Username != "op1" {
		t.Fatalf("SessionUser: %+v err=%v", su, err)
	}
	if gotExp.Unix() != exp.UTC().Truncate(time.Second).Unix() {
		t.Fatalf("expiry mismatch: got %v want %v", gotExp, exp)
	}
	if err := s.SlideSession(ctx, "tok1", time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing session: %v", err)
	}

	// expired session is invisible and reapable
	s.CreateSession(ctx, "tok2", id, time.Now().Add(-time.Minute), "")
	if _, _, err := s.SessionUser(ctx, "tok2"); err != ErrNotFound {
		t.Fatalf("expired session resolved: %v", err)
	}
	if err := s.ReapSessions(ctx); err != nil {
		t.Fatal(err)
	}

	// disabled user's session is invisible; last-admin accounting works
	if n, _ := s.CountAdmins(ctx); n != 1 {
		t.Fatalf("CountAdmins = %d", n)
	}
	if err := s.UpdateUser(ctx, id, "admin", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "tok1"); err != ErrNotFound {
		t.Fatalf("disabled user session resolved: %v", err)
	}
	if n, _ := s.CountAdmins(ctx); n != 0 {
		t.Fatalf("CountAdmins after disable = %d", n)
	}

	// delete removes sessions too
	if err := s.DeleteUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByID(ctx, id); err != ErrNotFound {
		t.Fatalf("deleted user still resolves: %v", err)
	}
}

func TestACLSeedOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.SeedACLs(ctx, []string{" naa.aaa ", "naa.bbb", ""}); err != nil {
		t.Fatal(err)
	}
	acls, _ := s.ListACLs(ctx, FabricFC)
	if len(acls) != 2 {
		t.Fatalf("seeded %d ACLs, want 2", len(acls))
	}
	// operator removes one; a restart's re-seed must NOT resurrect it
	if err := s.RemoveACL(ctx, "naa.aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedACLs(ctx, []string{"naa.aaa", "naa.bbb"}); err != nil {
		t.Fatal(err)
	}
	acls, _ = s.ListACLs(ctx, FabricFC)
	if len(acls) != 1 || acls[0].WWPN != "naa.bbb" {
		t.Fatalf("re-seed resurrected removed ACL: %+v", acls)
	}
	// fabric filtering: a foreign-fabric row (e.g. one left behind by a
	// pre-FC-only build) is invisible to the fc list
	if err := s.AddACL(ctx, InitiatorACL{WWPN: "legacy.initiator:client", Fabric: "other", Libraries: "10,20"}); err != nil {
		t.Fatal(err)
	}
	if acls, _ = s.ListACLs(ctx, FabricFC); len(acls) != 1 {
		t.Fatalf("fc list sees foreign-fabric rows: %+v", acls)
	}
	acls, _ = s.ListACLs(ctx, "other")
	if len(acls) != 1 || acls[0].Fabric != "other" {
		t.Fatalf("foreign-fabric list wrong: %+v", acls)
	}
	// scope helpers: '' = all (nil sets); csv parses
	if s := acls[0].LibrarySet(); s == nil || !s[10] || !s[20] || s[30] {
		t.Fatalf("library set wrong: %v", s)
	}
	if acls[0].PortSet() != nil {
		t.Fatalf("empty ports should mean all (nil)")
	}
	if err := s.UpdateACLScopes(ctx, "legacy.initiator:client", "migr", "", "10"); err != nil {
		t.Fatal(err)
	}
	acls, _ = s.ListACLs(ctx, "other")
	if acls[0].Alias != "migr" || acls[0].Libraries != "10" {
		t.Fatalf("scope update lost: %+v", acls[0])
	}
	// explicit-none sentinel: '-' = empty NON-nil sets (deny), distinct
	// from '' = all (nil)
	if err := s.UpdateACLScopes(ctx, "legacy.initiator:client", "migr", ScopeNone, ScopeNone); err != nil {
		t.Fatal(err)
	}
	acls, _ = s.ListACLs(ctx, "other")
	if ps := acls[0].PortSet(); ps == nil || len(ps) != 0 {
		t.Fatalf("ScopeNone ports should be an empty non-nil set: %v", ps)
	}
	if ls := acls[0].LibrarySet(); ls == nil || len(ls) != 0 {
		t.Fatalf("ScopeNone libraries should be an empty non-nil set: %v", ls)
	}
}

func TestPoolSamples(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		p := PoolSample{
			TS:   base.Add(time.Duration(i) * time.Hour).UTC().Format(time.RFC3339),
			Pool: "vg_vtl/pooldata", VDOUsedBytes: int64(i) * 100,
		}
		if err := s.AddPoolSample(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastPoolSample(ctx, "vg_vtl/pooldata")
	if err != nil || last.VDOUsedBytes != 200 {
		t.Fatalf("LastPoolSample: %+v err=%v", last, err)
	}
	hist, err := s.PoolHistory(ctx, "vg_vtl/pooldata", base.Add(30*time.Minute))
	if err != nil || len(hist) != 2 {
		t.Fatalf("PoolHistory: %d entries err=%v", len(hist), err)
	}
	if err := s.PrunePoolSamples(ctx, base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	hist, _ = s.PoolHistory(ctx, "vg_vtl/pooldata", base.Add(-time.Hour))
	if len(hist) != 2 {
		t.Fatalf("prune kept %d, want 2", len(hist))
	}
}

// Audit with the v0.5 signature lands actor + remote_addr.
func TestAuditIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Audit(ctx, "op1", "1.2.3.4:555", "job.retry", "OVT001L5", `{"id":1}`); err != nil {
		t.Fatal(err)
	}
	entries, err := s.RecentAudit(ctx, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("RecentAudit: %d err=%v", len(entries), err)
	}
	e := entries[0]
	if e.Actor != "op1" || e.RemoteAddr != "1.2.3.4:555" || e.Action != "job.retry" {
		t.Fatalf("entry: %+v", e)
	}
}
