#!/usr/bin/env bash
# Small cross-platform linker test: no daemon binary is run or installed.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t solu-winres-test.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
version="$(node -p 'JSON.parse(require("fs").readFileSync(process.argv[1])).version' "$ROOT/desktop/src-tauri/tauri.conf.json")"
node "$ROOT/scripts/windows-resources.mjs" "$version" "$TMP/one.syso"
node "$ROOT/scripts/windows-resources.mjs" "$version" "$TMP/two.syso"
cmp "$TMP/one.syso" "$TMP/two.syso"
cmp "$TMP/one.syso" "$ROOT/daemon/rsrc_windows_amd64.syso"
node "$ROOT/scripts/windows-resources.mjs" "$version" --check
if node "$ROOT/scripts/windows-resources.mjs" 1.2.3 --check; then
  echo 'stale committed resource was accepted' >&2; exit 1
fi
# A failed check must leave the tracked resource unchanged.
cmp "$TMP/one.syso" "$ROOT/daemon/rsrc_windows_amd64.syso"
printf 'module resource-fixture\n\ngo 1.25\n' > "$TMP/go.mod"
printf 'package main\nfunc main() {}\n' > "$TMP/main.go"
cp "$TMP/one.syso" "$TMP/rsrc_windows_amd64.syso"
rm "$TMP/one.syso" "$TMP/two.syso"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -trimpath -ldflags '-buildid=' -o "$TMP/solud.exe" .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -trimpath -ldflags '-buildid=' -o "$TMP/solud-again.exe" .
cmp "$TMP/solud.exe" "$TMP/solud-again.exe"
go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/solud.exe" "$version"
if go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/solud.exe" 1.2.3; then
  echo 'wrong version was accepted' >&2; exit 1
fi
# A new release version must regenerate both fixed and string versions.
node "$ROOT/scripts/windows-resources.mjs" 1.2.3 "$TMP/rsrc_windows_amd64.syso"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -o "$TMP/new.exe" .
go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/new.exe" 1.2.3
rm "$TMP/rsrc_windows_amd64.syso"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -o "$TMP/bare.exe" .
if go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/bare.exe" 1.2.3; then
  echo 'missing resources were accepted' >&2; exit 1
fi
# The Windows-suffixed COFF object must not affect a Linux build.
node "$ROOT/scripts/windows-resources.mjs" 1.2.3 "$TMP/rsrc_windows_amd64.syso"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -o "$TMP/solud-linux" .
echo 'Windows resource generation/link/version/manifest checks passed; Linux build passed'
