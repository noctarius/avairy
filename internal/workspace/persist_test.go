package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHubSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "hub.json") // Save must create the parent dir

	h := NewHub()
	h.Push("alice", Change{Path: "src/main.go", Content: []byte("package main\n"), Base: 0}) // v1
	h.Push("alice", Change{Path: "src/main.go", Content: []byte("package main // v2\n"), Base: 1})
	h.Push("bob", Change{Path: "run.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755, Base: 0})
	h.Push("bob", Change{Path: "old.txt", Content: []byte("x"), Base: 0}) // v1
	h.Push("bob", Change{Path: "old.txt", Deleted: true, Base: 1})        // tombstone v2

	if err := h.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	h2, err := LoadHub(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	main, ok := h2.Get("src/main.go")
	if !ok || main.Version != 2 || string(main.Content) != "package main // v2\n" {
		t.Fatalf("main.go not restored: %+v", main)
	}
	run, _ := h2.Get("run.sh")
	if run.Mode.Perm() != 0o755 || run.Writer != "bob" {
		t.Fatalf("run.sh mode/writer not restored: %+v", run)
	}
	if del, _ := h2.Get("old.txt"); !del.Deleted || del.Version != 2 {
		t.Fatalf("deletion tombstone not restored: %+v", del)
	}

	// A node that already has v2 of main.go pulls nothing; one at v0 pulls the restored state.
	if got := h2.Pull(map[string]uint64{"src/main.go": 2, "run.sh": 1, "old.txt": 2}); len(got) != 0 {
		t.Fatalf("up-to-date node should pull nothing, got %d", len(got))
	}
	if got := h2.Pull(map[string]uint64{}); len(got) != 3 {
		t.Fatalf("fresh node should pull all 3 paths, got %d", len(got))
	}
}

func TestHubLoadMissingIsEmpty(t *testing.T) {
	h, err := LoadHub(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing snapshot should not error: %v", err)
	}
	if len(h.List()) != 0 {
		t.Fatal("missing snapshot should yield an empty hub")
	}
}

func TestHubSaveIfDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	h := NewHub()
	h.Push("a", Change{Path: "f", Content: []byte("x"), Base: 0})

	wrote, err := h.SaveIfDirty(path)
	if err != nil || !wrote {
		t.Fatalf("first save should write: wrote=%v err=%v", wrote, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	// No change since → no write.
	if wrote, _ := h.SaveIfDirty(path); wrote {
		t.Fatal("clean hub should not re-write")
	}
	// A change re-arms it.
	h.Push("a", Change{Path: "f", Content: []byte("y"), Base: 1})
	if wrote, _ := h.SaveIfDirty(path); !wrote {
		t.Fatal("dirtied hub should write")
	}
}

func TestResumeFromHubAdoptsLocalVersions(t *testing.T) {
	wsDir := t.TempDir()
	h := NewHub()
	h.Push("alice", Change{Path: "a.go", Content: []byte("A\n"), Base: 0})     // v1, exists locally
	h.Push("node", Change{Path: "remote.go", Content: []byte("R\n"), Base: 0}) // v1, NOT local

	// Operator's dir has a.go (unchanged) but not remote.go.
	write(t, wsDir, "a.go", "A\n", 0o644)

	nv := NewNodeView("core")
	nv.ResumeFromHub(h, wsDir)

	if nv.Base("a.go") != 1 {
		t.Fatalf("should adopt local file's hub version, got base %d", nv.Base("a.go"))
	}
	if nv.Base("remote.go") != 0 {
		t.Fatal("must NOT claim a hub file absent locally (else SyncUp would delete it)")
	}

	// SyncUp: a.go unchanged → no bump; remote.go unclaimed → not deleted.
	if _, err := nv.SyncUp(h, wsDir, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.Get("a.go"); a.Version != 1 {
		t.Fatalf("unchanged local file churned version to %d", a.Version)
	}
	if r, _ := h.Get("remote.go"); r.Deleted || r.Version != 1 {
		t.Fatalf("node-contributed file was wrongly deleted/changed: %+v", r)
	}
}
