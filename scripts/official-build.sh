#!/usr/bin/env bash
# The official SoluTerminal build: the steps .github/workflows/official-build.yml
# runs in the public repository, usable from a local checkout too. It builds
# and checks; it never signs, tags, uploads or publishes. See
# docs/OFFICIAL-BUILD.md.
#
#   scripts/official-build.sh preflight
#       Check that this tree has everything the official app needs (voice,
#       the hosted cloud in the desktop allowlists, the public auth defaults,
#       a release version) and say exactly what is missing.
#   scripts/official-build.sh daemons --out DIR
#       Build the web bundle and cross-compile solud for the five release
#       platforms with the release flags (scripts/release-ldflags.sh); write
#       the binaries, SHA256SUMS and BUILDINFO.txt to DIR (empty or missing).
#   scripts/official-build.sh stage-sidecars --from DIR
#       Verify DIR/SHA256SUMS and put DIR's Windows and Linux daemons where
#       desktop/scripts/prepare-sidecar.js reads them (the repository root),
#       with the sidecar manifest scripts/validate-shell-installer.sh checks.
#   scripts/official-build.sh check-installer --installer EXE --from DIR
#       Unpack the NSIS installer (7z) and require that the daemons inside it
#       are byte for byte DIR's, and that the app names the hosted cloud relay.
#
# Settings, from the environment (the workflow sets them; see the doc):
#   SOLU_SUPABASE_URL, SOLU_SUPABASE_ANON_KEY  public auth defaults baked into solud
#   SOLU_CLOUD_URL, SOLU_CLOUD_APP_URL  optional: a hosted cloud other than the
#                        product's (desktop/src-tauri/hosted-cloud.env and
#                        daemon/hosted_cloud_defaults.go); see resolve_cloud
#   SOLU_EXPECT_VERSION  when set (a tag vX.Y.Z without the v), must equal
#                        desktop/src-tauri/tauri.conf.json's version
#   SOLU_SOURCE_SHA      the commit being built (default: git rev-parse HEAD)
#   SOLU_ALLOW_INCOMPLETE=1  dry runs only: report missing official inputs as
#                        warnings and build anyway. The result is not the
#                        official app and must never be signed.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
if [[ ! -f "$ROOT/scripts/release-ldflags.sh" ]]; then
  echo "error: scripts/release-ldflags.sh is missing; the official build uses the release flags it defines" >&2
  exit 1
fi
. "$ROOT/scripts/release-ldflags.sh"

usage() { sed -n '2,/^set -euo/p' "$0" | sed -n 's/^# \{0,1\}//p'; }
die() { echo "error: $*" >&2; exit 1; }

# The platforms publish.sh ships and update.json lists, with the asset names.
PLATFORMS=(
  "linux-amd64:linux:amd64:solud-linux-amd64"
  "linux-arm64:linux:arm64:solud-linux-arm64"
  "darwin-amd64:darwin:amd64:solud-darwin-amd64"
  "darwin-arm64:darwin:arm64:solud-darwin-arm64"
  "windows-amd64:windows:amd64:solud-windows-amd64.exe"
)

missing=()
need() { missing+=("$1"); }

is_origin() { [[ "$1" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]]; }

# The hosted cloud the build gets, resolved the way desktop/src-tauri/build.rs
# resolves it: SOLU_CLOUD_URL in the environment wins (then only the
# environment is read), otherwise desktop/src-tauri/hosted-cloud.env, the
# product's published values. Sets CLOUD_ORIGIN, CLOUD_APP and CLOUD_FROM
# (env, file or none). When the values come from the file, the daemon takes
# the same product values from daemon/hosted_cloud_defaults.go; when they come
# from the environment, the daemon gets them with -X.
resolve_cloud() {
  CLOUD_ORIGIN="" CLOUD_APP="" CLOUD_FROM="none"
  local env_file=desktop/src-tauri/hosted-cloud.env
  if [[ -v SOLU_CLOUD_URL ]]; then
    CLOUD_FROM="env"
    CLOUD_ORIGIN="${SOLU_CLOUD_URL%/}"
    CLOUD_APP="${SOLU_CLOUD_APP_URL:-}"
  elif [[ -f "$env_file" ]]; then
    CLOUD_FROM="file"
    CLOUD_ORIGIN="$(sed -n 's/^[[:space:]]*SOLU_CLOUD_URL=[[:space:]]*\([^[:space:]]*\).*/\1/p' "$env_file" | head -1)"
    CLOUD_APP="$(sed -n 's/^[[:space:]]*SOLU_CLOUD_APP_URL=[[:space:]]*\([^[:space:]]*\).*/\1/p' "$env_file" | head -1)"
    CLOUD_ORIGIN="${CLOUD_ORIGIN%/}"
  fi
  CLOUD_APP="${CLOUD_APP%/}"
  [[ -n "$CLOUD_APP" ]] || CLOUD_APP="$CLOUD_ORIGIN"
}

preflight() {
  missing=()
  local version origin
  version="$(solu_tauri_version "$ROOT")"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
    die "desktop/src-tauri/tauri.conf.json version is '$version', not X.Y.Z"
  if [[ -n "${SOLU_EXPECT_VERSION:-}" && "$SOLU_EXPECT_VERSION" != "$version" ]]; then
    die "tag v$SOLU_EXPECT_VERSION does not match desktop/src-tauri/tauri.conf.json version $version"
  fi

  # Public auth defaults: both or neither (release-ldflags.sh refuses one).
  if [[ -z "${SOLU_SUPABASE_URL:-}" || -z "${SOLU_SUPABASE_ANON_KEY:-}" ]]; then
    [[ -z "${SOLU_SUPABASE_URL:-}${SOLU_SUPABASE_ANON_KEY:-}" ]] ||
      die "set both SOLU_SUPABASE_URL and SOLU_SUPABASE_ANON_KEY, not one"
    need "public auth defaults: repository variables SOLU_SUPABASE_URL and SOLU_SUPABASE_ANON_KEY are not set"
  fi

  # Hosted cloud: the official app has one.
  resolve_cloud
  origin="$CLOUD_ORIGIN"
  case "$CLOUD_FROM" in
    none)
      need "hosted cloud: desktop/src-tauri/hosted-cloud.env (the product's cloud) is not in the tree and SOLU_CLOUD_URL is not set"
      origin="" ;;
    env)
      is_origin "$origin" || die "SOLU_CLOUD_URL='$origin' must be an https origin with no path; the official app has a hosted cloud"
      is_origin "$CLOUD_APP" || die "SOLU_CLOUD_APP_URL='$CLOUD_APP' must be an https origin with no path"
      # -X silently ignores a name that does not exist.
      local name
      for name in defaultCloudHTTPBase publicAppURL; do
        grep -Eq "^[[:space:]]*(var[[:space:]]+)?${name}[[:space:]]+string" daemon/*.go ||
          need "hosted cloud: no package-level 'var $name string' in daemon/*.go for -X to set"
      done ;;
    file)
      is_origin "$origin" || die "desktop/src-tauri/hosted-cloud.env: SOLU_CLOUD_URL='$origin' must be an https origin with no path"
      is_origin "$CLOUD_APP" || die "desktop/src-tauri/hosted-cloud.env: SOLU_CLOUD_APP_URL='$CLOUD_APP' must be an https origin with no path"
      [[ -f daemon/hosted_cloud_defaults.go ]] ||
        need "hosted cloud: daemon/hosted_cloud_defaults.go (the daemon's product cloud) is not in the tree"
      grep -Fq -- "\"$origin\"" daemon/hosted_cloud_defaults.go 2>/dev/null ||
        need "hosted cloud: daemon/hosted_cloud_defaults.go does not use the shell's cloud $origin" ;;
  esac
  # build.rs only warns when the shell's allowlists do not name the cloud
  # origin; the cloud's pages would then load without the shell's plugins, so
  # the official build refuses instead.
  if [[ -n "$origin" ]]; then
    grep -Fq -- "$origin" desktop/src-tauri/tauri.conf.json ||
      need "hosted cloud: desktop/src-tauri/tauri.conf.json's CSP does not allow $origin"
    local cap
    for cap in desktop/src-tauri/capabilities/*.json; do
      grep -q '"remote"' "$cap" || continue
      grep -Fq -- "\"$origin\"" "$cap" ||
        need "hosted cloud: $cap does not list $origin under remote.urls"
    done
  fi

  # Voice dictation is part of the official app.
  local voice_pkg
  voice_pkg="$(node -e 'const p=require(process.argv[1]); const d={...p.devDependencies,...p.dependencies}; process.stdout.write(d.soluvoice||"")' "$ROOT/web/package.json")"
  if [[ -z "$voice_pkg" ]]; then
    need "voice: web/package.json does not depend on soluvoice"
  elif [[ "$voice_pkg" == file:* && ! -f "web/${voice_pkg#file:}" ]]; then
    need "voice: web/package.json depends on $voice_pkg, which is not in the tree"
  fi
  [[ -f web/src/private/voice.register.tsx || -f web/src/private/voice.register.ts ]] ||
    need "voice: web/src/private/voice.register.tsx (the dictation UI registration) is missing"
  [[ -f daemon/voice.go ]] || need "voice: daemon/voice.go (dictation and transcription) is missing"
  [[ -d shared/voiceclean ]] || need "voice: shared/voiceclean is missing"

  if [[ "${#missing[@]}" -eq 0 ]]; then
    echo "==> preflight: official inputs present (version $version, cloud $origin)"
    return 0
  fi
  local level=error
  [[ "${SOLU_ALLOW_INCOMPLETE:-0}" == 1 ]] && level=warning
  echo "$level: this tree cannot build the official app; missing:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    printf '::%s title=official build input missing::%s\n' "$level" "${missing[@]}"
  fi
  if [[ "$level" == warning ]]; then
    echo "warning: SOLU_ALLOW_INCOMPLETE=1: building anyway; the result is not the official app" >&2
    return 0
  fi
  exit 1
}

require_tool_versions() {
  # Same toolchain as the release recipe: the go.mod toolchain line, and none
  # of the ambient settings that change code generation or build metadata.
  local go_toolchain setting value
  go_toolchain="$(sed -n 's/^toolchain \(go[0-9][0-9.]*\)$/\1/p' daemon/go.mod | head -1)"
  [[ -n "$go_toolchain" ]] || die "daemon/go.mod has no toolchain line"
  export GOTOOLCHAIN="$go_toolchain"
  unset GOFLAGS GOAMD64 GOARM64 GOEXPERIMENT GO386 GOARM
  GO_VERSION="$(go env GOVERSION)"
  [[ "$GO_VERSION" == "$go_toolchain" ]] || die "go reports $GO_VERSION, expected $go_toolchain from daemon/go.mod"
  for setting in GOFLAGS GOAMD64 GOARM64 GOEXPERIMENT; do
    value="$(go env "$setting")"
    case "$setting:$value" in
      GOFLAGS:|GOEXPERIMENT:|GOAMD64:|GOAMD64:v1|GOARM64:|GOARM64:v8.0) ;;
      *) die "go env $setting=$value (set via 'go env -w'?) differs from the release default" ;;
    esac
  done
}

daemons() {
  local out=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --out) out="${2:-}"; shift 2 ;;
      --out=*) out="${1#--out=}"; shift ;;
      *) die "daemons: unknown argument: $1" ;;
    esac
  done
  [[ -n "$out" ]] || die "daemons: --out DIR is required"
  mkdir -p "$out"
  out="$(cd "$out" && pwd)"
  [[ -z "$(ls -A "$out")" ]] || die "output directory $out is not empty"

  preflight
  require_tool_versions
  local version source_sha
  version="$(solu_tauri_version "$ROOT")"
  source_sha="${SOLU_SOURCE_SHA:-$(git rev-parse HEAD 2>/dev/null || true)}"
  [[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || die "SOLU_SOURCE_SHA must be the 40-hex commit being built (no git checkout found)"
  if git rev-parse --git-dir >/dev/null 2>&1; then
    [[ "$(git rev-parse HEAD)" == "$source_sha" ]] || die "SOLU_SOURCE_SHA $source_sha is not the checked-out commit"
    [[ -z "$(git status --porcelain --untracked-files=all -- daemon web shared desktop scripts)" ]] ||
      die "the tree has changes or untracked files under daemon/web/shared/desktop/scripts; the official build is of a clean commit"
  fi
  if [[ "${SOLU_ALLOW_UNSIGNED_UPDATE_MANIFEST:-0}" == 1 ]]; then
    die "SOLU_ALLOW_UNSIGNED_UPDATE_MANIFEST=1 builds a daemon that accepts unsigned updates"
  fi

  local ldflags pubkey cloud_flags=""
  resolve_cloud
  pubkey="$(solu_effective_update_manifest_pubkey)"
  # A cloud from the environment overrides the daemon's source defaults too,
  # so the shell and its daemons always name the same cloud.
  if [[ "$CLOUD_FROM" == env ]]; then
    cloud_flags="-X main.defaultCloudHTTPBase=$CLOUD_ORIGIN -X main.publicAppURL=$CLOUD_APP"
  fi
  # The release flags (version, pane runtime digest, source commit, clean
  # tree, release channel, update key, auth defaults, public flavour, empty
  # build ID and -B). The official app is always the public flavour: it
  # follows the public releases repo and the signed manifest must say so.
  # Verify the committed resource before computing the clean-source stamp.
  node "$ROOT/scripts/windows-resources.mjs" "$version" --check
  ldflags="$(SOLU_BUILD_CHANNEL=release SOLU_SOURCE_SHA="$source_sha" SOLU_BUILD_FLAVOR=public \
    solu_daemon_ldflags "$version" "$cloud_flags" "$ROOT")"
  case " $ldflags " in
    *" -X main.buildDirty=0 "*) ;;
    *) die "the source tree is not provably clean (buildDirty is not 0); build from a git checkout of the commit" ;;
  esac
  case " $ldflags " in
    *" -X main.buildFlavorStamp=solu-build-flavor=public "*) ;;
    *) die "the release flags do not stamp the public flavour" ;;
  esac

  echo "==> building web bundle"
  export SOLU_SOURCE_SHA="$source_sha"
  (cd web && npm ci --no-audit --no-fund)
  (cd web && npm run build)
  rm -rf daemon/webdist
  cp -r web/dist daemon/webdist
  solu_require_webdist_json daemon/webdist "$version" "$source_sha"
  # (Names differ from the variables release-ldflags.sh's functions assign:
  # they are not local there, so they would overwrite ours.)
  local web_marker web_digest
  web_marker="$(solu_extract_webdist_marker daemon/webdist/webdist.json | head -1)"
  web_digest="$(printf '%s\n' "$web_marker" | sed -n 's/^solu-webdist v=[^ ]* src=[^ ]* sha256=\([^ ]*\)$/\1/p')"
  [[ "$web_digest" =~ ^[0-9a-f]{64}$ ]] || die "could not read the web bundle digest from daemon/webdist/webdist.json"

  local entry key goos goarch artifact built=()
  for entry in "${PLATFORMS[@]}"; do
    IFS=':' read -r key goos goarch artifact <<<"$entry"
    echo "==> cross-compiling $key → $artifact"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -C "$ROOT/daemon" -buildvcs=false -trimpath -ldflags "$ldflags" -o "$out/$artifact" .
    built+=("$artifact")
  done

  go run -C "$ROOT/daemon" ./cmd/check-winres "$out/solud-windows-amd64.exe" "$version"

  echo "==> checking what each binary carries"
  local bin needle symbols
  for artifact in "${built[@]}"; do
    bin="$out/$artifact"
    solu_require_webdist_parity "$bin" "$version" "$source_sha" "$web_digest"
    for needle in "$version" "$pubkey" ${CLOUD_ORIGIN:+"$CLOUD_ORIGIN"} ${CLOUD_APP:+"$CLOUD_APP"} \
        ${SOLU_SUPABASE_URL:+"$SOLU_SUPABASE_URL"} ${SOLU_SUPABASE_ANON_KEY:+"$SOLU_SUPABASE_ANON_KEY"}; do
      grep -aFq -- "$needle" "$bin" || die "$artifact does not contain an expected build setting (${needle:0:40})"
    done
    if [[ "$CLOUD_FROM" == env ]]; then
      symbols="$(go tool nm "$bin")"
      if ! grep -Eq ' main\.defaultCloudHTTPBase$' <<<"$symbols" || ! grep -Eq ' main\.publicAppURL$' <<<"$symbols"; then
        die "$artifact lacks main.defaultCloudHTTPBase or main.publicAppURL; the cloud -X settings did not take"
      fi
    fi
  done
  for artifact in "${built[@]}"; do
    [[ "$(solu_binary_flavor "$out/$artifact")" == public ]] || die "$artifact is stamped as the team build"
  done

  (cd "$out" && sha256sum "${built[@]}" | LC_ALL=C sort -k2) >"$out/SHA256SUMS"
  {
    echo "version $version"
    echo "source_sha $source_sha"
    echo "go $GO_VERSION"
    echo "node $(node --version)"
    echo "npm $(npm --version)"
    echo "webdist $web_marker"
    echo "update_manifest_pubkey $pubkey"
    echo "cloud_url ${CLOUD_ORIGIN:-none} (from $CLOUD_FROM)"
    echo "cloud_app_url ${CLOUD_APP:-none}"
    echo "supabase_url ${SOLU_SUPABASE_URL:-none}"
    echo "supabase_anon_key_sha256 $(if [[ -n "${SOLU_SUPABASE_ANON_KEY:-}" ]]; then printf '%s' "$SOLU_SUPABASE_ANON_KEY" | sha256sum | awk '{print $1}'; else echo none; fi)"
    echo "incomplete $([[ "${#missing[@]}" -gt 0 ]] && echo yes || echo no)"
    # Every -X value except the anon key (public, but long), one per line.
    printf '%s\n' "$ldflags" | tr ' ' '\n' | grep '^main\.' | grep -v '^main\.defaultSupaAnonKey=' | sed 's/^/ldflag /'
  } >"$out/BUILDINFO.txt"
  echo "==> built ${#built[@]} daemons in $out"
  cat "$out/SHA256SUMS"
}

verify_sums() { # DIR
  (cd "$1" && sha256sum -c --strict SHA256SUMS >/dev/null) || die "$1 does not match its SHA256SUMS"
}

stage_sidecars() {
  local from=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --from) from="${2:-}"; shift 2 ;;
      *) die "stage-sidecars: unknown argument: $1" ;;
    esac
  done
  [[ -d "$from" ]] || die "stage-sidecars: --from DIR (the daemons build output) is required"
  verify_sums "$from"
  local version web_marker
  version="$(solu_tauri_version "$ROOT")"
  grep -q "^version $version\$" "$from/BUILDINFO.txt" || die "$from was built for another version than $version"
  cp "$from/solud-windows-amd64.exe" "$ROOT/solud.exe"
  cp "$from/solud-linux-amd64" "$ROOT/solud-linux"
  cp "$from/solud-linux-arm64" "$ROOT/solud-linux-arm64"
  web_marker="$(solu_extract_webdist_marker "$ROOT/solud.exe" | head -1)"
  [[ -n "$web_marker" ]] || die "solud.exe has no web provenance marker"
  # The manifest build-sidecar-binaries.sh writes; the installer validator
  # checks the staged sidecars against it.
  {
    printf 'version %s\n' "$version"
    printf 'webdist_version %s\n' "$(printf '%s\n' "$web_marker" | sed -n 's/^solu-webdist v=\([^ ]*\) src=.*/\1/p')"
    printf 'webdist_source %s\n' "$(printf '%s\n' "$web_marker" | sed -n 's/^solu-webdist v=[^ ]* src=\([^ ]*\) sha256=.*/\1/p')"
    printf 'update_manifest_pubkey %s\n' "$(awk '$1 == "update_manifest_pubkey" {print $2}' "$from/BUILDINFO.txt")"
    (cd "$ROOT" && sha256sum solud.exe solud-linux solud-linux-arm64)
  } >"$ROOT/.soluterminal-shell-sidecars.sha256"
  echo "==> staged solud.exe, solud-linux and solud-linux-arm64 from $from"
}

check_installer() {
  local installer="" from=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --installer) installer="${2:-}"; shift 2 ;;
      --from) from="${2:-}"; shift 2 ;;
      *) die "check-installer: unknown argument: $1" ;;
    esac
  done
  [[ -f "$installer" ]] || die "check-installer: --installer EXE is required"
  [[ -d "$from" ]] || die "check-installer: --from DIR (the daemons build output) is required"
  verify_sums "$from"
  local tool dir
  tool="${SOLU_INSTALLER_EXTRACT_TOOL:-$(command -v 7z || command -v 7za || command -v 7zz || true)}"
  [[ -n "$tool" ]] || die "check-installer needs 7z (or SOLU_INSTALLER_EXTRACT_TOOL)"
  dir="$(mktemp -d)"
  "$tool" x -y "-o$dir" "$installer" >/dev/null || die "could not unpack $installer"

  # payload name inside the installer → asset name in the daemons build
  local pair payload asset found want got
  for pair in "solud-linux-amd64.gz:solud-linux-amd64" "solud-linux-arm64.gz:solud-linux-arm64"; do
    payload="${pair%%:*}"; asset="${pair#*:}"
    found="$(find "$dir" -type f -name "$payload" -print -quit)"
    [[ -n "$found" ]] || die "the installer has no $payload"
    want="$(awk -v f="$asset" '$2 == f || $2 == "*"f {print $1}' "$from/SHA256SUMS")"
    got="$(gzip -dc "$found" | sha256sum | awk '{print $1}')"
    [[ -n "$want" && "$got" == "$want" ]] || die "the installer's $payload is not the built $asset ($got != $want)"
  done
  found="$(find "$dir" -type f -name 'solud*.exe' -print -quit)"
  [[ -n "$found" ]] || die "the installer has no solud*.exe sidecar"
  want="$(awk '$2 == "solud-windows-amd64.exe" || $2 == "*solud-windows-amd64.exe" {print $1}' "$from/SHA256SUMS")"
  got="$(sha256sum "$found" | awk '{print $1}')"
  [[ -n "$want" && "$got" == "$want" ]] || die "the installer's $(basename "$found") is not the built solud-windows-amd64.exe"

  # The shell bakes the relay in at build time (build.rs); it is long enough to
  # stay readable in a release binary.
  local relay="" exe named=0 apps=0
  resolve_cloud
  if [[ -n "$CLOUD_ORIGIN" ]]; then
    relay="wss://${CLOUD_ORIGIN#https://}/relay"
    while IFS= read -r -d '' exe; do
      apps=$((apps + 1))
      grep -aFq -- "$relay" "$exe" && named=1
    done < <(find "$dir" -type f -iname '*.exe' ! -iname 'solud*' ! -iname 'uninstall*' -print0)
    [[ "$apps" -gt 0 ]] || die "could not find the desktop app executable in the installer"
    [[ "$named" -eq 1 ]] || die "the desktop app in the installer does not name the hosted cloud relay $relay"
  fi
  rm -rf "$dir"
  echo "==> installer carries the built daemons byte for byte${relay:+ and the relay $relay}"
}

command="${1:-}"
[[ $# -gt 0 ]] && shift
case "$command" in
  preflight) preflight ;;
  daemons) daemons "$@" ;;
  stage-sidecars) stage_sidecars "$@" ;;
  check-installer) check_installer "$@" ;;
  -h|--help|'') usage ;;
  *) echo "error: unknown command: $command" >&2; usage >&2; exit 2 ;;
esac
