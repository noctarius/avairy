package operator

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

// vendorFS holds the vendored browser libs (markdown-it/marked + DOMPurify + highlight.js, #23) —
// served same-origin, no CDN, so the console stays self-contained and airgap-friendly.
//
//go:embed web/vendor
var vendorFS embed.FS

// handleUI serves the single-page operator console. It's static — the token (handed over the same
// way as for avairy-tui) is supplied as a ?token= query param and the page's JS uses it for the
// stream/state/action calls. Open: http://<core>/operator/ui?token=<operator-token>
func handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// vendorHandler serves the vendored JS/CSS under <PathUI>/vendor/ (content-type by extension).
func vendorHandler() http.Handler {
	sub, _ := fs.Sub(vendorFS, "web/vendor")
	return http.StripPrefix(PathUI+"/vendor/", http.FileServer(http.FS(sub)))
}
