package control

import (
	"testing"
	"time"

	"avairy/internal/journal"
	"avairy/internal/workspace"
)

func TestLivenessSweepTransitions(t *testing.T) {
	jrnl := journal.NewMemory()
	c := NewCore(workspace.NewHub(), jrnl)
	c.LivenessTimeout = 50 * time.Millisecond
	c.nodes["n1"] = &NodeInfo{ID: "n1", LastSeen: time.Now(), Live: true}

	// Fresh contact → stays live, no journal entry.
	c.sweepLiveness()
	if !c.nodes["n1"].Live {
		t.Fatal("recently-seen node should stay live")
	}

	// Heartbeats lapse → offline + a node_offline record.
	c.nodes["n1"].LastSeen = time.Now().Add(-time.Second)
	c.sweepLiveness()
	if c.nodes["n1"].Live {
		t.Fatal("lapsed node should be offline")
	}
	if !hasEvent(jrnl, "n1", "node_offline") {
		t.Fatal("expected node_offline journal record")
	}

	// A second sweep with no change must not re-journal (transition-only).
	before := len(jrnl.Records())
	c.sweepLiveness()
	if len(jrnl.Records()) != before {
		t.Fatal("offline node re-journaled without a transition")
	}

	// Contact resumes → online again.
	c.nodes["n1"].LastSeen = time.Now()
	c.sweepLiveness()
	if !c.nodes["n1"].Live {
		t.Fatal("re-contacted node should be live")
	}
	if !hasEvent(jrnl, "n1", "node_online") {
		t.Fatal("expected node_online journal record")
	}
}

func hasEvent(j *journal.Memory, actor, event string) bool {
	for _, rec := range j.Records() {
		if rec.Kind != journal.KindSystem || rec.Actor != actor {
			continue
		}
		if m, ok := rec.Data.(map[string]any); ok && m["event"] == event {
			return true
		}
	}
	return false
}
