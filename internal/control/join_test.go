package control

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadJoin(t *testing.T) {
	enc := EncodeJoin(JoinBundle{Core: "https://core:7700", Bus: "https://core:7702", Token: "tok"})

	// From an inline string.
	jb, err := ReadJoin(enc, "")
	if err != nil || jb.Core != "https://core:7700" || jb.Token != "tok" {
		t.Fatalf("inline: %+v err=%v", jb, err)
	}

	// From a file (used when the inline string is empty).
	path := filepath.Join(t.TempDir(), "join")
	if err := os.WriteFile(path, []byte("  "+enc+"\n"), 0o600); err != nil { // surrounding whitespace must be trimmed
		t.Fatal(err)
	}
	jb, err = ReadJoin("", path)
	if err != nil || jb.Bus != "https://core:7702" {
		t.Fatalf("file: %+v err=%v", jb, err)
	}

	// Neither set → zero bundle, no error.
	if jb, err := ReadJoin("", ""); err != nil || jb.Core != "" {
		t.Fatalf("empty: %+v err=%v", jb, err)
	}
	// Missing file → error.
	if _, err := ReadJoin("", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing join-file")
	}
}
