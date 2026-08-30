package store

import (
	"context"
	"testing"
	"time"
)

// SearchEvents backs the journal viewer's "search all history": an exact
// kind filter, a case-insensitive free-text match on subject/detail, and
// LIKE wildcards escaped so a literal % typed by the user matches itself.
func TestSearchEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	// id order = insertion order; SearchEvents returns newest (highest id) first.
	seed := []struct{ kind, subject, detail string }{
		{"write", "OVS001L5", "wrote 64k block"},
		{"move", "OVS001L5", "MOVE MEDIUM slot 3 -> drive 0"},
		{"pool_create", "pool1", "zpool ready"},
		{"write", "OVS002L5", "50% complete"},   // literal percent
		{"write", "OVS002L5", "fifty complete"}, // no percent
	}
	for i, e := range seed {
		if err := s.LogEvent(ctx, base.Add(time.Duration(i)*time.Second), e.kind, e.subject, e.detail); err != nil {
			t.Fatal(err)
		}
	}

	// Free text, case-insensitive, across subject + detail.
	if got, err := s.SearchEvents(ctx, "ovs001", "", 100); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 {
		t.Fatalf("subject match: got %d, want 2", len(got))
	} else if got[0].Detail != "MOVE MEDIUM slot 3 -> drive 0" {
		t.Fatalf("not newest-first: got %q", got[0].Detail)
	}

	// Kind filter with no text returns every row of that kind.
	if got, err := s.SearchEvents(ctx, "", "write", 100); err != nil {
		t.Fatal(err)
	} else if len(got) != 3 {
		t.Fatalf("kind filter: got %d, want 3", len(got))
	}

	// Kind + text together.
	if got, err := s.SearchEvents(ctx, "complete", "write", 100); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 {
		t.Fatalf("kind+text: got %d, want 2", len(got))
	}

	// A literal % must match only the row that contains one, not act as a
	// wildcard that matches everything.
	if got, err := s.SearchEvents(ctx, "%", "", 100); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("literal %%: got %d, want 1", len(got))
	} else if got[0].Detail != "50% complete" {
		t.Fatalf("literal %%: matched wrong row %q", got[0].Detail)
	}

	// A query that matches nothing returns a non-nil empty slice, so the API
	// emits [] not null (a nil slice marshals to null and breaks the client).
	if got, err := s.SearchEvents(ctx, "zz_no_match_zz", "", 100); err != nil {
		t.Fatal(err)
	} else if got == nil {
		t.Fatal("zero-match returned nil slice (would serialize as null)")
	} else if len(got) != 0 {
		t.Fatalf("zero-match: got %d, want 0", len(got))
	}

	// Empty query returns everything, capped and newest-first.
	if got, err := s.SearchEvents(ctx, "", "", 100); err != nil {
		t.Fatal(err)
	} else if len(got) != len(seed) {
		t.Fatalf("match-all: got %d, want %d", len(got), len(seed))
	}
}
