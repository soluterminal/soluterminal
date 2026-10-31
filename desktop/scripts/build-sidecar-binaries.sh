#!/usr/bin/env bash
# Cross-compile the two solud binaries Tauri's NSIS installer bundles:
#   - solud.exe       (Windows host-native sidecar)
#   - solud-linux     (Linux binary launched inside WSL by the app)
#
# Run from WSL bash before `cargo tauri build`. Both binaries are
# stamped with the version from desktop/src-tauri/tauri.conf.json via
# -ldflags so the auto-updater can compare against the release manifest.
# Without the stamp, `version=dev` is baked in and the in-app updater
# silently no-ops on every release (daemon/updater.go:189) — exactly
# the failure mode that kept v0.0.421 from reaching one running install.
#
# Output paths follow feedback_windows_build_command.md: write to the
# Windows-side repo root (not WSL ext4) to avoid the UNC exe cache.
#
#   build-sidecar-binaries.sh          release sidecars (needs scripts/release-ldflags.sh)
#   build-sidecar-binaries.sh --dev    unsigned development sidecars
#
# --dev needs nothing outside desktop/ and daemon/ (so it works in a source
# export): no signing key, no auth defaults, no release helpers. Its binaries
# report version "dev", which turns the daemon's auto-updater and install
# check-in off, and `cargo tauri build` accepts them only with
# SOLU_DEV_SIDECARS=1 (desktop/scripts/prepare-sidecar.js), which release
# builds never set. Output goes to the repository root (where
# prepare-sidecar.js reads it) unless WIN_OUT_DIR is set.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)/../.."
ROOT="$(cd "$ROOT" && pwd)"

DEV=0
for arg in "$@"; do
  case "$arg" in
    --dev) DEV=1 ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

if [[ "$DEV" == 1 ]]; then
  GO_BIN="${GO:-go}"
  if [[ ! -f "$ROOT/daemon/webdist/index.html" ]]; then
    echo "error: daemon/webdist is missing; the daemon embeds the web UI. Build it first:" >&2
    echo "       (cd web && npm ci && npm run build) && rm -rf daemon/webdist && cp -r web/dist daemon/webdist" >&2
    exit 1
  fi
  OUT_DIR="${WIN_OUT_DIR:-$ROOT}"
  mkdir -p "$OUT_DIR"
  echo "==> development sidecars (version dev, unsigned updates off) → $OUT_DIR"
  # SOLU_CLOUD_URL set and empty builds a shell with no hosted cloud; the
  # daemon then drops its hosted defaults too.
  DEV_LDFLAGS=""
  if [[ -v SOLU_CLOUD_URL && -z "$SOLU_CLOUD_URL" ]]; then
    DEV_LDFLAGS="-X main.hostedCloud=off"
    echo "==> no hosted cloud (SOLU_CLOUD_URL is empty)"
  fi
  build_dev() { # GOOS GOARCH output
    echo "==> building $3 ($1-$2)"
    (cd "$ROOT/daemon" && GOOS="$1" GOARCH="$2" CGO_ENABLED=0 "$GO_BIN" build -trimpath -ldflags "$DEV_LDFLAGS" -o "$OUT_DIR/$3" .)
  }
  build_dev windows amd64 solud.exe
  build_dev linux amd64 solud-linux
  build_dev linux arm64 solud-linux-arm64
  host_os="$("$GO_BIN" env GOHOSTOS)"
  if [[ "$host_os" != windows ]]; then
    # prepare-sidecar.js stages ./solud as the host-native sidecar off Windows.
    build_dev "$host_os" "$("$GO_BIN" env GOHOSTARCH)" solud
  fi
  echo
  echo "✓ development sidecars in $OUT_DIR"
  echo "  cargo tauri dev                          # stages them with prepare-sidecar.js --dev"
  echo "  SOLU_DEV_SIDECARS=1 cargo tauri build    # unsigned local installer; never publish it"
  exit 0
fi

if [[ ! -f "$ROOT/scripts/release-ldflags.sh" || ! -d "$ROOT/scripts/fix-agent" ]]; then
  echo "error: release sidecars from this script need the private release tooling, which this" >&2
  echo "       checkout does not have. Run with --dev for unsigned development sidecars; the" >&2
  echo "       official build stages its own (scripts/official-build.sh, docs/OFFICIAL-BUILD.md)." >&2
  exit 1
fi
. "$ROOT/scripts/release-ldflags.sh"

VERSION=$(solu_tauri_version "$ROOT")
if [[ -z "$VERSION" || "$VERSION" == "dev" ]]; then
  echo "error: refusing to bake VERSION='$VERSION' into sidecar binaries" >&2
  exit 1
fi
if ! solu_require_webdist_json "$ROOT/daemon/webdist" "$VERSION"; then
  found_web="$(sed -n 's/.*"webVersion":"\([^"]*\)".*/\1/p' "$ROOT/daemon/webdist/webdist.json" 2>/dev/null | head -1)"
  echo "error: daemon/webdist is stale (web ${found_web:-missing} != version $VERSION) — rebuild the web bundle (cd web && npm ci && npm run build; rm -rf daemon/webdist; cp -r web/dist daemon/webdist) before building sidecars" >&2
  exit 1
fi
WEBDIST_MARKER="$(solu_extract_webdist_marker "$ROOT/daemon/webdist/webdist.json" | head -1)"
WEBDIST_VERSION="$(printf '%s\n' "$WEBDIST_MARKER" | sed -n 's/^solu-webdist v=\([^ ]*\) src=.*/\1/p')"
WEBDIST_SOURCE="$(printf '%s\n' "$WEBDIST_MARKER" | sed -n 's/^solu-webdist v=[^ ]* src=\([^ ]*\) sha256=.*/\1/p')"
if [[ "${SOLU_ALLOW_UNSIGNED_UPDATE_MANIFEST:-0}" == "1" ]]; then
  echo "error: refusing to build installer sidecars without update manifest signature enforcement" >&2
  exit 1
fi
solu_load_supabase_defaults "$ROOT"
SIGNING_ENV="${SOLU_UPDATE_SIGNING_ENV:-$HOME/.soluterminal/update-signing.env}"
if [[ -f "$SIGNING_ENV" ]]; then
  while IFS= read -r line; do
    case "$line" in
      SOLU_UPDATE_MANIFEST_PUBKEY=*)
        : "${SOLU_UPDATE_MANIFEST_PUBKEY:=${line#SOLU_UPDATE_MANIFEST_PUBKEY=}}"
        ;;
      SOLU_UPDATE_SIGNING_PRIVATE_KEY=*)
        : "${SOLU_UPDATE_SIGNING_PRIVATE_KEY:=${line#SOLU_UPDATE_SIGNING_PRIVATE_KEY=}}"
        ;;
    esac
  done <"$SIGNING_ENV"
  export SOLU_UPDATE_MANIFEST_PUBKEY="${SOLU_UPDATE_MANIFEST_PUBKEY:-}"
  export SOLU_UPDATE_SIGNING_PRIVATE_KEY="${SOLU_UPDATE_SIGNING_PRIVATE_KEY:-}"
fi
if [[ -n "${SOLU_UPDATE_SIGNING_PRIVATE_KEY:-}" && -z "${SOLU_UPDATE_MANIFEST_PUBKEY:-}" ]]; then
  echo "==> deriving manifest public key from signing private key"
  eval "$(SOLU_UPDATE_SIGNING_PRIVATE_KEY="$SOLU_UPDATE_SIGNING_PRIVATE_KEY" go run "$ROOT/scripts/update-manifest-key.go" --public-from-private)"
  export SOLU_UPDATE_MANIFEST_PUBKEY
fi
if [[ "${SOLU_ALLOW_NO_AUTH_SIDECAR:-0}" != "1" ]]; then
  solu_require_supabase_defaults "Tauri sidecar binaries"
fi
# The same build stamp, operator-agent bundle and go flags as publish.sh, so an
# installer's bundled daemon is byte-identical to the released one (0.0.1222's
# was stamped channel=local and differed; scripts/test-release-reproduce.sh
# keeps the three in step).
SOURCE_SHA="$(git -C "$ROOT" rev-parse HEAD)"
# Verify the committed resource before computing the clean-source stamp.
node "$ROOT/scripts/windows-resources.mjs" "$VERSION" --check
LDFLAGS="$(SOLU_BUILD_CHANNEL=release SOLU_SOURCE_SHA="$SOURCE_SHA" solu_daemon_ldflags "$VERSION")"
export SOLU_SOURCE_SHA="$SOURCE_SHA"
(
  cd "$ROOT"
  rm -rf daemon/agentscripts
  mkdir -p daemon/agentscripts
  tar -cf - --sort=name --owner=0 --group=0 --numeric-owner --mtime='@0' --mode='u=rwX,go=rX' \
    --exclude='*.test.mjs' --exclude='README.md' --exclude='deploy' \
    scripts/fix-agent scripts/pr-agent scripts/investigate scripts/triage scripts/agent-defaults.env |
    gzip -n -9 >daemon/agentscripts/operator-agents.tar.gz
  touch daemon/agentscripts/.gitkeep
)
echo "==> baking version: ${VERSION}"
if [[ -n "${SOLU_SUPABASE_URL:-}" && -n "${SOLU_SUPABASE_ANON_KEY:-}" ]]; then
  echo "==> auth gate: ENABLED (Supabase creds baked into sidecars)"
else
  echo "warning: auth gate defaults omitted from sidecars (SOLU_ALLOW_NO_AUTH_SIDECAR=1)" >&2
fi

# Default Windows-side output paths from feedback_windows_build_command.md.
# Override by exporting WIN_OUT_DIR before invoking, e.g.
#   WIN_OUT_DIR=/mnt/c/Code/soluterminal ./build-sidecar-binaries.sh
WIN_OUT_DIR="${WIN_OUT_DIR:-/mnt/c/Users/benja/soluterminal}"
mkdir -p "$WIN_OUT_DIR"

echo "==> building solud.exe (windows-amd64) → $WIN_OUT_DIR/solud.exe"
cd "$ROOT/daemon"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -buildvcs=false -trimpath -ldflags "${LDFLAGS}" -o "$WIN_OUT_DIR/solud.exe" .

echo "==> building solud-linux (linux-amd64) → $WIN_OUT_DIR/solud-linux"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -buildvcs=false -trimpath -ldflags "${LDFLAGS}" -o "$WIN_OUT_DIR/solud-linux" .

echo "==> building solud-linux-arm64 (linux-arm64) → $WIN_OUT_DIR/solud-linux-arm64"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -buildvcs=false -trimpath -ldflags "${LDFLAGS}" -o "$WIN_OUT_DIR/solud-linux-arm64" .

MANIFEST="$WIN_OUT_DIR/.soluterminal-shell-sidecars.sha256"
{
  printf 'version %s\n' "$VERSION"
  printf 'webdist_version %s\n' "$WEBDIST_VERSION"
  printf 'webdist_source %s\n' "$WEBDIST_SOURCE"
  printf 'update_manifest_pubkey %s\n' "$(solu_effective_update_manifest_pubkey)"
  printf 'flavor %s\n' "$(solu_build_flavor "$ROOT/daemon")"
  (cd "$WIN_OUT_DIR" && sha256sum solud.exe solud-linux solud-linux-arm64)
} >"$MANIFEST"

echo
echo "✓ wrote $WIN_OUT_DIR/solud.exe"
echo "✓ wrote $WIN_OUT_DIR/solud-linux"
echo "✓ wrote $WIN_OUT_DIR/solud-linux-arm64"
echo "✓ wrote $MANIFEST"
echo
echo "Now copy these to the repo root the Tauri installer reads from"
echo "(the path prepare-sidecar.js stages from), then run cargo tauri build."
