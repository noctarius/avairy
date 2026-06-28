// Package jsonrpc is the line-delimited JSON-RPC 2.0 peer shared by the stdio-based agent adapters
// (the Codex app-server and the ACP engine). It owns request-id allocation, the pending-call table,
// framed writes, and the read loop; each adapter supplies a Handler for inbound notifications and
// server-initiated requests, and keeps its own params/response payload types.
package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// request is an outbound JSON-RPC request frame.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// message is any inbound frame: a response (id, result/error), a notification (method, no id), or a
// server request (method + id).
type message struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  *errObject       `json:"error"`
}

type errObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type result struct {
	res json.RawMessage
	err error
}

// Handler reacts to peer-initiated traffic. OnServerRequest MUST answer (via Peer.Write) or the
// turn hangs; OnNotification is fire-and-forget.
type Handler interface {
	OnNotification(method string, params json.RawMessage)
	OnServerRequest(id json.RawMessage, method string, params json.RawMessage)
}

// Peer is one JSON-RPC endpoint over a subprocess's stdio. name prefixes error messages.
type Peer struct {
	name  string
	stdin io.WriteCloser
	// Done is closed when the read loop ends (the subprocess's stdout closed). In-flight Calls
	// unblock with a "closed" error.
	Done chan struct{}

	encMu sync.Mutex // serializes framed writes

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan result
}

// NewPeer builds a peer that writes to stdin; start the read loop with Run.
func NewPeer(name string, stdin io.WriteCloser) *Peer {
	return &Peer{name: name, stdin: stdin, Done: make(chan struct{}), pending: make(map[int64]chan result)}
}

// Write frames v as one JSON line on stdin (used by Handlers to answer server requests).
func (p *Peer) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.encMu.Lock()
	defer p.encMu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Call sends a request and waits for its matching response, ctx cancellation, or peer death.
func (p *Peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&p.nextID, 1)
	ch := make(chan result, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()

	if err := p.Write(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.res, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.Done:
		return nil, errors.New(p.name + ": session closed")
	}
}

// Send fires a request without waiting for its response (e.g. a cancel). It still carries an id, as
// the peers expect.
func (p *Peer) Send(method string, params any) error {
	return p.Write(request{JSONRPC: "2.0", ID: atomic.AddInt64(&p.nextID, 1), Method: method, Params: params})
}

// Close closes the write side (stdin) of the pipe.
func (p *Peer) Close() error { return p.stdin.Close() }

// Run reads frames from r until the stream ends, dispatching server traffic to h and resolving
// pending Calls. It closes Done on return. Run it in its own goroutine.
func (p *Peer) Run(r io.Reader, h Handler) {
	defer close(p.Done)
	dec := json.NewDecoder(r)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			return
		}
		switch {
		case m.Method != "" && m.ID != nil:
			h.OnServerRequest(*m.ID, m.Method, m.Params)
		case m.Method != "":
			h.OnNotification(m.Method, m.Params)
		case m.ID != nil:
			id, _ := strconv.ParseInt(string(*m.ID), 10, 64)
			p.mu.Lock()
			ch := p.pending[id]
			delete(p.pending, id)
			p.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- result{err: fmt.Errorf("%s rpc error %d: %s", p.name, m.Error.Code, m.Error.Message)}
				} else {
					ch <- result{res: m.Result}
				}
			}
		}
	}
}
