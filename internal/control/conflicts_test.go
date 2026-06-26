package control

import "testing"

func TestConflictsRaiseDedupResolve(t *testing.T) {
	var raised, resolved int
	c := NewConflicts()
	c.OnRaise = func(OperatorConflict) { raised++ }
	c.OnResolve = func(OperatorConflict, string) { resolved++ }

	id1 := c.Raise(OperatorConflict{Path: "a.go", HubVersion: 3, Source: "seed"})
	c.Raise(OperatorConflict{Path: "b.go", HubVersion: 1, Source: "seed"})
	// Re-raising the same path updates in place (same id), does not stack or re-fire OnRaise.
	id1b := c.Raise(OperatorConflict{Path: "a.go", HubVersion: 4, Source: "seed"})
	if id1 != id1b {
		t.Fatalf("re-raise should reuse id: %s vs %s", id1, id1b)
	}
	if raised != 2 {
		t.Fatalf("OnRaise should fire once per distinct path, got %d", raised)
	}

	pend := c.Pending()
	if len(pend) != 2 {
		t.Fatalf("want 2 pending, got %d", len(pend))
	}
	if pend[0].Path != "a.go" || pend[0].HubVersion != 4 {
		t.Fatalf("oldest-first / update-in-place wrong: %+v", pend[0])
	}

	oc, ok := c.Resolve(id1, ConflictDelegate)
	if !ok || oc.Path != "a.go" {
		t.Fatalf("resolve returned %+v ok=%v", oc, ok)
	}
	if _, ok := c.Resolve(id1, ConflictMine); ok {
		t.Fatal("second resolve of same id should report not-pending")
	}
	if got := c.Pending(); len(got) != 1 || got[0].Path != "b.go" {
		t.Fatalf("after resolve want [b.go], got %+v", got)
	}

	// ClearPath removes by path (the markers were removed and it synced cleanly).
	if !c.ClearPath("b.go") {
		t.Fatal("ClearPath should report a clear")
	}
	if c.ClearPath("b.go") {
		t.Fatal("ClearPath on an absent path should report false")
	}
	if len(c.Pending()) != 0 {
		t.Fatal("expected no pending conflicts")
	}
	if resolved != 2 { // Resolve(id1) + ClearPath(b.go); the no-op second calls don't fire
		t.Fatalf("OnResolve fired %d times, want 2", resolved)
	}
}
