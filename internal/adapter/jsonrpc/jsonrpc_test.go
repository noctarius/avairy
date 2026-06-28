package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingHandler struct {
	mu    sync.Mutex
	notes []string
	reqs  []string
}

func (h *recordingHandler) OnNotification(method string, _ json.RawMessage) {
	h.mu.Lock()
	h.notes = append(h.notes, method)
	h.mu.Unlock()
}

func (h *recordingHandler) OnServerRequest(_ json.RawMessage, method string, _ json.RawMessage) {
	h.mu.Lock()
	h.reqs = append(h.reqs, method)
	h.mu.Unlock()
}

func TestPeer(t *testing.T) {
	cr, cw := io.Pipe() // peer writes cw; server reads cr
	sr, sw := io.Pipe() // server writes sw; peer reads sr
	h := &recordingHandler{}
	p := NewPeer("test", cw)
	go p.Run(sr, h)

	// A fake peer on the other end: push a notification + a server request, then echo "echo"
	// requests and error on "boom".
	go func() {
		fmt.Fprint(sw, `{"method":"note","params":{}}`+"\n")
		fmt.Fprint(sw, `{"id":99,"method":"ask","params":{}}`+"\n")
		dec := json.NewDecoder(cr)
		for {
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := dec.Decode(&req); err != nil {
				return
			}
			switch req.Method {
			case "echo":
				fmt.Fprintf(sw, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", req.ID, req.Params)
			case "boom":
				fmt.Fprintf(sw, `{"jsonrpc":"2.0","id":%d,"error":{"code":7,"message":"kaboom"}}`+"\n", req.ID)
			}
		}
	}()

	// Call round-trips: the server echoes our params back as the result.
	res, err := p.Call(context.Background(), "echo", map[string]int{"v": 42})
	if err != nil || !strings.Contains(string(res), "42") {
		t.Fatalf("echo: res=%s err=%v", res, err)
	}
	// An error response surfaces as an error naming the peer.
	if _, err := p.Call(context.Background(), "boom", nil); err == nil || !strings.Contains(err.Error(), "kaboom") || !strings.Contains(err.Error(), "test") {
		t.Fatalf("boom err=%v", err)
	}
	// The unprompted notification + server request reached the handler.
	if !waitFor(func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notes) == 1 && len(h.reqs) == 1 }) {
		t.Fatalf("handler missed note/ask: notes=%v reqs=%v", h.notes, h.reqs)
	}

	// Peer death (server stops) unblocks in-flight/new Calls with a closed error.
	sw.Close()
	if _, err := p.Call(context.Background(), "echo", nil); err == nil {
		t.Fatal("expected a closed error after the peer died")
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
