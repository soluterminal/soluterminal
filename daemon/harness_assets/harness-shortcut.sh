#!/bin/sh
# soluterminal-managed-harness
#
# Shared Node interpreter resolver for every managed harness shortcut.
#
# The `deepseek`/`pi`/`pi-glm`/`zcode` commands in ~/.local/bin are tiny shims
# that exec this file with the interpreter path captured when the shortcut was
# written. That captured path is a snapshot: an nvm version can be upgraded or
# removed, or a home directory can be copied to another machine, and the old
# absolute path then stops resolving. Re-resolving here, instead of failing on
# the snapshot, keeps the command usable and, when nothing can run it, prints an
# actionable error instead of a bare `exec: ...: not found`.
#
# The order mirrors the daemon's resolveNodeBin (daemon/schedules_agent.go): the
# pinned interpreter, PATH, the asdf shim, the newest nvm/asdf install, then the
# user's login shell. Every candidate must also satisfy the provider's
# supported-Node gate: an executable but too-old pin (a distro /usr/bin/node,
# say) must not shadow the newer nvm/asdf install the user just added. The
# launcher cannot fall through to another interpreter once it is running, so the
# gate is probed here. Keep the probes in step with launch.mjs
# (nodeSupportsDsh / nodeSupportsPi).
#
# Usage: harness-shortcut.sh NODE LAUNCHER PROVIDER --data-dir DIR [ARGS...]
# Invoked by the generated shortcut, never by users.

pinned=${1-}
launcher=${2-}
provider=${3-}
shift 3

# Run the provider's own gate on a candidate. dsh 0.2.2's published floor is
# Node 24.9 (it also needs `import.meta.main`); Pi's is Node 22.19. Both mirror
# nodeSupportsDsh / nodeSupportsPi in launch.mjs.
# A non-Node executable still exits non-zero here, which is the right answer.
node_capable() {
  case $provider in
    deepseek)
      "$1" -e 'const [major, minor] = process.versions.node.split(".").map(Number); process.exit(typeof import.meta.main === "boolean" && (major > 24 || (major === 24 && minor >= 9)) ? 0 : 1)' >/dev/null 2>&1
      ;;
    pi|pi-glm)
      "$1" -e 'const [major, minor] = process.versions.node.split(".").map(Number); process.exit(major > 22 || (major === 22 && minor >= 19) ? 0 : 1)' >/dev/null 2>&1
      ;;
    *)
      return 0
      ;;
  esac
}

usable() {
  [ -n "$1" ] || return 1
  [ -x "$1" ] || return 1
  node_seen=1
  node_capable "$1"
}

# Newest install wins, compared numerically: a lexicographic sort would rank
# v9 above v10. Paths arrive one per line from ls.
newest_node() {
  awk '
    {
      path = $0
      n = split(path, part, "/")
      version = part[n-2]
      sub(/^v/, "", version)
      split(version, v, ".")
      major = v[1] + 0; minor = v[2] + 0; patch = v[3] + 0
      if (major > bm || (major == bm && (minor > bn || (minor == bn && patch > bp)))) {
        bm = major; bn = minor; bp = patch; best = path
      }
    }
    END { if (best != "") print best }
  '
}

node=
node_seen=

# 1. The interpreter resolved when the shortcut was written.
if usable "$pinned"; then
  node=$pinned
fi

# 2. Whatever `node` this environment selects; the daemon's first step too.
if [ -z "$node" ]; then
  candidate=$(command -v node 2>/dev/null) || candidate=
  if usable "$candidate"; then
    node=$candidate
  fi
fi

# 3. The asdf shim, then the newest nvm/asdf install. The globs still work in a
#    non-login shell where nvm/asdf were never initialised.
if [ -z "$node" ] && [ -n "$HOME" ]; then
  if usable "$HOME/.asdf/shims/node"; then
    node=$HOME/.asdf/shims/node
  else
    candidate=$(ls -d "$HOME"/.nvm/versions/node/*/bin/node "$HOME"/.asdf/installs/nodejs/*/bin/node 2>/dev/null | newest_node 2>/dev/null)
    if usable "$candidate"; then
      node=$candidate
    fi
  fi
fi

# 4. The user's login shell, where nvm/asdf are actually initialised.
if [ -z "$node" ] && command -v bash >/dev/null 2>&1; then
  candidate=$(bash -lc 'command -v node' 2>/dev/null | tail -n 1)
  if usable "$candidate"; then
    node=$candidate
  fi
fi

if [ -z "$node" ]; then
  label=$provider
  required='Node.js'
  case $provider in
    deepseek) label='DeepSeek Harness'; required='Node 24.9 or newer' ;;
    pi) label='Pi'; required='Node 22.19 or newer' ;;
    pi-glm) label='Pi (GLM)'; required='Node 22.19 or newer' ;;
    glm) label='ZCode' ;;
  esac
  if [ -n "$node_seen" ]; then
    printf '%s: installed Node.js is below the supported version (%s).\n' "$label" "$required" >&2
  else
    printf '%s: Node.js not found.\n' "$label" >&2
  fi
  printf '  pinned interpreter: %s\n' "${pinned:-<none>}" >&2
  printf '  Install %s, then run the harness install from SoluTerminal -> Credentials.\n' "$required" >&2
  exit 127
fi

exec "$node" "$launcher" "$provider" "$@"
