package workspace

import (
	"strings"
	"testing"
)

// MergeMarkers marks only the lines that actually differ — common lines stay clean (one copy,
// outside any marker), and a single changed span becomes one hunk with both sides.
func TestMergeMarkersHunkLevel(t *testing.T) {
	local := []byte("alpha\nlocal-change\nomega\n")
	hub := []byte("alpha\nhub-change\nomega\n")
	out := string(MergeMarkers(local, hub, 7))

	if !HasConflictMarkers([]byte(out)) {
		t.Fatalf("expected conflict markers:\n%s", out)
	}
	if strings.Count(out, markerStart) != 1 {
		t.Fatalf("expected a single hunk, got %d:\n%s", strings.Count(out, markerStart), out)
	}
	// Unchanged lines appear exactly once and OUTSIDE the markers (alpha before, omega after).
	if strings.Count(out, "alpha") != 1 || strings.Count(out, "omega") != 1 {
		t.Fatalf("common lines should not be duplicated:\n%s", out)
	}
	hunkStart := strings.Index(out, markerStart)
	if strings.Index(out, "alpha") > hunkStart {
		t.Fatalf("unchanged 'alpha' should precede the hunk:\n%s", out)
	}
	if strings.LastIndex(out, "omega") < strings.Index(out, markerEnd) {
		t.Fatalf("unchanged 'omega' should follow the hunk:\n%s", out)
	}
	// Both sides of the change are present, and the hub version is labelled.
	for _, want := range []string{"local-change", "hub-change", ">>>>>>> hub v7"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// A pure insertion (hub added lines, local had none there) emits an empty "ours" side — no spurious
// blank line — and HasConflictMarkers still detects it.
func TestMergeMarkersInsertion(t *testing.T) {
	local := []byte("one\ntwo\n")
	hub := []byte("one\ninserted\ntwo\n")
	out := string(MergeMarkers(local, hub, 2))
	if !HasConflictMarkers([]byte(out)) {
		t.Fatalf("expected markers for an insertion:\n%s", out)
	}
	if !strings.Contains(out, markerStart+"\n"+markerMid) {
		t.Fatalf("empty local side should put ======= right after the start marker:\n%s", out)
	}
	if !strings.Contains(out, "inserted") {
		t.Fatalf("hub insertion missing:\n%s", out)
	}
}
