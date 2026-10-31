#!/bin/sh
# SoluTerminal installer for Linux (including WSL) and macOS.
#
#     curl -fsSL https://github.com/soluterminal/soluterminal-releases/releases/latest/download/install.sh | sh
#     curl -fsSL https://soluterminal-cloud.onrender.com/install.sh | sh   # same bytes, if GitHub is blocked
#     ... | sh -s -- --force-downgrade        # allow replacing a newer solud
#     ... | sh -s -- --version 0.0.1216       # install one exact release
#     ... | sh -s -- --cloud https://cloud.example.com   # pair with another cloud
#
# This file is static: every byte is committed to the SoluTerminal source
# repository (shared/installer/install.sh), attached unchanged to every release
# (with a detached signature, install.sh.sig), and served unchanged by the
# cloud's /install.sh. Nothing in it is generated per request.
#
# Trust: the script does not trust the cloud or any download host. It carries
# the release signing public key below, downloads the release's update.json
# and update.json.sig, verifies the Ed25519 signature locally, and installs a
# binary only when its SHA-256 matches the digest in that verified manifest.
# GitHub is tried first; the cloud (--cloud) is only a fallback download
# mirror, and its bytes go through exactly the same checks. Any failure stops
# before the existing installation is touched.
#
# Ed25519 verification uses, in order, the first tool that passes a built-in
# RFC 8032 known-answer test: OpenSSL 3 (`openssl pkeyutl -rawin`), Node.js 12+
# (`crypto.verify`), or Python 3 (a pure-standard-library verifier embedded
# below). macOS's system LibreSSL cannot verify Ed25519 and fails the test.
# If none passes, the installer refuses and explains what to install.
#
# After installing, it optionally pairs this machine with the cloud and
# installs a systemd --user unit (Linux) or a launchd agent (macOS) so solud
# starts on login. Re-running is safe: it upgrades in place.
set -eu

# Release signing public key (raw 32-byte Ed25519, base64). It must equal
# defaultUpdateManifestPubKey in daemon/updater.go and cloud/release_provenance.go;
# tests and publish.sh check that.
SOLU_RELEASE_PUBKEY='sm+MMrCMET4jQkfdIHySZ7RJWHLSV22qcBqKUVeU5k4='
# Public releases repositories (current home first, then the previous one).
# Must match shared/releases/repo.txt and legacy-repo.txt.
SOLU_RELEASE_REPOS='soluterminal/soluterminal-releases'
SOLU_DEFAULT_CLOUD='https://soluterminal-cloud.onrender.com'
DATA_DIR="$HOME/.solu"

# --- helpers ---
say()   { printf '%s\n' "$*"; }
warn()  { printf '\033[33m%s\033[0m\n' "$*" >&2; }
die()   { printf '\033[31merror: %s\033[0m\n' "$*" >&2; exit 1; }

usage() {
  say "usage: install.sh [--force-downgrade] [--version X.Y.Z] [--cloud https://HOST]"
}

force_downgrade=0
requested_version=""
CLOUD="$SOLU_DEFAULT_CLOUD"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --force-downgrade) force_downgrade=1 ;;
    --version)
      [ "$#" -ge 2 ] || die "--version needs a value"
      requested_version=$2
      shift
      ;;
    --version=*) requested_version=${1#--version=} ;;
    --cloud)
      [ "$#" -ge 2 ] || die "--cloud needs a value"
      CLOUD=$2
      shift
      ;;
    --cloud=*) CLOUD=${1#--cloud=} ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
  shift
done

# The cloud URL is used for pairing and as a fallback download mirror only.
CLOUD=${CLOUD%/}
case "$CLOUD" in
  https://*) cloud_hostport=${CLOUD#https://} ;;
  http://localhost|http://localhost:*|http://127.0.0.1|http://127.0.0.1:*) cloud_hostport=${CLOUD#http://} ;;
  *) die "--cloud must be an https:// origin (http:// only for localhost), got: $CLOUD" ;;
esac
case "$cloud_hostport" in
  ''|*[!A-Za-z0-9.:-]*) die "--cloud must be an origin with no path, got: $CLOUD" ;;
esac
case "$CLOUD" in
  https://*) RELAY="wss://$cloud_hostport/relay" ;;
  *) RELAY="ws://$cloud_hostport/relay" ;;
esac

# Release versions: dotted numbers with an optional suffix. Same pattern as
# releaseVersionRe in daemon/updater_manifest.go. A version read from an
# unverified source passes this before it is placed in a URL.
valid_release_version() {
  printf '%s\n' "$1" | grep -Eq '^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.-]+)?$'
}

if [ -n "$requested_version" ]; then
  requested_version=${requested_version#v}
  valid_release_version "$requested_version" || die "--version must look like 0.0.1216, got: $requested_version"
fi

# Published versions are dot-separated numeric components. Keep this parser
# strict: an existing binary without a trustworthy marker must not be replaced
# unless the operator explicitly authorizes it.
valid_numeric_version() {
  _version=$1
  [ -n "$_version" ] || return 1
  case "$_version" in .*|*.|*..*) return 1 ;; esac
  _old_ifs=$IFS
  IFS=.
  # shellcheck disable=SC2086 # split on dots on purpose
  set -- $_version
  IFS=$_old_ifs
  [ "$#" -gt 0 ] || return 1
  for _component do
    case "$_component" in
      ''|*[!0-9]*) return 1 ;;
    esac
  done
}

version_gt() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    na = split(a, av, "."); nb = split(b, bv, "."); n = na > nb ? na : nb
    for (i = 1; i <= n; i++) {
      ai = (i <= na ? av[i] : 0) + 0
      bi = (i <= nb ? bv[i] : 0) + 0
      if (ai > bi) exit 0
      if (ai < bi) exit 1
    }
    exit 1
  }'
}

# confirm PROMPT [default=Y] — asks Y/n, returns 0 on yes.
# If STDIN isn't a TTY (the curl|sh case), falls back to the default
# rather than hanging. Users get sensible out-of-the-box behavior.
confirm() {
  _prompt=$1
  _default=${2:-Y}
  if [ ! -t 0 ]; then
    say "$_prompt [$_default auto — not a tty]"
    case "$_default" in Y|y) return 0 ;; *) return 1 ;; esac
  fi
  case "$_default" in
    Y|y) printf '%s [Y/n] ' "$_prompt" ;;
    *) printf '%s [y/N] ' "$_prompt" ;;
  esac
  read -r _ans
  case "$_ans" in
    "") case "$_default" in Y|y) return 0 ;; *) return 1 ;; esac ;;
    [Yy]*) return 0 ;;
    *) return 1 ;;
  esac
}

file_size() {
  wc -c <"$1" | tr -d ' '
}

# json_string FILE PATH prints the string value at a dotted key path
# (e.g. platforms.linux-amd64.sha256). It tokenizes the JSON (strings with
# escapes, nesting, commas) rather than matching lines, and fails unless the
# path holds exactly one string value. It is only ever trusted on a manifest
# whose signature has already been verified.
json_string() {
  LC_ALL=C awk -v want="$2" '
    function keypath(depth,   s, k) {
      s = key[1]
      for (k = 2; k <= depth; k++) s = s "." key[k]
      return s
    }
    { buf = buf $0 "\n" }
    END {
      n = length(buf); d = 0; i = 1; bad = 0; hits = 0
      while (i <= n) {
        c = substr(buf, i, 1)
        if (c == "\"") {
          j = i + 1; s = ""
          while (j <= n) {
            ch = substr(buf, j, 1)
            if (ch == "\\") { s = s ch substr(buf, j + 1, 1); j += 2; continue }
            if (ch == "\"") break
            s = s ch; j++
          }
          if (j > n) { bad = 1; break }
          i = j + 1
          if (d > 0 && typ[d] == "o" && wantkey[d]) { key[d] = s; wantkey[d] = 0 }
          else if (d > 0 && keypath(d) == want) { hits++; val = s }
          continue
        }
        if (c == "{" || c == "[") {
          d++
          if (c == "{") { typ[d] = "o"; wantkey[d] = 1; key[d] = "" }
          else { typ[d] = "a"; wantkey[d] = 0; key[d] = "[]" }
          i++; continue
        }
        if (c == "}" || c == "]") { if (d == 0) { bad = 1; break }; d--; i++; continue }
        if (c == ",") { if (d > 0 && typ[d] == "o") wantkey[d] = 1; i++; continue }
        i++
      }
      if (bad || d != 0 || hits != 1) exit 1
      print val
    }' "$1"
}

# --- 1. detect platform ---
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS \"$os\"; only linux and darwin are built." ;;
esac
BACKEND_ID="host"
if [ "$os" = "linux" ] && [ "$(uname -r | grep -ic microsoft || true)" -gt 0 ]; then
  BACKEND_ID="wsl"
fi

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)    arch=amd64 ;;
  aarch64|arm64)   arch=arm64 ;;
  *) die "unsupported arch \"$arch\"; only amd64 and arm64 are built." ;;
esac
platform="$os-$arch"

command -v curl >/dev/null 2>&1 || die "curl is required; nothing was changed"

digest_tool=""
if command -v sha256sum >/dev/null 2>&1; then
  digest_tool="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  digest_tool="shasum"
elif command -v openssl >/dev/null 2>&1; then
  digest_tool="openssl"
else
  die "cannot verify downloads: install sha256sum, shasum or openssl"
fi

file_sha256() {
  case "$digest_tool" in
    sha256sum) sha256sum "$1" | awk '{print $1}' ;;
    shasum) shasum -a 256 "$1" | awk '{print $1}' ;;
    openssl) openssl dgst -sha256 "$1" | awk '{print $NF}' ;;
  esac
}

dest_dir="$HOME/.local/bin"
dest="$dest_dir/solud"
version_marker="$dest.version"

work=""
staged=""
marker_tmp=""
cleanup_install_temps() {
  [ -z "$staged" ] || rm -f -- "$staged"
  [ -z "$marker_tmp" ] || rm -f -- "$marker_tmp"
  [ -z "$work" ] || rm -rf -- "$work"
}
trap 'cleanup_install_temps' EXIT
trap 'cleanup_install_temps; exit 1' INT TERM HUP
work=$(mktemp -d "${TMPDIR:-/tmp}/solu-install.XXXXXX")

# --- 2. pick an Ed25519 verifier ---
# Each candidate must accept a known-good RFC 8032 signature (test 2) and
# reject the same signature over a different message before it is trusted.

ED25519_SPKI_PREFIX_B64='MCowBQYDK2VwAyEA' # DER SubjectPublicKeyInfo header for Ed25519

# verify_openssl PUB_B64 MESSAGE_FILE SIG_B64_FILE
verify_openssl() {
  _vd=$(mktemp -d "$work/openssl.XXXXXX")
  printf -- '-----BEGIN PUBLIC KEY-----\n%s%s\n-----END PUBLIC KEY-----\n' "$ED25519_SPKI_PREFIX_B64" "$1" >"$_vd/pub.pem"
  openssl base64 -d -A -in "$3" -out "$_vd/sig.bin" >/dev/null 2>&1 || return 1
  [ "$(file_size "$_vd/sig.bin")" = 64 ] || return 1
  openssl pkeyutl -verify -pubin -inkey "$_vd/pub.pem" -rawin -in "$2" -sigfile "$_vd/sig.bin" >"$_vd/out" 2>&1 || return 1
  grep -q 'Signature Verified Successfully' "$_vd/out"
}

# shellcheck disable=SC2016 # JavaScript source, not shell
SOLU_NODE_VERIFY='
try {
  const crypto = require("crypto"), fs = require("fs");
  const [pubB64, msgPath, sigPath] = process.argv.slice(1);
  const pub = Buffer.from(pubB64, "base64");
  const sig = Buffer.from(fs.readFileSync(sigPath, "utf8").trim(), "base64");
  if (pub.length !== 32 || sig.length !== 64) process.exit(1);
  const key = crypto.createPublicKey({
    key: Buffer.concat([Buffer.from("302a300506032b6570032100", "hex"), pub]),
    format: "der", type: "spki",
  });
  process.exit(crypto.verify(null, fs.readFileSync(msgPath), key, sig) ? 0 : 1);
} catch (e) { process.exit(1); }
'

verify_node() {
  NODE_OPTIONS='' node -e "$SOLU_NODE_VERIFY" "$1" "$2" "$3" >/dev/null 2>&1
}

# Pure-standard-library Ed25519 verification (RFC 8032 section 5.1.7,
# cofactorless, as Go's crypto/ed25519): accept only when the encoding of
# [S]B - [k]A equals R, with S < L and A a valid point.
SOLU_PYTHON_VERIFY='
import base64, binascii, hashlib, sys
p = 2**255 - 19
L = 2**252 + 27742317777372353535851937790883648493
d = -121665 * pow(121666, p - 2, p) % p
SQRT_M1 = pow(2, (p - 1) // 4, p)

def recover_x(y, sign):
    if y >= p:
        return None
    x2 = (y * y - 1) * pow(d * y * y + 1, p - 2, p) % p
    if x2 == 0:
        return None if sign else 0
    x = pow(x2, (p + 3) // 8, p)
    if (x * x - x2) % p:
        x = x * SQRT_M1 % p
    if (x * x - x2) % p:
        return None
    if x & 1 != sign:
        x = p - x
    return x

def add(P, Q):
    a = (P[1] - P[0]) * (Q[1] - Q[0]) % p
    b = (P[1] + P[0]) * (Q[1] + Q[0]) % p
    c = 2 * P[3] * Q[3] * d % p
    e = 2 * P[2] * Q[2] % p
    E, F, G, H = b - a, e - c, e + c, b + a
    return (E * F % p, G * H % p, F * G % p, E * H % p)

def mul(s, P):
    Q = (0, 1, 1, 0)
    while s:
        if s & 1:
            Q = add(Q, P)
        P = add(P, P)
        s >>= 1
    return Q

def encode(P):
    zi = pow(P[2], p - 2, p)
    x, y = P[0] * zi % p, P[1] * zi % p
    return (y | ((x & 1) << 255)).to_bytes(32, "little")

def decode(b):
    y = int.from_bytes(b, "little")
    sign, y = y >> 255, y & ((1 << 255) - 1)
    x = recover_x(y, sign)
    return None if x is None else (x, y, 1, x * y % p)

def verify(pub, msg, sig):
    if len(pub) != 32 or len(sig) != 64:
        return False
    A = decode(pub)
    if A is None:
        return False
    S = int.from_bytes(sig[32:], "little")
    if S >= L:
        return False
    k = int.from_bytes(hashlib.sha512(sig[:32] + pub + msg).digest(), "little") % L
    By = 4 * pow(5, p - 2, p) % p
    B = (recover_x(By, 0), By, 1, recover_x(By, 0) * By % p)
    negA = (-A[0] % p, A[1], A[2], -A[3] % p)
    return encode(add(mul(S, B), mul(k, negA))) == sig[:32]

try:
    pub = base64.b64decode(sys.argv[1], validate=True)
    with open(sys.argv[3], "rb") as f:
        sig = base64.b64decode(f.read().strip(), validate=True)
    with open(sys.argv[2], "rb") as f:
        msg = f.read()
    ok = verify(pub, msg, sig)
except (OSError, ValueError, binascii.Error, IndexError):
    ok = False
sys.exit(0 if ok else 1)
'

verify_python3() {
  python3 -I -c "$SOLU_PYTHON_VERIFY" "$1" "$2" "$3" >/dev/null 2>&1
}

verifier_available() {
  case "$1" in
    openssl) command -v openssl >/dev/null 2>&1 ;;
    node) command -v node >/dev/null 2>&1 ;;
    python3)
      _py=$(command -v python3 2>/dev/null) || return 1
      # On macOS /usr/bin/python3 is a stub until the Command Line Tools are
      # installed; running it would open an install dialog.
      if [ "$os" = darwin ] && [ "$_py" = /usr/bin/python3 ]; then
        xcode-select -p >/dev/null 2>&1 || return 1
      fi
      ;;
    *) return 1 ;;
  esac
}

ed25519_verify() {
  case "$1" in
    openssl) verify_openssl "$2" "$3" "$4" ;;
    node) verify_node "$2" "$3" "$4" ;;
    python3) verify_python3 "$2" "$3" "$4" ;;
    *) return 1 ;;
  esac
}

verifier_self_test() {
  printf 'r' >"$work/kat.msg"
  printf 's' >"$work/kat-other.msg"
  printf '%s\n' 'kqAJqfDUyrhyDoILX2QlQKKye1QWUD+Ps3YiI+vbadoIWsHkPhWZbkWPNhPQ8R2MOHsurrQwKu6wDSkWErsMAA==' >"$work/kat.sig"
  _kat_pub='PUAXw+hDiVqStwqnTRt+vJyYLM8uxJaMwM1V8Sr0Zgw='
  ed25519_verify "$1" "$_kat_pub" "$work/kat.msg" "$work/kat.sig" || return 1
  if ed25519_verify "$1" "$_kat_pub" "$work/kat-other.msg" "$work/kat.sig"; then
    return 1
  fi
  return 0
}

die_no_verifier() {
  printf '\033[31merror: cannot verify the release signature on this machine; nothing was changed.\033[0m\n' >&2
  cat >&2 <<'EOF'
The installer needs one of these to check the Ed25519 release signature:
  - OpenSSL 3 or newer. Linux: install your distribution's "openssl" package.
    macOS: `brew install openssl@3` and put its bin directory first on PATH
    (the system LibreSSL cannot verify Ed25519).
  - Node.js 12 or newer.
  - Python 3. macOS: `xcode-select --install`.
Install one, then run the installer again. To check a release by hand instead,
see "Initial install" in docs/UPDATE_SIGNING.md: verify update.json against
update.json.sig with the public key at the top of this script, then compare
the SHA-256 of solud-<os>-<arch> with the value in update.json.
EOF
  exit 1
}

case "$SOLU_RELEASE_PUBKEY" in
  *[!A-Za-z0-9+/=]*) die "installer public key is malformed" ;;
esac
[ "${#SOLU_RELEASE_PUBKEY}" -eq 44 ] || die "installer public key is malformed"

VERIFIER=""
for _candidate in ${SOLU_INSTALL_VERIFIER:-openssl node python3}; do
  case "$_candidate" in
    openssl|node|python3) ;;
    *) die "SOLU_INSTALL_VERIFIER may list only openssl, node and python3, got: $_candidate" ;;
  esac
  verifier_available "$_candidate" || continue
  if verifier_self_test "$_candidate"; then
    VERIFIER=$_candidate
    break
  fi
done
[ -n "$VERIFIER" ] || die_no_verifier

# --- 3. download and verify the release manifest ---
# fetch_asset NAME VERSION OUT: GitHub release (current repo, then previous),
# then the cloud as a mirror. An empty VERSION means the "latest" alias, which
# is used only for a version hint. Callers verify everything fetched.
asset_source=""
mirror_used=0
fetch_github() {
  for _repo in $SOLU_RELEASE_REPOS; do
    if [ -n "$2" ]; then
      _url="https://github.com/$_repo/releases/download/v$2/$1"
    else
      _url="https://github.com/$_repo/releases/latest/download/$1"
    fi
    if curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --retry 2 --retry-max-time 60 -o "$3" "$_url" 2>/dev/null; then
      asset_source="github.com/$_repo"
      return 0
    fi
  done
  return 1
}
# The mirror's HTTP status is kept so a rate limit (429) can be explained.
# --retry-max-time matters: curl honours Retry-After, which the mirror sets
# to up to an hour once an address has used its budget.
mirror_status=""
fetch_mirror() {
  _url="$CLOUD/install/$1"
  [ -z "$2" ] || _url="$_url?v=$2"
  mirror_status=$(curl -sSL --connect-timeout 15 --retry 2 --retry-max-time 60 -o "$3" -w '%{http_code}' "$_url" 2>/dev/null) || mirror_status=000
  if [ "$mirror_status" = 200 ]; then
    asset_source="$CLOUD (mirror)"
    return 0
  fi
  rm -f -- "$3"
  return 1
}
fetch_asset() {
  rm -f -- "$3"
  if [ "$mirror_used" -eq 0 ] && fetch_github "$1" "$2" "$3"; then
    return 0
  fi
  if fetch_mirror "$1" "$2" "$3"; then
    if [ "$mirror_used" -eq 0 ]; then
      warn "could not download $1 from GitHub; using $CLOUD as a mirror (still checked against the release signature)"
      mirror_used=1
    fi
    return 0
  fi
  if [ "$mirror_used" -eq 1 ] && fetch_github "$1" "$2" "$3"; then
    return 0
  fi
  if [ "$mirror_status" = 429 ]; then
    warn "the download mirror at $CLOUD is rate limiting this address; try again in an hour, or from a network that can reach github.com"
  fi
  return 1
}

if [ -n "$requested_version" ]; then
  INSTALL_VERSION=$requested_version
else
  fetch_asset update.json "" "$work/latest.json" || die "could not reach the release server (GitHub or $CLOUD); nothing was changed"
  [ "$(file_size "$work/latest.json")" -le 1048576 ] || die "release manifest is too large; nothing was changed"
  # The latest alias is not trusted for anything but this version hint.
  INSTALL_VERSION=$(json_string "$work/latest.json" version) || die "could not read the latest release version; nothing was changed"
  valid_release_version "$INSTALL_VERSION" || die "latest release version is malformed; nothing was changed"
fi

fetch_asset update.json "$INSTALL_VERSION" "$work/update.json" || die "could not download update.json for v$INSTALL_VERSION; nothing was changed"
fetch_asset update.json.sig "$INSTALL_VERSION" "$work/update.json.sig" || die "could not download update.json.sig for v$INSTALL_VERSION; nothing was changed"
[ "$(file_size "$work/update.json")" -le 1048576 ] || die "release manifest is too large; nothing was changed"
[ "$(file_size "$work/update.json.sig")" -le 4096 ] || die "release signature is too large; nothing was changed"
if ! ed25519_verify "$VERIFIER" "$SOLU_RELEASE_PUBKEY" "$work/update.json" "$work/update.json.sig"; then
  die "release manifest signature verification failed for v$INSTALL_VERSION (checked with $VERIFIER); nothing was changed"
fi
say "✓ verified the signed release manifest for v$INSTALL_VERSION ($VERIFIER)"

signed_version=$(json_string "$work/update.json" version) || die "signed manifest has no version; nothing was changed"
[ "$signed_version" = "$INSTALL_VERSION" ] || die "signed manifest is for v$signed_version, expected v$INSTALL_VERSION; nothing was changed"

expected_url=$(json_string "$work/update.json" "platforms.$platform.url") ||
  die "no verified download published for platform \"$platform\" in release v$INSTALL_VERSION; nothing was changed"
expected_sha=$(json_string "$work/update.json" "platforms.$platform.sha256") ||
  die "no verified download published for platform \"$platform\" in release v$INSTALL_VERSION; nothing was changed"
case "$expected_sha" in
  ''|*[!0-9a-f]*) die "signed manifest contains an invalid sha256; nothing was changed" ;;
esac
[ "${#expected_sha}" -eq 64 ] || die "signed manifest contains an invalid sha256; nothing was changed"
url_ok=0
for _repo in $SOLU_RELEASE_REPOS; do
  [ "$expected_url" != "https://github.com/$_repo/releases/download/v$INSTALL_VERSION/solud-$platform" ] || url_ok=1
done
[ "$url_ok" -eq 1 ] || die "signed manifest lists an unexpected download URL for $platform; nothing was changed"

# --- 4. refuse unintended downgrades ---
install_version_numeric=1
if ! valid_numeric_version "$INSTALL_VERSION"; then
  install_version_numeric=0
  warn "installer target version '$INSTALL_VERSION' is not numeric; version comparison and marker write are disabled"
fi

if [ -e "$dest" ]; then
  if [ "$install_version_numeric" -ne 1 ]; then
    if [ "$force_downgrade" -ne 1 ]; then
      die "refusing to replace existing solud: installer version $INSTALL_VERSION is not numeric; re-run explicitly with --force-downgrade"
    fi
    warn "forcing solud replacement with non-numeric installer version $INSTALL_VERSION (--force-downgrade)"
  else
  installed_version=""
  installed_source=""
  marker_state="missing"
  if [ -f "$version_marker" ]; then
    IFS= read -r installed_version <"$version_marker" || true
    case "$installed_version" in *"$(printf '\r')") installed_version=${installed_version%"$(printf '\r')"} ;; esac
    if valid_numeric_version "$installed_version"; then
      marker_state="valid"
    else
      marker_state="malformed"
    fi
  fi

  if [ "$marker_state" != "valid" ]; then
    embedded_versions=$(tr -c '[:print:]\n' '\n' <"$dest" | sed -n 's/.*main\.version=\([0-9][0-9.]*\).*/\1/p' | sort -u)
    embedded_count=$(printf '%s\n' "$embedded_versions" | sed '/^$/d' | wc -l | tr -d ' ')
    if [ "$embedded_count" = "1" ] && valid_numeric_version "$embedded_versions"; then
      installed_version=$embedded_versions
      installed_source=" (embedded build version)"
      marker_state="valid"
    else
      installed_label="unknown ($marker_state version marker, no embedded build version)"
      if [ "$force_downgrade" -ne 1 ]; then
        die "refusing to replace existing solud: installed version $installed_label, installer version $INSTALL_VERSION; re-run explicitly with --force-downgrade"
      fi
      warn "forcing solud replacement: installed version $installed_label, installer version $INSTALL_VERSION (--force-downgrade)"
    fi
  fi
  if [ "$marker_state" = "valid" ] && version_gt "$installed_version" "$INSTALL_VERSION"; then
    if [ "$force_downgrade" -ne 1 ]; then
      die "refusing daemon downgrade: installed version $installed_version$installed_source is newer than installer version $INSTALL_VERSION; re-run explicitly with --force-downgrade"
    fi
    warn "forcing daemon downgrade: installed version $installed_version$installed_source to installer version $INSTALL_VERSION (--force-downgrade)"
  fi
  fi
fi

# --- 5. download the binary and check it against the signed digest ---
mkdir -p "$dest_dir"
staged=$(mktemp "$dest_dir/.solud-install.XXXXXX")
downloaded=0
if [ "$mirror_used" -eq 0 ] && curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --retry 2 --retry-max-time 60 -o "$staged" "$expected_url" 2>/dev/null; then
  downloaded=1
  asset_source=${expected_url#https://}
  asset_source=${asset_source%%/releases/*}
elif fetch_asset "solud-$platform" "$INSTALL_VERSION" "$staged"; then
  downloaded=1
fi
[ "$downloaded" -eq 1 ] || die "download of solud v$INSTALL_VERSION failed; existing installation left unchanged"
actual_sha=$(file_sha256 "$staged")
if [ "$actual_sha" != "$expected_sha" ]; then
  die "sha256 mismatch: expected $expected_sha, got $actual_sha; existing installation left unchanged"
fi
say "✓ downloaded solud v$INSTALL_VERSION from $asset_source (sha256 matches the signed manifest)"
chmod 0755 "$staged"
mv "$staged" "$dest"
staged=""
if [ "$install_version_numeric" -eq 1 ]; then
  marker_tmp=$(mktemp "$dest_dir/.solud-version.XXXXXX")
  printf '%s\n' "$INSTALL_VERSION" >"$marker_tmp"
  mv "$marker_tmp" "$version_marker"
  marker_tmp=""
else
  rm -f "$version_marker"
fi
say "✓ installed: $dest (version $INSTALL_VERSION)"

# Warn if ~/.local/bin isn't on PATH yet.
case ":${PATH:-}:" in
  *":$dest_dir:"*) ;;
  *)
    warn "$dest_dir is not on your PATH. Add this to your shell rc:"
    # shellcheck disable=SC2016 # printed literally for the user's shell rc
    warn '    export PATH="$HOME/.local/bin:$PATH"'
    PATH="$dest_dir:$PATH"
    export PATH
    ;;
esac

# --- 6. pair with the cloud (if not already paired) ---
claim_file="$DATA_DIR/claim.json"
if [ -f "$claim_file" ]; then
  say ""
  say "✓ this machine is already paired with the cloud ($claim_file)"
  say "  (delete $claim_file if you want to re-pair)"
else
  say ""
  # Pairing is optional (the local UI works without an account), so a piped
  # install never starts it on its own.
  if confirm "Pair this machine with the cloud now?" N; then
    say ""
    say "Starting pairing — the daemon will print a 6-char code."
    say "Open $CLOUD in a browser, sign in, click '+ Claim a new machine',"
    say "paste the code, and give this machine a name."
    say ""
    # Run --claim in the foreground. Its own output shows the code
    # and waits for acceptance; on success it writes claim.json.
    "$dest" --claim --relay "$RELAY" || die "pairing failed (see above)"
    say ""
    say "✓ paired"
  else
    say "  skipped pairing — run later with: solud --claim --relay $RELAY"
  fi
fi

# --- 7. install systemd --user unit (Linux only) ---
# systemd's user mode means no sudo. Service auto-starts on login;
# restarts if the daemon crashes. On macOS the equivalent is launchd
# (.plist under ~/Library/LaunchAgents) — see the darwin branch below.
if [ "$os" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  unit_dst="$HOME/.config/systemd/user/soluterminal.service"
  env_dst="$DATA_DIR/service.env"

  # Drop a service.env if this machine is paired. Do NOT pin
  # --relay-token here: solud reads ~/.solu/claim.json when --relay is set,
  # and token rotation updates claim.json. Pinning the token in service.env
  # would resurrect old credentials on the next systemd restart.
  if [ -f "$claim_file" ]; then
    mkdir -p "$DATA_DIR"
    printf 'RELAY_ARGS=--relay %s --backend-id %s\n' "$RELAY" "$BACKEND_ID" >"$env_dst"
    chmod 600 "$env_dst"
    say "✓ wrote $env_dst (relay + backend id; token loaded from claim.json)"
  fi

  if systemctl --user is-enabled soluterminal.service >/dev/null 2>&1; then
    say ""
    say "✓ systemd service already enabled — restarting to pick up the new binary"
    systemctl --user restart soluterminal.service || true
  else
    say ""
    if confirm "Install systemd --user unit so solud starts on login?"; then
      mkdir -p "$(dirname "$unit_dst")"
      cat >"$unit_dst" <<UNIT
[Unit]
Description=soluterminal daemon
After=network.target

[Service]
EnvironmentFile=-$env_dst
ExecStart=$dest --addr 127.0.0.1:8787 \$RELAY_ARGS
Restart=always
RestartSec=2
StandardOutput=journal
StandardError=journal
KillMode=process
TimeoutStopSec=5

[Install]
WantedBy=default.target
UNIT
      systemctl --user daemon-reload
      systemctl --user enable --now soluterminal.service
      say "✓ systemd service enabled + started"
      say "  tail logs: journalctl --user -u soluterminal -f"
      if [ "$(uname -r | grep -ic microsoft || true)" -gt 0 ]; then
        warn "WSL users: services stop when your last WSL terminal closes."
        warn "Run 'sudo loginctl enable-linger $USER' to keep it running across WSL sessions."
      fi
    else
      say "  skipped systemd — start solud manually: solud --relay $RELAY"
    fi
  fi
fi

# --- 7b. install launchd LaunchAgent (macOS only) ---
# launchd's per-user agents mean no sudo. RunAtLoad starts solud on
# login; KeepAlive restarts it if the daemon crashes — the launchd
# analogue of systemd's Restart=always. Mirrors the systemd flags:
# same --addr and the same relay/backend args (loaded from claim.json).
if [ "$os" = "darwin" ] && command -v launchctl >/dev/null 2>&1; then
  agent_label="com.soluterminal.daemon"
  agent_dir="$HOME/Library/LaunchAgents"
  agent_dst="$agent_dir/$agent_label.plist"
  log_dir="$DATA_DIR/logs"
  log_out="$log_dir/solud.out.log"
  log_err="$log_dir/solud.err.log"

  # Relay/backend args, mirroring the systemd service.env. Pass empty
  # strings when this machine isn't paired yet so the plist stays valid;
  # solud reads ~/.solu/claim.json when --relay is set, so we don't pin
  # the rotating token here (same reasoning as the systemd branch).
  relay_arg_1=""
  relay_arg_2=""
  relay_arg_3=""
  relay_arg_4=""
  if [ -f "$claim_file" ]; then
    relay_arg_1="--relay"
    relay_arg_2="$RELAY"
    relay_arg_3="--backend-id"
    relay_arg_4="$BACKEND_ID"
  fi

  if launchctl list "$agent_label" >/dev/null 2>&1; then
    say ""
    say "✓ launchd agent already loaded — restarting to pick up the new binary"
    launchctl kickstart -k "gui/$(id -u)/$agent_label" >/dev/null 2>&1 ||
      launchctl stop "$agent_label" >/dev/null 2>&1 || true
  else
    say ""
    if confirm "Install launchd agent so solud starts on login?"; then
      mkdir -p "$agent_dir" "$log_dir"
      cat >"$agent_dst" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$agent_label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$dest</string>
    <string>--addr</string>
    <string>127.0.0.1:8787</string>
    $( [ -n "$relay_arg_1" ] && printf '<string>%s</string>\n    <string>%s</string>\n    <string>%s</string>\n    <string>%s</string>' "$relay_arg_1" "$relay_arg_2" "$relay_arg_3" "$relay_arg_4" )
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>2</integer>
  <key>StandardOutPath</key>
  <string>$log_out</string>
  <key>StandardErrorPath</key>
  <string>$log_err</string>
</dict>
</plist>
PLIST
      # Modern bootstrap (macOS 10.11+) with a fallback to legacy load
      # for older systems, then kickstart so it starts immediately.
      launchctl bootstrap "gui/$(id -u)" "$agent_dst" >/dev/null 2>&1 ||
        launchctl load "$agent_dst" >/dev/null 2>&1 || true
      launchctl kickstart -k "gui/$(id -u)/$agent_label" >/dev/null 2>&1 ||
        launchctl start "$agent_label" >/dev/null 2>&1 || true
      say "✓ launchd agent installed + started"
      say "  tail logs: tail -f $log_err"
    else
      say "  skipped launchd — start solud manually: solud --relay $RELAY"
    fi
  fi
fi

say ""
say "Done. Next steps:"
if [ -f "$claim_file" ]; then
  say "  • Open http://localhost:8787 for the local UI"
  say "  • Open $CLOUD in a browser to access your machine from anywhere"
  say "    A new install accepts only devices you approve. The first time a phone or"
  say "    browser asks, approve it on this computer in http://localhost:8787 →"
  say "    Settings → Privacy & security, after checking the codes match. On a machine without"
  say "    a screen: ssh -L 8787:127.0.0.1:8787 <this-machine>, then open"
  say "    http://localhost:8787 on your own computer."
else
  say "  • solud --relay $RELAY            # start the daemon (skip if the service above is running)"
  say "  • Open http://localhost:8787 for the local UI"
  say "  • Optional: solud --claim --relay $RELAY    # pair with the cloud"
fi
