package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// A tiny click-zone layer (v2-native — bubblezone targets classic charmbracelet/bubbletea and won't
// take our charm.land/v2 mouse messages). Clickable text is wrapped with mark(id, s); after the view
// is rendered, scan() records each id's on-screen bounds and strips the markers so they never reach
// the terminal, and hit(x, y) maps a mouse position back to an id.
//
// Markers are NUL-delimited and can't occur in rendered content: mark(id,s) emits
// "\x00S<id>\x00" + s + "\x00E<id>\x00". Splitting a line on "\x00" then alternates content (even
// indices) and tokens (odd), so column math is just ansi.StringWidth of the content segments.
const zoneSep = "\x00"

// zoneBox is a clickable region on one row (affordances are short, single-line). x1 inclusive, x2
// exclusive.
type zoneBox struct{ row, x1, x2 int }

type zones struct {
	box map[string]zoneBox
}

func newZones() *zones { return &zones{box: map[string]zoneBox{}} }

func (z *zones) reset() { z.box = map[string]zoneBox{} }

// mark wraps clickable text so scan can find it. Returns s unchanged if id is empty.
func mark(id, s string) string {
	if id == "" {
		return s
	}
	return zoneSep + "S" + id + zoneSep + s + zoneSep + "E" + id + zoneSep
}

// scan records zone bounds from a marked view and returns the view with markers stripped.
func (z *zones) scan(view string) string {
	if !strings.Contains(view, zoneSep) {
		return view
	}
	var out strings.Builder
	for row, line := range strings.Split(view, "\n") {
		if row > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(z.scanLine(row, line))
	}
	return out.String()
}

func (z *zones) scanLine(row int, line string) string {
	if !strings.Contains(line, zoneSep) {
		return line
	}
	parts := strings.Split(line, zoneSep)
	var clean strings.Builder
	col := 0
	starts := map[string]int{}
	for i, p := range parts {
		if i%2 == 0 { // content segment
			clean.WriteString(p)
			col += ansi.StringWidth(p)
			continue
		}
		if p == "" { // stray separator; ignore
			continue
		}
		id := p[1:]
		switch p[0] {
		case 'S':
			starts[id] = col
		case 'E':
			if x1, ok := starts[id]; ok {
				z.box[id] = zoneBox{row: row, x1: x1, x2: col}
			}
		}
	}
	return clean.String()
}

// hit returns the id of the zone containing (x, y), or "" if none.
func (z *zones) hit(x, y int) string {
	for id, b := range z.box {
		if y == b.row && x >= b.x1 && x < b.x2 {
			return id
		}
	}
	return ""
}
