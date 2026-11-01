#!/bin/sh
# Shared build-time defaults for shipped solud binaries.
#
# Source this file from release/install build scripts, then call
# solu_daemon_ldflags <version> [base-ldflags] [source-dir]. The source directory
# can be the repository root or daemon module. The returned string stamps the
# version, update manifest verification key, and optional Supabase defaults into
# daemon binaries via Go -ldflags.

SOLU_DEFAULT_UPDATE_MANIFEST_PUBKEY="sm+MMrCMET4jQkfdIHySZ7RJWHLSV22qcBqKUVeU5k4="

# solu_windows_build_source ROOT VERSION SCRATCH: return the daemon directory
# to compile. Four-part team candidates have build-derived resources, not a
# resource committed on main. Stage the source, regenerate and
# strictly check the exact candidate, without dirtying the pinned source.
# Three-part and public builds keep checking the committed resource.
solu_windows_build_source() (
  set -e
  resource_root="$1" resource_version="$2" resource_scratch="$3"
  resource_flavor="$(solu_build_flavor "$resource_root/daemon")" || exit $?
  case "$resource_flavor:$resource_version" in
    team:*.*.*.*)
      mkdir -p "$resource_scratch" || exit $?
      resource_scratch="$(cd "$resource_scratch" && pwd)" || exit $?
      # Interrupted builds can leave their copy behind. Never sweep fresh
      # copies another build may be using, symlinks, or a broader path pattern.
      find "$resource_scratch" -mindepth 1 -maxdepth 1 -type d -name 'windows-source.??????' -mmin +1440 -print | while IFS= read -r stale_stage; do
        solu_cleanup_windows_build_source "$resource_root" "$stale_stage/daemon" || exit $?
      done
      resource_stage="$(mktemp -d "$resource_scratch/windows-source.XXXXXX")" || exit $?
      trap 'solu_cleanup_windows_build_source "$resource_root" "$resource_stage/daemon"' EXIT
      cp -a "$resource_root/daemon" "$resource_stage/daemon" || exit $?
      cp -a "$resource_root/shared" "$resource_stage/shared" || exit $?
      resource_target="$resource_stage/daemon/rsrc_windows_amd64.syso"
      node "$resource_root/scripts/windows-resources.mjs" "$resource_version" "$resource_target" || exit $?
      node "$resource_root/scripts/windows-resources.mjs" "$resource_version" --check "$resource_target" || exit $?
      printf '%s\n' "$resource_stage/daemon"
      trap - EXIT
      ;;
    *)
      node "$resource_root/scripts/windows-resources.mjs" "$resource_version" --check || exit $?
      printf '%s\n' "$resource_root/daemon"
      ;;
  esac
)

# Remove only the scratch copy created above, never the canonical source.
solu_cleanup_windows_build_source() (
  [ "$2" != "$1/daemon" ] || exit 0
  case "$2" in
    */windows-source.??????/daemon) rm -rf "${2%/daemon}" ;;
    *) echo "error: refusing to remove unexpected Windows build source: $2" >&2; exit 1 ;;
  esac
)

# Extract the canonical frontend provenance marker from either webdist.json or
# a daemon binary containing the embedded file. Unique output makes duplicate
# linker/string-table copies harmless while still letting parity checks reject
# conflicting markers.
solu_extract_webdist_marker() {
  file="$1"
  grep -aoE 'solu-webdist v=[^ "]+ src=([0-9a-fA-F]+|unknown) sha256=[0-9a-f]{64}' "$file" 2>/dev/null | sort -u
}

# Recompute the emitter's canonical digest: sorted relative path bytes followed
# immediately by each file's bytes, excluding webdist.json itself.
solu_compute_webdist_digest() {
  dir="$1"
  if ! command -v node >/dev/null 2>&1; then
    echo "error: node is required to verify webdist bundle bytes in $dir" >&2
    return 1
  fi
  node - "$dir" <<'NODE'
const { createHash } = require('node:crypto');
const { readdirSync, readFileSync } = require('node:fs');
const path = require('node:path');
const root = path.resolve(process.argv[2]);
function files(dir, prefix = '') {
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (relative === 'webdist.json') continue;
    if (entry.isDirectory()) out.push(...files(path.join(dir, entry.name), relative));
    else if (entry.isFile()) out.push(relative);
  }
  return out.sort();
}
const hash = createHash('sha256');
for (const relative of files(root)) {
  hash.update(relative, 'utf8');
  hash.update(readFileSync(path.join(root, ...relative.split('/'))));
}
process.stdout.write(`${hash.digest('hex')}\n`);
NODE
}

solu_require_webdist_parity() {
  file="$1"
  expect_version="$2"
  expect_source="${3:-}"
  expect_digest="${4:-}"
  markers="$(solu_extract_webdist_marker "$file" || true)"
  if [ -z "$markers" ]; then
    echo "error: embedded web provenance marker missing from $file (expected web version $expect_version${expect_source:+ from source $expect_source})" >&2
    return 1
  fi

  while IFS= read -r marker; do
    [ -n "$marker" ] || continue
    found_version="$(printf '%s\n' "$marker" | sed -n 's/^solu-webdist v=\([^ ]*\) src=.*/\1/p')"
    found_source="$(printf '%s\n' "$marker" | sed -n 's/^solu-webdist v=[^ ]* src=\([^ ]*\) sha256=.*/\1/p')"
    found_digest="$(printf '%s\n' "$marker" | sed -n 's/^solu-webdist v=[^ ]* src=[^ ]* sha256=\([^ ]*\)$/\1/p')"
    if [ "$found_version" != "$expect_version" ]; then
      echo "error: embedded web version $found_version in $file does not match expected web version $expect_version (marker: $marker)" >&2
      return 1
    fi
    if [ -n "$expect_source" ] && [ "$found_source" != "$expect_source" ]; then
      echo "error: embedded web source $found_source in $file does not match expected source $expect_source (marker: $marker)" >&2
      return 1
    fi
    if [ -n "$expect_digest" ] && [ "$found_digest" != "$expect_digest" ]; then
      echo "error: embedded web digest $found_digest in $file does not match expected webdist digest $expect_digest (marker: $marker)" >&2
      return 1
    fi
  done <<EOF
$markers
EOF
}

solu_require_webdist_json() {
  dir="$1"
  expect_version="$2"
  expect_source="${3:-}"
  provenance="$dir/webdist.json"
  if [ ! -f "$provenance" ]; then
    echo "error: web provenance file missing: $provenance" >&2
    return 1
  fi
  solu_require_webdist_parity "$provenance" "$expect_version" "$expect_source" || return 1

  json_version="$(sed -n 's/.*"webVersion":"\([^"]*\)".*/\1/p' "$provenance" | head -1)"
  json_source="$(sed -n 's/.*"sourceSha":"\([^"]*\)".*/\1/p' "$provenance" | head -1)"
  json_digest="$(sed -n 's/.*"digest":"\([0-9a-f]*\)".*/\1/p' "$provenance" | head -1)"
  marker_digest="$(solu_extract_webdist_marker "$provenance" | sed -n 's/^.* sha256=\([0-9a-f]*\)$/\1/p' | head -1)"
  if [ "$json_version" != "$expect_version" ]; then
    echo "error: webdist.json webVersion $json_version in $dir does not match expected web version $expect_version" >&2
    return 1
  fi
  if [ -n "$expect_source" ] && [ "$json_source" != "$expect_source" ]; then
    echo "error: webdist.json sourceSha $json_source in $dir does not match expected source $expect_source" >&2
    return 1
  fi
  if [ -z "$json_digest" ] || [ "$json_digest" != "$marker_digest" ]; then
    echo "error: webdist.json digest $json_digest in $dir does not match marker digest $marker_digest" >&2
    return 1
  fi
  actual_digest="$(solu_compute_webdist_digest "$dir")" || return 1
  if [ "$actual_digest" != "$json_digest" ]; then
    echo "error: webdist bundle digest $actual_digest in $dir does not match declared digest $json_digest" >&2
    return 1
  fi
}

solu_tauri_version() {
  root="$1"
  # Read the version with sed — portable across Linux, WSL, and Git Bash on
  # Windows runners. The previous `node -p "require(...)"` form broke on
  # windows-latest (the CI shell-installer job): Git Bash hands Node an
  # MSYS-style /d/a/... path it can't resolve, so the sidecar build aborted
  # before stamping the version. sed reads the file directly and extracts the
  # identical "version" string on every platform.
  sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$root/desktop/src-tauri/tauri.conf.json" | head -1
}

solu__strip_env_quotes() {
  value="$1"
  value="${value#\"}"
  value="${value%\"}"
  value="${value#\'}"
  value="${value%\'}"
  printf '%s' "$value"
}

solu_load_supabase_defaults() {
  root="$1"
  env_local="$root/cloud/.env.local"

  if { [ -z "${SOLU_SUPABASE_URL:-}" ] || [ -z "${SOLU_SUPABASE_ANON_KEY:-}" ]; } && [ -f "$env_local" ]; then
    while IFS= read -r line; do
      case "$line" in
        SUPABASE_URL=*)
          if [ -z "${SOLU_SUPABASE_URL:-}" ]; then
            SOLU_SUPABASE_URL="$(solu__strip_env_quotes "${line#SUPABASE_URL=}")"
            export SOLU_SUPABASE_URL
          fi
          ;;
        SUPABASE_ANON_KEY=*)
          if [ -z "${SOLU_SUPABASE_ANON_KEY:-}" ]; then
            SOLU_SUPABASE_ANON_KEY="$(solu__strip_env_quotes "${line#SUPABASE_ANON_KEY=}")"
            export SOLU_SUPABASE_ANON_KEY
          fi
          ;;
      esac
    done <"$env_local"
  fi
}

solu_effective_update_manifest_pubkey() {
  if [ "${SOLU_ALLOW_UNSIGNED_UPDATE_MANIFEST:-0}" = "1" ]; then
    printf '%s\n' ""
  elif [ -n "${SOLU_UPDATE_MANIFEST_PUBKEY:-}" ]; then
    printf '%s\n' "$SOLU_UPDATE_MANIFEST_PUBKEY"
  else
    printf '%s\n' "$SOLU_DEFAULT_UPDATE_MANIFEST_PUBKEY"
  fi
}

# The team build's source marker (daemon/team_source_private.go, never in the
# public export): a daemon source tree without it builds the public flavour.
# (Overridable only so the release tests can model a public tree that still
# compiles; SOLU_BUILD_FLAVOR is the supported switch.)
SOLU_TEAM_SOURCE_MARKER="${SOLU_TEAM_SOURCE_MARKER:-team_source_private.go}"

# The team build's private release file, next to the team source marker and
# likewise never in the public export: the team releases repo and the cloud's
# team update endpoint, stamped into team builds only (solu_daemon_ldflags).
SOLU_TEAM_RELEASE_FILE="${SOLU_TEAM_RELEASE_FILE:-team-release.private.env}"

# solu_team_release_value <daemon-dir> <KEY>: KEY's value in the team build's
# private release file. An error when the file or the value is missing: a
# team build without them could not receive updates.
solu_team_release_value() {
  file="$1/$SOLU_TEAM_RELEASE_FILE"
  if [ ! -f "$file" ]; then
    echo "error: the team build needs its release file ($file is missing)" >&2
    return 1
  fi
  value="$(sed -n "s/^$2=//p" "$file" | tail -n 1 | tr -d '[:space:]')"
  if [ -z "$value" ]; then
    echo "error: $file sets no $2" >&2
    return 1
  fi
  printf '%s\n' "$value"
}

# solu_build_flavor <daemon-dir>: the flavour release tooling builds, from
# SOLU_BUILD_FLAVOR. When unset it is team if the daemon directory has the
# team source marker, else public (the public export, which also carries this
# script). The directory is required, so no caller gets team by omission. A
# value other than team or public is an error rather than a guess.
solu_build_flavor() {
  if [ -z "${1:-}" ]; then
    echo "error: solu_build_flavor needs the daemon source directory" >&2
    return 1
  fi
  default_flavor=public
  if [ -f "$1/$SOLU_TEAM_SOURCE_MARKER" ]; then
    default_flavor=team
  fi
  case "${SOLU_BUILD_FLAVOR:-$default_flavor}" in
    team) printf 'team\n' ;;
    public) printf 'public\n' ;;
    *)
      echo "error: SOLU_BUILD_FLAVOR must be team or public, not '${SOLU_BUILD_FLAVOR}'" >&2
      return 1
      ;;
  esac
}

# solu_binary_flavor <file>: the flavour stamped into a daemon binary. A team
# binary carries the marker "solu-build-flavor=team"; no public binary does.
solu_binary_flavor() {
  if grep -aFq -- 'solu-build-flavor=team' "$1"; then
    printf 'team\n'
  else
    printf 'public\n'
  fi
}

solu_require_supabase_defaults() {
  label="${1:-release binary}"
  if [ -z "${SOLU_SUPABASE_URL:-}" ] || [ -z "${SOLU_SUPABASE_ANON_KEY:-}" ]; then
    echo "error: Supabase defaults missing for ${label}." >&2
    echo "       Set SOLU_SUPABASE_URL and SOLU_SUPABASE_ANON_KEY (repository variables in the official build)." >&2
    return 1
  fi
}

solu_daemon_ldflags() {
  version="$1"
  base="${2:-}"

  if [ -z "$version" ]; then
    echo "error: daemon build version is empty" >&2
    return 1
  fi

  ldflags="${base:+$base }-X main.version=$version"

  # Inventory currently supports component compatibility on Linux/amd64.
  # Resolve this graph on that target even when packaging on Windows/macOS.
  # Failed analysis aborts the build; unstamped ad-hoc builds fail closed at
  # runtime instead of inheriting compatibility from a reused version label.
  # Accept either the repo root (which contains daemon/) or the daemon module
  # directory itself (a flat copy of the module), so both layouts work.
  runtime_root="${3:-${ROOT:-}}"
  if [ -z "$runtime_root" ]; then
    echo "error: solu_daemon_ldflags needs ROOT or an explicit repository or daemon source directory for pane runtime provenance" >&2
    return 1
  fi
  daemon_module="$runtime_root/daemon"
  [ -d "$daemon_module" ] || daemon_module="$runtime_root"
  runtime_source="$(go run -C "$daemon_module" ./cmd/agent-fingerprint -root .)" || return 1
  case "$runtime_source" in
    ''|*[!0-9a-f]*) echo "error: invalid pane runtime source digest" >&2; return 1 ;;
  esac
  if [ "${#runtime_source}" -ne 64 ]; then
    echo "error: invalid pane runtime source digest length" >&2
    return 1
  fi
  ldflags="$ldflags -X main.paneRuntimeSourceLinux=$runtime_source"

  # Build provenance (docs/RUNBOOK-hand-install.md): the source commit, whether
  # the tree had uncommitted changes, and the channel. Only publish.sh sets
  # SOLU_BUILD_CHANNEL=release, so any other build reports itself as such.
  source_sha="${SOLU_SOURCE_SHA:-}"
  if [ -z "$source_sha" ]; then
    source_sha="$(git -C "$daemon_module" rev-parse HEAD 2>/dev/null || echo unknown)"
  fi
  source_dirty="${SOLU_SOURCE_DIRTY:-}"
  if [ -z "$source_dirty" ]; then
    if git -C "$daemon_module" rev-parse --git-dir >/dev/null 2>&1; then
      if [ -n "$(git -C "$daemon_module" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
        source_dirty=1
      else
        source_dirty=0
      fi
    else
      source_dirty=unknown
    fi
  fi
  ldflags="$ldflags -X main.buildSourceSha=$source_sha -X main.buildDirty=$source_dirty -X main.buildChannel=${SOLU_BUILD_CHANNEL:-local}"

  # Build flavour (daemon/update_flavor.go). The private tree stamps "team"
  # unless SOLU_BUILD_FLAVOR says otherwise; the public export (which carries
  # this script for the official build) has no team source marker and stamps
  # "public". Team is refused there: such a binary would follow the team
  # channel without the team source.
  flavor="$(solu_build_flavor "$daemon_module")" || return 1
  if [ "$flavor" = team ] && [ ! -f "$daemon_module/$SOLU_TEAM_SOURCE_MARKER" ]; then
    echo "error: SOLU_BUILD_FLAVOR=team needs the team source ($daemon_module/$SOLU_TEAM_SOURCE_MARKER is missing)" >&2
    return 1
  fi
  ldflags="$ldflags -X main.buildFlavorStamp=solu-build-flavor=$flavor"
  # Only a team build learns where team releases live: the cloud endpoint
  # team installs update from (daemon/team_updates.go) and the private repo
  # (only the release pipeline reads it). A public build is stamped with
  # neither, so it names no team repo and has no team update source. A team
  # build that starts on the public channel (the one-time migration release
  # and the cloud's install binaries, both downloadable by anyone) gets the
  # endpoint but not the repo name: no client needs it.
  if [ "$flavor" = team ]; then
    team_repo="$(solu_team_release_value "$daemon_module" SOLU_TEAM_RELEASES_REPO)" || return 1
    team_path="$(solu_team_release_value "$daemon_module" SOLU_TEAM_UPDATES_PATH)" || return 1
    case "$team_repo" in
      */*/*|/*|*/|*[!A-Za-z0-9_./-]*) echo "error: SOLU_TEAM_RELEASES_REPO must be <owner>/<name>" >&2; return 1 ;;
      */*) ;;
      *) echo "error: SOLU_TEAM_RELEASES_REPO must be <owner>/<name>" >&2; return 1 ;;
    esac
    case "$team_path" in
      /*) ;;
      *) echo "error: SOLU_TEAM_UPDATES_PATH must be an absolute path" >&2; return 1 ;;
    esac
    case "$team_path" in
      *[!A-Za-z0-9_/-]*|*//*) echo "error: SOLU_TEAM_UPDATES_PATH has unexpected characters" >&2; return 1 ;;
    esac
    ldflags="$ldflags -X main.teamUpdatesPath=$team_path"
    if [ -z "${SOLU_UPDATE_INITIAL_CHANNEL:-}" ]; then
      ldflags="$ldflags -X soluterminal-shared/releases.TeamRepo=$team_repo"
    fi
  fi
  # The one-time migration release starts installs from before flavours on
  # the public channel (daemon/update_channel.go); nothing else sets this.
  case "${SOLU_UPDATE_INITIAL_CHANNEL:-}" in
    '') ;;
    team|public) ldflags="$ldflags -X main.buildInitialChannelStamp=$SOLU_UPDATE_INITIAL_CHANNEL" ;;
    *) echo "error: SOLU_UPDATE_INITIAL_CHANNEL must be team or public, not '$SOLU_UPDATE_INITIAL_CHANNEL'" >&2; return 1 ;;
  esac

  pubkey="$(solu_effective_update_manifest_pubkey)"
  if [ -n "$pubkey" ]; then
    ldflags="$ldflags -X main.defaultUpdateManifestPubKey=$pubkey"
    # Fail-closed: when a real pubkey is baked in (every normal release), refuse
    # to apply an unsigned/wrong-sig manifest or control file instead of silently
    # degrading to checksum-only if the key is ever dropped from a build. The one
    # escape hatch (SOLU_ALLOW_UNSIGNED_UPDATE_MANIFEST=1) yields an empty pubkey
    # above, so this stays unset for dev/dry-run builds.
    ldflags="$ldflags -X main.requireSignedManifest=1"
  fi

  if [ -n "${SOLU_SUPABASE_URL:-}" ] || [ -n "${SOLU_SUPABASE_ANON_KEY:-}" ]; then
    if [ -z "${SOLU_SUPABASE_URL:-}" ] || [ -z "${SOLU_SUPABASE_ANON_KEY:-}" ]; then
      echo "error: partial Supabase defaults; both SOLU_SUPABASE_URL and SOLU_SUPABASE_ANON_KEY are required" >&2
      return 1
    fi
    ldflags="$ldflags -X main.defaultSupaURL=$SOLU_SUPABASE_URL -X main.defaultSupaAnonKey=$SOLU_SUPABASE_ANON_KEY"
  fi

  # An empty Go build ID. Its first half hashes the builder's environment, so it
  # was the only difference between v0.0.1221 and its CI rebuild (20-100 bytes
  # per binary, every other byte identical; docs/RELEASE-PROVENANCE.md).
  # Go derives a Mach-O binary's LC_UUID from the build ID, and macOS 15+ needs
  # one before granting Local Network access (golang/go#68678), so -B sets it
  # (and the ELF build-id note) from the version and source commit instead:
  # the same on every builder, different for every release.
  build_uuid="$(printf '%s %s' "$version" "$source_sha" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-32)"
  case "$build_uuid" in
    *[!0-9a-f]*|'') echo "error: could not derive the build UUID" >&2; return 1 ;;
  esac
  if [ "${#build_uuid}" -ne 32 ]; then
    echo "error: build UUID has the wrong length" >&2
    return 1
  fi
  ldflags="$ldflags -buildid= -B 0x$build_uuid"

  printf '%s\n' "$ldflags"
}
