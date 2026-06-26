package workspace

import (
	"bytes"
	"fmt"
)

// Git-style 3-way conflict markers (DESIGN.md §9): when a node's edit is rejected as divergent,
// the node writes both sides into the file so the agent resolves it in place — the same workflow
// agents already know from git. The local edit is the "ours" side, so nothing is lost; the file
// is held (not pushed, not overwritten) until the markers are gone.

const (
	markerStart = "<<<<<<< local (your edit)"
	markerMid   = "======="
	markerEnd   = ">>>>>>> hub"
)

// HasConflictMarkers reports whether content still contains unresolved conflict markers.
func HasConflictMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(markerStart)) && bytes.Contains(b, []byte("\n"+markerMid+"\n"))
}

// MergeMarkers wraps the local edit and the canonical hub content in 3-way markers.
func MergeMarkers(local, hub []byte, hubVersion uint64) []byte {
	var b bytes.Buffer
	b.WriteString(markerStart + "\n")
	writeBlock(&b, local)
	b.WriteString(markerMid + "\n")
	writeBlock(&b, hub)
	fmt.Fprintf(&b, "%s v%d\n", markerEnd, hubVersion)
	return b.Bytes()
}

// writeBlock writes content, ensuring it ends with a newline so the next marker is on its own line.
func writeBlock(b *bytes.Buffer, content []byte) {
	b.Write(content)
	if len(content) == 0 || content[len(content)-1] != '\n' {
		b.WriteByte('\n')
	}
}
