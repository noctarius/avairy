package agent

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// ToolSummary renders a concise, human-readable description of a tool call — "Bash: go test
// ./...", "Read src/main.go" — so the operator sees what an agent is actually doing instead of
// "Bash" or "Read" repeated. It also gives loop detection a meaningful per-action signature:
// reading 100 different files is 100 distinct actions, not one loop.
func ToolSummary(tc *ToolCall) string {
	if tc == nil {
		return ""
	}
	if d := actionDetail(tc.Input); d != "" {
		return tc.Name + ": " + d
	}
	return tc.Name
}

// ActionKey is the loop-detection signature for a tool call (richer than ToolSummary, which is for
// humans). It folds in a digest of the change content and the read region, so the same file edited
// twice with *different* content keys differently, and the same file read at *different* offsets
// keys differently — only a genuinely identical action repeats the same key. This stops the
// facilitator from flagging "edit → read → edit → read" (real progress) as a loop (#14).
func ActionKey(tc *ToolCall) string {
	if tc == nil {
		return ""
	}
	key := ToolSummary(tc)
	if d := contentDigest(tc.Input); d != "" {
		key += "#" + d // distinct edits/writes to one file differ
	}
	if r := readRegion(tc.Input); r != "" {
		key += "@" + r // distinct read positions in one file differ
	}
	return key
}

// bodyKeys are the large diff/body fields TrimInput drops; they identify *what* an edit changed.
var bodyKeys = []string{"content", "new_string", "old_string", "file_text", "patch", "edits"}

// contentDigest fingerprints an edit's change: the precomputed "_digest" TrimInput left behind (the
// node path, where bodies were already stripped), else a hash of the raw body fields still present
// (core-local agents journal untrimmed input). Same change → same digest.
func contentDigest(in map[string]any) string {
	if d, ok := in["_digest"].(string); ok && d != "" {
		return d
	}
	return bodyDigest(in)
}

// bodyDigest hashes the body fields in a fixed order (map iteration is random, so we can't range).
func bodyDigest(in map[string]any) string {
	var b strings.Builder
	for _, k := range bodyKeys {
		if v, ok := in[k]; ok {
			fmt.Fprintf(&b, "%s=%v\x00", k, v)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	h := fnv.New64a()
	h.Write([]byte(b.String()))
	return strconv.FormatUint(h.Sum64(), 16)
}

// readRegion renders the slice of a file a read/view touched, so reading different parts isn't seen
// as repetition while re-reading the same span is.
func readRegion(in map[string]any) string {
	var parts []string
	for _, k := range []string{"offset", "limit", "line", "lines", "start_line", "end_line", "view_range"} {
		if v, ok := in[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, ",")
}

// actionDetail picks the identifying argument of a tool call (the command, the file, the
// pattern) — what distinguishes one invocation from another.
func actionDetail(in map[string]any) string {
	for _, k := range []string{"command", "cmd", "file_path", "filePath", "path", "pattern", "query", "url"} {
		if v, ok := in[k].(string); ok && v != "" {
			return trunc(firstLineEllipsis(v), 120)
		}
	}
	return ""
}

// TrimInput returns a copy of a tool input safe to ship over the wire and store in the journal:
// large or noisy values (file bodies, diffs) are dropped and long strings truncated, keeping
// the identifiers (command, file_path, …) that matter for display and loop detection.
func TrimInput(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "content", "new_string", "old_string", "file_text", "patch", "edits":
			continue // bodies/diffs: too big to ship
		}
		if s, ok := v.(string); ok {
			out[k] = trunc(s, 256)
		} else {
			out[k] = v
		}
	}
	// Keep a small fingerprint of the dropped diff so loop detection can tell one edit from another
	// (#14) without shipping the whole body. ActionKey reads it back as "_digest".
	if d := bodyDigest(in); d != "" {
		out["_digest"] = d
	}
	return out
}

func firstLineEllipsis(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
