#!/usr/bin/env bash
# Validate a Tauri/NSIS shell installer before publish.sh advertises it in
# update.json. The installer is opaque from Linux unless archive tooling is
# available, so this also validates the sidecar binaries that were staged for
# the Windows build.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
. "$ROOT/scripts/release-ldflags.sh"

INSTALLER=""
RELEASE_VERSION=""
SHELL_VERSION_ARG=""
EXPECT_WEB_VERSION=""
EXPECT_WEB_SOURCE=""
EXPECT_FLAVOR=""
PRINT_VERSION=0
ALLOW_MISSING_SIDECARS=0
EXTRACT_CHECK=0
SIDECAR_ROOTS=()
REQUIRE_BINDING=0

fail() {
  echo "error: $*" >&2
  exit 1
}

warn() {
  echo "warning: $*" >&2
}

usage() {
  cat >&2 <<'EOF'
Usage: scripts/validate-shell-installer.sh --installer PATH --release-version X.Y.Z [options]

Options:
  --shell-version X.Y.Z       Expected shell version; must match installer name.
  --expect-web-version X.Y.Z  Expected embedded web bundle version (defaults
                              to the installer filename version).
  --expect-web-source SHA     Expected embedded web bundle source SHA.
  --expect-flavor team|public Every bundled daemon must be this flavour
                              (daemon/update_flavor.go).
  --sidecar-root PATH         Root containing solud.exe and solud-linux.
  --require-payload-binding   With --extract-check and the sidecar hash
                              manifest: fail unless the installer's bundled
                              daemons are the staged sidecars, by sha256
                              (publish.sh passes it with --sidecar-root).
  --allow-missing-sidecars    Deprecated; refused because release validation
                              must inspect provenance-bearing sidecars.
  --extract-check             Unpack the .exe and assert the bundled
                              solud-linux inside it is a non-empty ELF
                              (needs 7z/7za/7zr/7zz/innoextract; SOLU_INSTALLER_EXTRACT_TOOL
                              overrides the extractor). Best-effort by default.
  --print-version             Print the validated shell version to stdout.
EOF
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --installer)
      [[ "$#" -ge 2 ]] || fail "--installer requires a path"
      INSTALLER="$2"
      shift 2
      ;;
    --installer=*)
      INSTALLER="${1#--installer=}"
      shift
      ;;
    --release-version)
      [[ "$#" -ge 2 ]] || fail "--release-version requires a version"
      RELEASE_VERSION="$2"
      shift 2
      ;;
    --release-version=*)
      RELEASE_VERSION="${1#--release-version=}"
      shift
      ;;
    --shell-version)
      [[ "$#" -ge 2 ]] || fail "--shell-version requires a version"
      SHELL_VERSION_ARG="$2"
      shift 2
      ;;
    --shell-version=*)
      SHELL_VERSION_ARG="${1#--shell-version=}"
      shift
      ;;
    --expect-web-version)
      [[ "$#" -ge 2 ]] || fail "--expect-web-version requires a version"
      EXPECT_WEB_VERSION="$2"
      shift 2
      ;;
    --expect-web-version=*)
      EXPECT_WEB_VERSION="${1#--expect-web-version=}"
      shift
      ;;
    --expect-web-source)
      [[ "$#" -ge 2 ]] || fail "--expect-web-source requires a SHA"
      EXPECT_WEB_SOURCE="$2"
      shift 2
      ;;
    --expect-web-source=*)
      EXPECT_WEB_SOURCE="${1#--expect-web-source=}"
      shift
      ;;
    --expect-flavor)
      [[ "$#" -ge 2 ]] || fail "--expect-flavor requires team or public"
      EXPECT_FLAVOR="$2"
      shift 2
      ;;
    --expect-flavor=*)
      EXPECT_FLAVOR="${1#--expect-flavor=}"
      shift
      ;;
    --sidecar-root)
      [[ "$#" -ge 2 ]] || fail "--sidecar-root requires a path"
      SIDECAR_ROOTS+=("$2")
      shift 2
      ;;
    --sidecar-root=*)
      SIDECAR_ROOTS+=("${1#--sidecar-root=}")
      shift
      ;;
    --allow-missing-sidecars)
      ALLOW_MISSING_SIDECARS=1
      shift
      ;;
    --extract-check)
      EXTRACT_CHECK=1
      shift
      ;;
    --require-payload-binding)
      REQUIRE_BINDING=1
      shift
      ;;
    --print-version)
      PRINT_VERSION=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$INSTALLER" ]] || fail "--installer is required"
[[ -n "$RELEASE_VERSION" ]] || fail "--release-version is required"
[[ "$RELEASE_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "release version must be X.Y.Z: $RELEASE_VERSION"
[[ -f "$INSTALLER" ]] || fail "--shell-installer path not found: $INSTALLER"
[[ -r "$INSTALLER" ]] || fail "--shell-installer path is not readable: $INSTALLER"

SHELL_NAME="$(basename "$INSTALLER")"
if [[ "$SHELL_NAME" =~ ^soluterminal_([0-9]+\.[0-9]+\.[0-9]+)_x64-setup\.exe$ ]]; then
  SHELL_VERSION="${BASH_REMATCH[1]}"
else
  fail "installer filename must be soluterminal_<X.Y.Z>_x64-setup.exe: $SHELL_NAME"
fi

if [[ -n "$SHELL_VERSION_ARG" && "$SHELL_VERSION_ARG" != "$SHELL_VERSION" ]]; then
  fail "--shell-version ($SHELL_VERSION_ARG) does not match installer filename version ($SHELL_VERSION)"
fi
EXPECT_WEB_VERSION="${EXPECT_WEB_VERSION:-$SHELL_VERSION}"
[[ "$EXPECT_WEB_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "expected web version must be X.Y.Z: $EXPECT_WEB_VERSION"
if [[ -n "$EXPECT_WEB_SOURCE" && ! "$EXPECT_WEB_SOURCE" =~ ^[0-9a-fA-F]+$ ]]; then
  fail "expected web source must be a hexadecimal SHA: $EXPECT_WEB_SOURCE"
fi
if [[ "$ALLOW_MISSING_SIDECARS" -eq 1 ]]; then
  fail "--allow-missing-sidecars is incompatible with provenance-enforced installer validation; provide sidecars (and use --extract-check for the bundled payload)"
fi

version_gt() {
  local left="$1" right="$2"
  local la lb lc ra rb rc
  IFS=. read -r la lb lc <<<"$left"
  IFS=. read -r ra rb rc <<<"$right"
  ((10#$la > 10#$ra)) && return 0
  ((10#$la < 10#$ra)) && return 1
  ((10#$lb > 10#$rb)) && return 0
  ((10#$lb < 10#$rb)) && return 1
  ((10#$lc > 10#$rc))
}

if version_gt "$SHELL_VERSION" "$RELEASE_VERSION"; then
  fail "installer version ($SHELL_VERSION) is newer than release version ($RELEASE_VERSION)"
fi
if [[ "$SHELL_VERSION" != "$RELEASE_VERSION" ]]; then
  warn "installer version ($SHELL_VERSION) differs from release version ($RELEASE_VERSION); update.json will advertise shell.version=$SHELL_VERSION"
fi

file_size() {
  if stat -c '%s' "$1" >/dev/null 2>&1; then
    stat -c '%s' "$1"
  else
    stat -f '%z' "$1"
  fi
}

file_mtime() {
  if stat -c '%Y' "$1" >/dev/null 2>&1; then
    stat -c '%Y' "$1"
  else
    stat -f '%m' "$1"
  fi
}

size="$(file_size "$INSTALLER")"
if (( size < 1048576 )); then
  fail "installer is unexpectedly small (${size} bytes): $INSTALLER"
fi

magic="$(head -c 2 "$INSTALLER" || true)"
[[ "$magic" == "MZ" ]] || fail "installer is not a Windows PE executable (missing MZ header): $INSTALLER"

if command -v file >/dev/null 2>&1; then
  desc="$(file -b "$INSTALLER" || true)"
  case "$desc" in
    *PE32*|*Windows*) ;;
    *) warn "file(1) did not identify a Windows executable: $desc" ;;
  esac
  case "$desc" in
    *Nullsoft*|*NSIS*) ;;
    *) warn "file(1) could not confirm an NSIS/Nullsoft installer: $desc" ;;
  esac
fi

dedupe_roots() {
  local root seen
  for root in "${SIDECAR_ROOTS[@]}" "$ROOT"; do
    [[ -n "$root" ]] || continue
    root="$(cd "$root" 2>/dev/null && pwd || true)"
    [[ -n "$root" ]] || continue
    case " ${seen:-} " in
      *" $root "*) ;;
      *)
        printf '%s\n' "$root"
        seen="${seen:-} $root"
        ;;
    esac
  done
}

installer_abs="$(cd "$(dirname "$INSTALLER")" && pwd)/$SHELL_NAME"
case "$installer_abs" in
  */desktop/src-tauri/target/release/bundle/nsis/*)
    inferred="${installer_abs%%/desktop/src-tauri/target/release/bundle/nsis/*}"
    SIDECAR_ROOTS+=("$inferred")
    ;;
esac

find_first_existing() {
  local candidate
  for candidate in "$@"; do
    if [[ -s "$candidate" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

WINDOWS_SIDECAR=""
LINUX_SIDECAR=""
while IFS= read -r sidecar_root; do
  windows_candidate="$(find_first_existing \
    "$sidecar_root/solud.exe" \
    "$sidecar_root/desktop/src-tauri/binaries/solud-x86_64-pc-windows-msvc.exe" \
    "$sidecar_root/desktop/src-tauri/binaries/solud-x86_64-pc-windows-msvc" || true)"
  linux_candidate="$(find_first_existing \
    "$sidecar_root/solud-linux" \
    "$sidecar_root/desktop/src-tauri/resources/solud-linux" || true)"
  if [[ -n "$windows_candidate" && -n "$linux_candidate" ]]; then
    WINDOWS_SIDECAR="$windows_candidate"
    LINUX_SIDECAR="$linux_candidate"
    break
  fi
done < <(dedupe_roots)

if [[ -z "$WINDOWS_SIDECAR" || -z "$LINUX_SIDECAR" ]]; then
  fail "could not find non-empty solud.exe and solud-linux sidecars near the installer; run desktop/scripts/build-sidecar-binaries.sh before cargo tauri build"
fi

require_marker() {
  local file="$1"
  local marker="$2"
  local label="$3"
  grep -aFq -- "$marker" "$file" || fail "$label marker missing from $(basename "$file")"
}

require_magic() {
  local file="$1"
  local magic="$2"
  local label="$3"
  local got
  got="$(head -c "${#magic}" "$file" || true)"
  [[ "$got" == "$magic" ]] || fail "$label has unexpected binary header: $(basename "$file")"
}

require_magic "$WINDOWS_SIDECAR" "MZ" "Windows sidecar"
require_magic "$LINUX_SIDECAR" "$(printf '\177ELF')" "Linux sidecar"

for sidecar in "$WINDOWS_SIDECAR" "$LINUX_SIDECAR"; do
  require_marker "$sidecar" "$SHELL_VERSION" "shell version $SHELL_VERSION"
  solu_require_webdist_parity "$sidecar" "$EXPECT_WEB_VERSION" "$EXPECT_WEB_SOURCE" || exit 1
done

# Even when validating a carried-forward installer (no current-release source
# expectation), every daemon inside that installer must contain the exact same
# web build. Resolve the staged Windows marker as the reference, then enforce
# its source + digest across the staged Linux daemon and all extracted payloads.
REFERENCE_WEB_MARKER="$(solu_extract_webdist_marker "$WINDOWS_SIDECAR" | head -1)"
RESOLVED_WEB_SOURCE="${EXPECT_WEB_SOURCE:-$(printf '%s\n' "$REFERENCE_WEB_MARKER" | sed -n 's/^solu-webdist v=[^ ]* src=\([^ ]*\) sha256=.*/\1/p')}"
RESOLVED_WEB_DIGEST="$(printf '%s\n' "$REFERENCE_WEB_MARKER" | sed -n 's/^solu-webdist v=[^ ]* src=[^ ]* sha256=\([^ ]*\)$/\1/p')"
for sidecar in "$WINDOWS_SIDECAR" "$LINUX_SIDECAR"; do
  solu_require_webdist_parity "$sidecar" "$EXPECT_WEB_VERSION" "$RESOLVED_WEB_SOURCE" "$RESOLVED_WEB_DIGEST" || exit 1
done

solu_load_supabase_defaults "$ROOT"
if [[ "${SOLU_ALLOW_NO_AUTH_SIDECAR:-0}" != "1" ]]; then
  solu_require_supabase_defaults "shell installer sidecar validation"
  for sidecar in "$WINDOWS_SIDECAR" "$LINUX_SIDECAR"; do
    require_marker "$sidecar" "$SOLU_SUPABASE_URL" "Supabase URL"
    require_marker "$sidecar" "$SOLU_SUPABASE_ANON_KEY" "Supabase anon key"
  done
fi

effective_pubkey="$(solu_effective_update_manifest_pubkey)"
if [[ -n "$effective_pubkey" ]]; then
  for sidecar in "$WINDOWS_SIDECAR" "$LINUX_SIDECAR"; do
    require_marker "$sidecar" "$effective_pubkey" "update manifest public key"
  done
elif [[ "${SOLU_ALLOW_UNSIGNED_UPDATE_MANIFEST:-0}" != "1" ]]; then
  fail "update manifest public key is empty"
fi

case "$EXPECT_FLAVOR" in
  '' | team | public) ;;
  *) fail "--expect-flavor must be team or public, not '$EXPECT_FLAVOR'" ;;
esac
if [[ -n "$EXPECT_FLAVOR" ]]; then
  for sidecar in "$WINDOWS_SIDECAR" "$LINUX_SIDECAR"; do
    got_flavor="$(solu_binary_flavor "$sidecar")"
    [[ "$got_flavor" == "$EXPECT_FLAVOR" ]] ||
      fail "$sidecar is a $got_flavor build; this ${EXPECT_FLAVOR} release needs a ${EXPECT_FLAVOR} installer (build its sidecars with SOLU_BUILD_FLAVOR=${EXPECT_FLAVOR})"
  done
fi

SIDECAR_MANIFEST="$(dirname "$WINDOWS_SIDECAR")/.soluterminal-shell-sidecars.sha256"
if [[ -f "$SIDECAR_MANIFEST" ]]; then
  manifest_version="$(awk '$1 == "version" {print $2}' "$SIDECAR_MANIFEST" | tail -1)"
  [[ "$manifest_version" == "$SHELL_VERSION" ]] || fail "sidecar manifest version ($manifest_version) does not match installer version ($SHELL_VERSION)"
  manifest_web_version="$(awk '$1 == "webdist_version" {print $2}' "$SIDECAR_MANIFEST" | tail -1)"
  [[ "$manifest_web_version" == "$EXPECT_WEB_VERSION" ]] || fail "sidecar manifest webdist_version ($manifest_web_version) does not match expected web version ($EXPECT_WEB_VERSION)"
  manifest_web_source="$(awk '$1 == "webdist_source" {print $2}' "$SIDECAR_MANIFEST" | tail -1)"
  [[ "$manifest_web_source" == "$RESOLVED_WEB_SOURCE" ]] || fail "sidecar manifest webdist_source ($manifest_web_source) does not match embedded web source ($RESOLVED_WEB_SOURCE)"
  manifest_pubkey="$(awk '$1 == "update_manifest_pubkey" {print $2}' "$SIDECAR_MANIFEST" | tail -1)"
  [[ "$manifest_pubkey" == "$effective_pubkey" ]] || fail "sidecar manifest update pubkey does not match effective publish pubkey"
  manifest_flavor="$(awk '$1 == "flavor" {print $2}' "$SIDECAR_MANIFEST" | tail -1)"
  if [[ -n "$EXPECT_FLAVOR" && -n "$manifest_flavor" && "$manifest_flavor" != "$EXPECT_FLAVOR" ]]; then
    fail "sidecar manifest flavor ($manifest_flavor) does not match this ${EXPECT_FLAVOR} release"
  fi
  manifest_dir="$(dirname "$SIDECAR_MANIFEST")"
  # A GNU checksum line uses one separator and a text (' ') or binary ('*')
  # mode marker. The latter is the default on Windows. Avoid awk interval
  # expressions so this also works with older system awk implementations.
  (cd "$manifest_dir" && awk 'length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/ && substr($0, 65, 2) ~ /^ [ *]$/ {print}' "$(basename "$SIDECAR_MANIFEST")" | sha256sum -c --ignore-missing >/dev/null) ||
    fail "sidecar hashes do not match $SIDECAR_MANIFEST"
  # A copied installer is always newer than the manifest, so this order says
  # little on its own. The extract check below binds the bundled daemons to
  # the manifest by sha256; a newer manifest is fatal only without that.
  MANIFEST_NEWER=0
  if (( $(file_mtime "$SIDECAR_MANIFEST") > $(file_mtime "$INSTALLER") )); then
    MANIFEST_NEWER=1
  fi
else
  [[ "$REQUIRE_BINDING" -eq 0 ]] || fail "--require-payload-binding needs the sidecar hash manifest (.soluterminal-shell-sidecars.sha256) next to the staged sidecars"
  warn "sidecar hash manifest missing near sidecars; run desktop/scripts/build-sidecar-binaries.sh before cargo tauri build"
fi
[[ "$REQUIRE_BINDING" -eq 0 || "$EXTRACT_CHECK" -eq 1 ]] ||
  fail "--require-payload-binding needs --extract-check: only the unpacked installer can show its daemons are the staged ones"
PAYLOAD_BOUND=0

# Optional payload-extraction check. The checks above validate the sidecars
# that were *staged* for the bundle — strong, but indirect: they trust that
# `cargo tauri build` actually embedded them. --extract-check closes that gap
# by unpacking the produced .exe and asserting the solud-linux *inside it* is a
# non-empty ELF that also carries this release's version marker. CI runs this
# so a Tauri config regression that drops the WSL resource (or bundles a stale
# placeholder) fails the build instead of shipping a daemon-less installer.
#
# NSIS installers are LZMA-compressed PE archives; 7-Zip (preinstalled on the
# GitHub windows runner) extracts them. innoextract is a fallback for non-7z
# hosts. When --extract-check is requested but NO extractor is available we now
# FAIL (was: warn) — it is the ONLY check that unpacks the .exe and verifies the
# BUNDLED daemon (the staged-sidecar checks above only verify the loose sidecars
# next to the installer, not what makensis actually embedded). Skipping it
# silently once risked shipping a blindly-built installer, so a real release must
# have an extractor. Install one on the build box: `sudo apt install p7zip-full`.
if [[ "$EXTRACT_CHECK" -eq 1 ]]; then
  extract_tool="${SOLU_INSTALLER_EXTRACT_TOOL:-}"
  if [[ -z "$extract_tool" ]]; then
    for cand in 7z 7za 7zr 7zz innoextract; do
      if command -v "$cand" >/dev/null 2>&1; then
        extract_tool="$cand"
        break
      fi
    done
  fi
  if [[ -z "$extract_tool" ]]; then
    fail "--extract-check requested but no extractor (7z/7za/7zr/7zz/innoextract) found — refusing to publish an UNVERIFIED installer (this is the only check of the daemon actually bundled in the .exe). Install one: sudo apt install p7zip-full"
  else
    extract_dir="$(mktemp -d 2>/dev/null || mktemp -d -t soluinstaller)"
    # shellcheck disable=SC2064
    trap "rm -rf '$extract_dir'" EXIT
    extract_ok=0
    case "$(basename "$extract_tool")" in
      innoextract*)
        innoextract -s -d "$extract_dir" "$INSTALLER" >/dev/null 2>&1 && extract_ok=1
        ;;
      *)
        # 7z family: `x` recurses, `-y` assumes yes, `-o<dir>` (no space).
        "$extract_tool" x -y "-o$extract_dir" "$INSTALLER" >/dev/null 2>&1 && extract_ok=1
        ;;
    esac
    if [[ "$extract_ok" -ne 1 ]]; then
      fail "--extract-check: failed to unpack installer with $extract_tool: $INSTALLER"
    fi
    # Find the bundled Linux daemon inside the extracted tree. Since v0.0.740
    # the installer carries BOTH arches GZIPPED (resources/solud-linux-{amd64,arm64}.gz —
    # prepare-sidecar.js gzips them; start-wsl.sh picks by uname -m and unpacks
    # at first launch). Older layouts carried a plain solud-linux. Accept either,
    # but always verify the actual daemon BYTES: a .gz is gunzipped to a temp
    # file so the ELF-magic + version-marker checks run on what WSL will execute.
    check_linux_payload() {
      # $1 = payload path (plain ELF or .gz), $2 = label for messages
      local f="$1" label="$2" bytes="$1" sz
      if [[ "$f" == *.gz ]]; then
        bytes="$extract_dir/.$(basename "$f").unpacked"
        if ! gzip -dc "$f" >"$bytes" 2>/dev/null; then
          fail "--extract-check: $label failed to gunzip — corrupt payload: $f"
        fi
      fi
      sz="$(file_size "$bytes")"
      if (( sz == 0 )); then
        fail "--extract-check: $label is EMPTY (0 bytes) — installer would strand WSL-backend users: $f"
      fi
      require_magic "$bytes" "$(printf '\177ELF')" "$label"
      require_marker "$bytes" "$SHELL_VERSION" "$label shell version $SHELL_VERSION"
      solu_require_webdist_parity "$bytes" "$EXPECT_WEB_VERSION" "$RESOLVED_WEB_SOURCE" "$RESOLVED_WEB_DIGEST" || exit 1
      require_payload_markers "$bytes" "$label"
      echo "==> payload check: $label is a ${sz}-byte ELF (version=$SHELL_VERSION)" >&2
    }
    # The staged-sidecar marker checks, run again on what the installer carries.
    require_payload_markers() { # <bytes> <label>
      if [[ "${SOLU_ALLOW_NO_AUTH_SIDECAR:-0}" != "1" ]]; then
        require_marker "$1" "$SOLU_SUPABASE_URL" "$2 Supabase URL"
        require_marker "$1" "$SOLU_SUPABASE_ANON_KEY" "$2 Supabase anon key"
      fi
      [[ -z "$effective_pubkey" ]] || require_marker "$1" "$effective_pubkey" "$2 update manifest public key"
      if [[ -n "$EXPECT_FLAVOR" && "$(solu_binary_flavor "$1")" != "$EXPECT_FLAVOR" ]]; then
        fail "--extract-check: $2 is not a ${EXPECT_FLAVOR} build"
      fi
    }
    payload_linux="$(find "$extract_dir" -type f -name 'solud-linux' -print 2>/dev/null | head -1)"
    if [[ -n "$payload_linux" ]]; then
      check_linux_payload "$payload_linux" "bundled Linux daemon"
    else
      payload_amd="$(find "$extract_dir" -type f -name 'solud-linux-amd64.gz' -print 2>/dev/null | head -1)"
      payload_arm="$(find "$extract_dir" -type f -name 'solud-linux-arm64.gz' -print 2>/dev/null | head -1)"
      if [[ -z "$payload_amd" || -z "$payload_arm" ]]; then
        fail "--extract-check: no solud-linux (or the solud-linux-{amd64,arm64}.gz pair) found inside installer payload — the WSL daemon resource was not bundled: $INSTALLER"
      fi
      check_linux_payload "$payload_amd" "bundled Linux daemon (amd64.gz)"
      check_linux_payload "$payload_arm" "bundled Linux daemon (arm64.gz)"
    fi

    # Bundled Windows daemon sidecar — the binary the Windows-native backend
    # actually runs. Tauri externalBin names sidecars <bin>-<target-triple>.exe
    # (e.g. solud-x86_64-pc-windows-msvc.exe). An unstamped `dev` build here is
    # exactly what strands users on a "solud dev" chip with auto-update disabled.
    # The staged-sidecar marker check earlier can be fooled by a WSL/Windows
    # repo-root divergence; this checks the actual bytes inside the installer, so
    # it is the authoritative guard whenever an extractor is available.
    payload_exe="$(find "$extract_dir" -type f -name 'solud*.exe' -print 2>/dev/null | head -1)"
    if [[ -z "$payload_exe" ]]; then
      fail "--extract-check: no solud*.exe found inside installer payload — the Windows daemon sidecar was not bundled: $INSTALLER"
    fi
    require_magic "$payload_exe" "MZ" "bundled Windows daemon"
    if ! grep -aFq -- "$SHELL_VERSION" "$payload_exe"; then
      fail "--extract-check: bundled solud.exe does not contain version $SHELL_VERSION — it is likely an unstamped 'dev' build (plain go build, no -ldflags). Run desktop/scripts/build-sidecar-binaries.sh before cargo tauri build."
    fi
    solu_require_webdist_parity "$payload_exe" "$EXPECT_WEB_VERSION" "$RESOLVED_WEB_SOURCE" "$RESOLVED_WEB_DIGEST" || exit 1
    require_payload_markers "$payload_exe" "bundled solud.exe"
    echo "==> payload check: bundled solud.exe ($(basename "$payload_exe")) carries version=$SHELL_VERSION" >&2

    # Bind the bundled daemons to the staged sidecars by sha256: the marker,
    # key and Supabase checks above ran on the staged files, so equal bytes
    # carry them into the installer. A .gz payload is compared unpacked.
    if [[ -f "$SIDECAR_MANIFEST" ]]; then
      manifest_sha() {
        awk -v n="$1" 'length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/ && ($2 == n || $2 == "*" n) {print tolower($1)}' "$SIDECAR_MANIFEST" | tail -1
      }
      # <payload bytes> <staged file the checks above ran on, or ""> <manifest name> <label>
      bind_payload() {
        local want got
        if [[ -n "$2" ]]; then
          want="$(sha256sum "$2" | awk '{print $1}')"
          [[ "$(manifest_sha "$3")" == "$want" ]] || fail "--extract-check: the staged $3 does not match its sidecar manifest entry"
        else
          # Only the manifest names it; it must be the file sha256sum -c
          # verified next to the manifest (that check ignores missing files).
          [[ -s "$manifest_dir/$3" ]] || fail "--extract-check: no staged $3 next to $(basename "$SIDECAR_MANIFEST") to bind the $4 to"
          want="$(manifest_sha "$3")"
        fi
        [[ -n "$want" ]] || fail "--extract-check: the sidecar manifest has no $3 entry to bind the $4 to"
        got="$(sha256sum "$1" | awk '{print $1}')"
        [[ "$got" == "$want" ]] || fail "--extract-check: the $4 is not the staged $3 (sha256 $got; staged $want)"
      }
      bind_payload "$payload_exe" "$WINDOWS_SIDECAR" solud.exe "bundled Windows daemon"
      if [[ -n "$payload_linux" ]]; then
        bind_payload "$payload_linux" "$LINUX_SIDECAR" solud-linux "bundled Linux daemon"
      else
        bind_payload "$extract_dir/.$(basename "$payload_amd").unpacked" "$LINUX_SIDECAR" solud-linux "bundled Linux daemon (amd64)"
        bind_payload "$extract_dir/.$(basename "$payload_arm").unpacked" "" solud-linux-arm64 "bundled Linux daemon (arm64)"
      fi
      PAYLOAD_BOUND=1
      echo "==> payload check: the bundled daemons are the staged sidecars (sha256 = $(basename "$SIDECAR_MANIFEST"))" >&2
    fi

    # Builder paths: a Rust release build embeds panic locations, so without
    # desktop/scripts/tauri-release-build.cmd's --remap-path-prefix the app
    # carries the builder's Windows user folder (and Go builds without
    # -trimpath their home directory). Refuse to ship either.
    builder_path_re='[A-Za-z]:\\Users\\[^\\]+\\|/home/[A-Za-z0-9._-]+/(\.cargo|\.rustup|go/pkg)/'
    while IFS= read -r -d '' bin; do
      if grep -aEq -- "$builder_path_re" "$bin"; then
        sample="$(grep -aoE -- "$builder_path_re" "$bin" | head -1)"
        fail "--extract-check: $(basename "$bin") contains a build-machine path (${sample}…) — build with desktop/scripts/tauri-release-build.cmd (and Go with -trimpath)"
      fi
    done < <(find "$extract_dir" -type f -iname '*.exe' -print0)
    echo "==> payload check: no build-machine paths in the bundled executables" >&2
    rm -rf "$extract_dir"
    trap - EXIT
  fi
fi

if [[ "${MANIFEST_NEWER:-0}" -eq 1 ]]; then
  [[ "$PAYLOAD_BOUND" -eq 1 ]] ||
    fail "sidecar manifest is newer than installer; rebuild the Tauri installer after rebuilding sidecars"
  warn "sidecar manifest is newer than the installer file (a copy?); its bundled daemons match the manifest by sha256"
fi
[[ "$REQUIRE_BINDING" -eq 0 || "$PAYLOAD_BOUND" -eq 1 ]] ||
  fail "--require-payload-binding: the bundled daemons could not be bound to the staged sidecars"

echo "==> shell installer validated: $SHELL_NAME version=$SHELL_VERSION platform=windows-amd64" >&2
echo "    sidecars: $WINDOWS_SIDECAR, $LINUX_SIDECAR" >&2

if [[ "$PRINT_VERSION" -eq 1 ]]; then
  printf '%s\n' "$SHELL_VERSION"
fi
