package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestZonesScanAndHit(t *testing.T) {
	z := newZones()
	// A line with a leading label, two adjacent styled buttons, and a trailing wide rune — exercises
	// ANSI-aware and wide-rune column math.
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	line := "hi " + mark("a", red.Render("[allow]")) + mark("b", "[deny]") + " 世"
	view := "top\n" + line
	clean := z.scan(view)

	if strings.Contains(clean, zoneSep) {
		t.Fatalf("markers must be stripped from output: %q", clean)
	}
	if !strings.Contains(clean, "[allow]") || !strings.Contains(clean, "[deny]") {
		t.Fatalf("content must survive: %q", clean)
	}
	// "hi " = 3 cells on row 1; [allow] spans cols 3..9, [deny] 10..15.
	if id := z.hit(4, 1); id != "a" {
		t.Fatalf("click inside [allow] should hit a, got %q", id)
	}
	if id := z.hit(11, 1); id != "b" {
		t.Fatalf("click inside [deny] should hit b, got %q", id)
	}
	if id := z.hit(0, 1); id != "" {
		t.Fatalf("click on the label should hit nothing, got %q", id)
	}
	if id := z.hit(4, 0); id != "" {
		t.Fatalf("click on another row should miss, got %q", id)
	}
}
