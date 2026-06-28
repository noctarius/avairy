// Package buildinfo carries version metadata stamped into the binaries at build time via -ldflags
// (-X avairy/internal/buildinfo.Version=… etc.; see the Makefile). With a plain `go build` the
// defaults below apply, so a dev build still reports something sensible.
package buildinfo

import (
	"fmt"
	"runtime"
)

var (
	// Version is the release tag (e.g. v0.1.0) or "dev" for an unstamped build.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the UTC build timestamp (RFC 3339).
	Date = "unknown"
)

// String is a one-line, human-readable build identifier.
func String() string {
	return fmt.Sprintf("avairy %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
