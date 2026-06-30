package operator

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"avairy/internal/control"
	"avairy/internal/journal"
)

// RandomToken mints a random bearer token for the operator API (used as the default when no
// -operator-token is supplied).
func RandomToken() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Server serves the operator API for remote TUI/web clients (item #18). It's mounted under the
// control listener so it inherits that channel's TLS; a bearer token (the operator token, handed
// over the same way as the enrollment token) authenticates every request.
type Server struct {
	svc   *Services
	token string
	web   bool // serve the browser console at /operator/ui (opt-in via -web)
}

// NewServer wraps services with a bearer token. An empty token leaves the API OPEN (dev only).
// web enables the browser console page (#17); the API itself (for avairy-tui) is always served.
func NewServer(svc *Services, token string, web bool) *Server {
	return &Server{svc: svc, token: token, web: web}
}

// Handler returns the operator routes. Mount it under "/operator/" on the control mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// The web UI (#17) is a second client of this same API: a static page, served unauthenticated
	// (its data calls carry the token), that consumes the stream/state/actions below in a browser.
	// Opt-in: only mounted with -web, so a headless/remote-TUI deployment needn't expose a page.
	if s.web {
		mux.HandleFunc(PathUI, handleUI)
		mux.Handle(PathUI+"/vendor/", vendorHandler()) // markdown/sanitize/highlight libs (#23)
	}
	mux.HandleFunc(PathStream, s.auth(s.handleStream))
	mux.HandleFunc(PathState, s.auth(s.handleState))
	mux.HandleFunc(PathInject, s.auth(s.handleInject))
	mux.HandleFunc(PathReact, s.auth(s.handleReact))
	mux.HandleFunc(PathInterrupt, s.auth(s.handleInterrupt))
	mux.HandleFunc(PathApproval, s.auth(s.handleApproval))
	mux.HandleFunc(PathConflict, s.auth(s.handleConflict))
	mux.HandleFunc(PathCommit, s.auth(s.handleCommit))
	mux.HandleFunc(PathToken, s.auth(s.handleToken))
	mux.HandleFunc(PathConsult, s.auth(s.handleConsult))
	mux.HandleFunc(PathClose, s.auth(s.handleClose))
	return mux
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			h(w, r) // open (dev)
			return
		}
		// mTLS (#30): a verified operator client cert authenticates without a token. Only an operator
		// cert (avairy-operator: SAN) qualifies — a node cert, though CA-signed, does not.
		if r.TLS != nil {
			for _, chain := range r.TLS.VerifiedChains {
				if len(chain) > 0 && control.OperatorIDFromCert(chain[0]) != "" {
					h(w, r)
					return
				}
			}
		}
		// Bearer header (API clients) OR ?token= query param — the browser's EventSource can't set
		// headers, so the web UI passes the token in the URL.
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// handleStream replays the journal then streams new records as SSE. It subscribes BEFORE snapshotting
// so nothing appended in between is lost; the small backfill/live overlap is deduped by the client
// on the records' original Seq.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub, cancel := s.svc.Journal.Subscribe()
	defer cancel()
	for _, rec := range s.svc.Journal.Records() {
		writeRecord(w, rec)
	}
	writeRaw(w, "event: "+readyEvent+"\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case rec, ok := <-sub:
			if !ok {
				return
			}
			writeRecord(w, rec)
			flusher.Flush()
		}
	}
}

func writeRecord(w http.ResponseWriter, rec journal.Record) {
	b, err := json.Marshal(encodeRecord(rec))
	if err != nil {
		return
	}
	writeRaw(w, "data: "+string(b)+"\n\n")
}

func writeRaw(w http.ResponseWriter, s string) { _, _ = w.Write([]byte(s)) }

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.svc.state())
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req injectRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.svc.Inject(req.Target, req.Body)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReact(w http.ResponseWriter, r *http.Request) {
	var req reactRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.svc.React(req.Seq, req.Kind)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	s.svc.Interrupt()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalDecision
	if !readJSON(w, r, &req) {
		return
	}
	s.svc.ResolveApproval(req.ID, req.Decision)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConflict(w http.ResponseWriter, r *http.Request) {
	var req conflictDecision
	if !readJSON(w, r, &req) {
		return
	}
	s.svc.ResolveConflict(req.ID, req.Decision, req.Target)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req commitRequest
	if !readJSON(w, r, &req) {
		return
	}
	if s.svc.Commit == nil {
		writeJSON(w, commitResponse{Error: "core has no git repo"})
		return
	}
	hash, err := s.svc.Commit(req.Message)
	resp := commitResponse{Hash: hash}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, resp)
}

func (s *Server) handleConsult(w http.ResponseWriter, r *http.Request) {
	var req consultRequest
	if !readJSON(w, r, &req) {
		return
	}
	if s.svc.Consult == nil {
		writeJSON(w, consultResponse{Error: "consult unavailable"})
		return
	}
	id, err := s.svc.Consult(req.Target, req.Family)
	resp := consultResponse{ID: id}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, resp)
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	var req closeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if s.svc.CloseConsult != nil {
		s.svc.CloseConsult(req.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if s.svc.NewToken == nil {
		http.Error(w, "no control API", http.StatusNotFound)
		return
	}
	writeJSON(w, tokenResponse{Token: s.svc.NewToken()})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
