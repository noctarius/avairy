package operator

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

// handleUI serves the single-page operator console. It's static — the token (handed over the same
// way as for avairy-tui) is supplied as a ?token= query param and the page's JS uses it for the
// stream/state/action calls. Open: http://<core>/operator/ui?token=<operator-token>
func handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
