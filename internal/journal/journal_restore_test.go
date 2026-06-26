package journal

import "testing"

func TestMemoryRestoreSeedsHistoryAndContinuesSeq(t *testing.T) {
	m := NewMemory()
	m.Restore([]Record{
		{Seq: 1, Kind: KindSystem, Actor: "x", Data: "a"},
		{Seq: 2, Kind: KindSystem, Actor: "y", Data: "b"},
	})
	if got := m.Records(); len(got) != 2 || got[0].Data != "a" {
		t.Fatalf("restore did not seed history: %+v", got)
	}
	if r := m.Append(KindSystem, "z", "c"); r.Seq != 3 {
		t.Fatalf("append after restore got seq %d, want 3 (continue past restored)", r.Seq)
	}
}
