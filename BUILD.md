# Building avairy

> **TL;DR:** `make build` (host) · `make release` (all targets → `dist/`) · `make package`
> (release + per-platform archives + `SHA256SUMS`). This doc explains the commands the
> [Makefile](Makefile) runs, for reference or CI without `make`.

avairy is **pure Go (no CGO)**, so it cross-compiles to every target from any machine with a
Go 1.26+ toolchain — no C compiler, no per-OS setup. There are three executables:

- **`avairy`** — core + operator TUI (run on the operator machine)
- **`avairy-node`** — the node daemon (run on each remote machine/VM, incl. Windows)
- **`avairy-tui`** — the operator console as a standalone client (attach to a remote core)

## Versioning

Build metadata is stamped into `internal/buildinfo` at link time and shown by `avairy version`
(likewise `avairy-node version` / `avairy-tui version`). The Makefile injects it via
`-ldflags -X`:

| Field | Default | Source in CI |
|-------|---------|--------------|
| `Version` | `git describe` or `dev` | the release tag |
| `Commit` | short git SHA | `git rev-parse --short HEAD` |
| `Date` | UTC build time | `date -u` at build |

Override any of them: `make build VERSION=v1.2.3 COMMIT=abc1234 DATE=2026-01-01T00:00:00Z`.

## Releases

Releases are produced by the [`release` workflow](.github/workflows/release.yml) and tagged with
**semver**:

- `vMAJOR.MINOR.PATCH` — a stable release (e.g. `v1.2.3`).
- `vMAJOR.MINOR.PATCH-<prerelease>` — a prerelease (e.g. `v1.0.0-rc1`). It's published but marked
  *pre-release*, so it isn't "latest" and `install.sh` keeps serving the last stable (install it
  explicitly with `AVAIRY_VERSION=v1.0.0-rc1`).

Cut one either way: **push the tag** (`git tag v1.2.3 && git push origin v1.2.3`), or **run the
workflow manually** with the version — which creates and pushes the tag for you. CI validates the
semver, runs `make test vet` then `make package`, and publishes a GitHub Release with every platform
archive, a `SHA256SUMS` file, and `install.sh`. End users install with the one-liner in the
[README](README.md#install); the script resolves their OS/arch, downloads the matching archive,
verifies its checksum, and drops the binaries on the `PATH`.

## Target matrix

| OS      | `GOOS`    | arm64            | x86_64 (`amd64`) |
|---------|-----------|------------------|------------------|
| macOS   | `darwin`  | `darwin/arm64`   | `darwin/amd64`   |
| Windows | `windows` | `windows/arm64`  | `windows/amd64`  |
| Linux   | `linux`   | `linux/arm64`    | `linux/amd64`    |
| BSD     | `freebsd` | `freebsd/arm64`  | `freebsd/amd64`  |

> "x86_64" is Go's `amd64`. Windows binaries get a `.exe` suffix. For other BSDs swap `GOOS`
> to `netbsd` or `openbsd` (same arches; all verified to compile).

## Build everything

Save as `build-all.sh` and run it (writes to `dist/`):

```bash
#!/usr/bin/env bash
set -euo pipefail

targets=(
  darwin/arm64  darwin/amd64
  windows/arm64 windows/amd64
  linux/arm64   linux/amd64
  freebsd/arm64 freebsd/amd64
)
cmds=(avairy avairy-node avairy-tui)

for t in "${targets[@]}"; do
  os="${t%/*}"; arch="${t#*/}"
  ext=""; [ "$os" = windows ] && ext=".exe"
  dir="dist/${os}-${arch}"; mkdir -p "$dir"
  for c in "${cmds[@]}"; do
    out="${dir}/${c}${ext}"
    echo "building $out"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      go build -trimpath -ldflags="-s -w" -o "$out" "./cmd/${c}"
  done
done
echo "done → dist/"
```

```sh
chmod +x build-all.sh && ./build-all.sh
```

This produces 24 binaries (3 commands × 8 targets) under per-target dirs, e.g.
`dist/darwin-arm64/avairy`, `dist/windows-amd64/avairy-node.exe`,
`dist/freebsd-arm64/avairy-node`.

> **fish shell:** the loop above is bash. Run it with `bash build-all.sh`, or set per-build
> vars fish-style: `env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ...`.

## Build a single target

```sh
# native (host OS/arch)
go build -o dist/avairy      ./cmd/avairy
go build -o dist/avairy-node ./cmd/avairy-node

# one cross target, e.g. Windows on ARM64
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w" -o dist/windows-arm64/avairy-node.exe ./cmd/avairy-node
```

## Flags used

- `CGO_ENABLED=0` — fully static, dependency-free binaries; required for clean cross-compiles.
- `-trimpath` — strips local filesystem paths from the binary (reproducible, no leakage).
- `-ldflags="-s -w"` — drops the symbol table and DWARF debug info for smaller binaries.

To stamp a version, add e.g. `-ldflags="-s -w -X main.version=$(git describe --tags --always)"`
(requires a `var version string` in the command's `main`).
