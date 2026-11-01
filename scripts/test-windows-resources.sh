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
# A four-part team candidate is generated in staged build source. The pinned
# resource in the source tree remains byte-identical to the three-part release.
fixture="$TMP/team-fixture"
mkdir -p "$fixture/daemon" "$fixture/scripts" "$fixture/shared"
cp "$TMP/go.mod" "$TMP/main.go" "$fixture/daemon/"
printf 'package main\n' >"$fixture/daemon/team_source_private.go"
cp "$ROOT/scripts/windows-resources.mjs" "$fixture/scripts/"
cp "$ROOT/daemon/rsrc_windows_amd64.syso" "$fixture/daemon/"
. "$ROOT/scripts/release-ldflags.sh"
three_source="$(SOLU_BUILD_FLAVOR=team solu_windows_build_source "$fixture" "$version" "$TMP/three-build")"
[[ "$three_source" == "$fixture/daemon" ]]
team_version="${version}.1"
# Old failed builds are swept on the next build, with a strict suffix and
# scratch scope. Fresh concurrent sources and unrelated paths stay intact.
mkdir -p "$TMP/team-build/windows-source.old123" "$TMP/team-build/windows-source.fresh1" "$TMP/team-build/windows-source.too-long" "$TMP/other/windows-source.old123"
touch -d '2 days ago' "$TMP/team-build/windows-source.old123" "$TMP/team-build/windows-source.too-long" "$TMP/other/windows-source.old123"
ln -s "$TMP/other/windows-source.old123" "$TMP/team-build/windows-source.link12"
team_source="$(SOLU_BUILD_FLAVOR=team solu_windows_build_source "$fixture" "$team_version" "$TMP/team-build")"
[[ "$team_source" != "$fixture/daemon" ]]
[[ ! -e "$TMP/team-build/windows-source.old123" ]]
[[ -d "$TMP/team-build/windows-source.fresh1" && -d "$TMP/team-build/windows-source.too-long" && -d "$TMP/other/windows-source.old123" && -L "$TMP/team-build/windows-source.link12" ]]
# Preparation failure cleans its just-created copy immediately.
if SOLU_BUILD_FLAVOR=team solu_windows_build_source "$fixture" '999999.2.3.4' "$TMP/failed-build"; then
  echo 'invalid resource version was accepted' >&2; exit 1
fi
[[ -z "$(find "$TMP/failed-build" -mindepth 1 -maxdepth 1 -type d -name 'windows-source.??????' -print)" ]]
if solu_cleanup_windows_build_source "$fixture" "$TMP/team-build/windows-source.too-long/daemon"; then
  echo 'cleanup accepted a broader path' >&2; exit 1
fi
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -C "$team_source" -buildvcs=false -trimpath -o "$TMP/team.exe" .
go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/team.exe" "$team_version"
if go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/team.exe" "${version}.2"; then
  echo 'wrong fourth version component was accepted' >&2; exit 1
fi
node "$fixture/scripts/windows-resources.mjs" "$team_version" --check "$team_source/rsrc_windows_amd64.syso"
if node "$fixture/scripts/windows-resources.mjs" "${version}.2" --check "$team_source/rsrc_windows_amd64.syso"; then
  echo 'mismatched staged resource was accepted' >&2; exit 1
fi
if SOLU_BUILD_FLAVOR=public solu_windows_build_source "$fixture" "$team_version" "$TMP/public-build"; then
  echo 'public build accepted an uncommitted resource' >&2; exit 1
fi
cmp "$ROOT/daemon/rsrc_windows_amd64.syso" "$fixture/daemon/rsrc_windows_amd64.syso"
# Failed checks must not rewrite the generated four-part resource either.
node "$fixture/scripts/windows-resources.mjs" "$team_version" --check "$team_source/rsrc_windows_amd64.syso"
solu_cleanup_windows_build_source "$fixture" "$team_source"
[[ ! -d "${team_source%/daemon}" ]]
solu_cleanup_windows_build_source "$fixture" "$three_source"
cmp "$ROOT/daemon/rsrc_windows_amd64.syso" "$fixture/daemon/rsrc_windows_amd64.syso"
rm "$TMP/rsrc_windows_amd64.syso"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -o "$TMP/bare.exe" .
if go run -C "$ROOT/daemon" ./cmd/check-winres "$TMP/bare.exe" 1.2.3; then
  echo 'missing resources were accepted' >&2; exit 1
fi
# The Windows-suffixed COFF object must not affect a Linux build.
node "$ROOT/scripts/windows-resources.mjs" 1.2.3 "$TMP/rsrc_windows_amd64.syso"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -C "$TMP" -buildvcs=false -o "$TMP/solud-linux" .
echo 'Windows resource generation/link/version/manifest checks passed; Linux build passed'
