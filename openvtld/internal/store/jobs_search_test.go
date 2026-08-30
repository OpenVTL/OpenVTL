package store

import (
	"context"
	"testing"
)

// SearchJobs backs the Jobs page's "search all jobs": a case-insensitive
// free-text match across id/kind/state/cart label/trigger/generation/system/
// error over the whole job table, newest first, never a nil slice.
func TestSearchJobs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mk := func(kind, label, state, trigger string) {
		if _, err := s.CreateJob(ctx, kind, label, nil, "", state, trigger); err != nil {
			t.Fatal(err)
		}
	}
	mk("export", "OVS001L5", "done", "manual")
	mk("import", "OVS002L5", "running", "recover")
	mk("evict", "OVS001L5", "failed", "ie-watcher") // newest of the two OVS001 jobs
	mk("pool_create", "pool1", "done", "manual")

	check := func(q string, want int) {
		t.Helper()
		got, err := s.SearchJobs(ctx, q, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("q=%q returned nil slice (serializes as null)", q)
		}
		if len(got) != want {
			t.Fatalf("q=%q: got %d, want %d", q, len(got), want)
		}
	}
	check("ovs001", 2)      // cart label, case-insensitive, two jobs
	check("recover", 1)     // trigger
	check("export", 1)      // kind
	check("failed", 1)      // state
	check("", 4)            // match-all
	check("zz_no_match", 0) // non-nil empty

	if got, _ := s.SearchJobs(ctx, "OVS001", 100); got[0].Kind != "evict" {
		t.Fatalf("not newest-first: got %q", got[0].Kind)
	}
}
