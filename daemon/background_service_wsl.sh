#!/bin/bash
# Shared read-only ownership proof and tray-only stop. Never launches anything.
set -euo pipefail
export PATH=/usr/bin:/bin LC_ALL=C
proc=/proc
home="$(getent passwd "$(id -u)" | cut -d: -f6)"
[ -n "$home" ] || exit 1
dir="$home/.local/share/soluterminal"
pid="$(cat "$dir/solud-wsl.pid")"
sup="$(cat "$dir/start-wsl.pid")"
[[ "$pid" =~ ^[0-9]+$ && "$sup" =~ ^[0-9]+$ ]] || exit 1
[[ "$pid" -gt 1 && "$sup" -gt 1 && "$pid" != "$sup" ]] || exit 1
kill -0 "$pid" || exit 1
kill -0 "$sup" || exit 1
[ "$(stat -c %u "$proc/$pid")" = "$(id -u)" ] || exit 1
[ "$(stat -c %u "$proc/$sup")" = "$(id -u)" ] || exit 1
exe="$(readlink "$proc/$pid/exe")"
[[ "$exe" = "$dir/solud" || "$exe" = "$dir/solud (deleted)" ]] || exit 1
tr '\0' '\n' < "$proc/$pid/cmdline" | awk 'last == "--backend-id" && $0 == "wsl" {ok=1} {last=$0} END {exit !ok}' || exit 1
tr '\0' '\n' < "$proc/$sup/cmdline" | grep -Eq '(^|/)start-wsl\.sh$' || exit 1
[ "$(readlink "$proc/$sup/fd/9")" = "$dir/start-wsl.lock" ] || exit 1
awk '$1 == "lock:" && $3 == "FLOCK" && $5 == "WRITE" {ok=1} END {exit !ok}' "$proc/$sup/fdinfo/9" || exit 1
# Compare inodes too: a replaced lock pathname must not count as supervision.
[ "$(stat -Lc '%d:%i' "$proc/$sup/fd/9")" = "$(stat -Lc '%d:%i' "$dir/start-wsl.lock")" ] || exit 1
start="$(sed 's/.*) //' "$proc/$pid/stat" | awk '{print $20}')"
supstart="$(sed 's/.*) //' "$proc/$sup/stat" | awk '{print $20}')"
[[ -n "$start" && -n "$supstart" ]] || exit 1
# Prove the daemon belongs to this supervisor by the actual parent chain.
# Adopted/orphan successors without that chain are deliberately unsupported.
parent="$pid"
owned=0
for ((depth=0; depth<32; depth++)); do
  parent="$(sed 's/.*) //' "$proc/$parent/stat" | awk '{print $2}')" || exit 1
  [[ "$parent" =~ ^[0-9]+$ && "$parent" -gt 1 ]] || exit 1
  if [[ "$parent" = "$sup" ]]; then owned=1; break; fi
done
[[ "$owned" = 1 ]] || exit 1
# An update is a separate operation. Do not interrupt an admitted handoff.
[ ! -e "$dir/solud.update-pending" ] || exit 1
digest="$(printf '%s\n' "$pid" "$start" "$exe" "$sup" "$supstart" | sha256sum | cut -d' ' -f1)"
case "${1:-inspect}" in
  inspect) printf '%s %s\n' "$pid" "$digest" ;;
  stop)
    [[ "${2:-}" = "$pid" && "${3:-}" = "$digest" ]] || exit 1
    # Recheck birth times immediately before signalling; no name sweep and no
    # process-group signal. Pane hosts and the supervisor are left running.
    [ "$(sed 's/.*) //' "$proc/$pid/stat" | awk '{print $20}')" = "$start" ] || exit 1
    [ "$(sed 's/.*) //' "$proc/$sup/stat" | awk '{print $20}')" = "$supstart" ] || exit 1
    # Only a bounded diagnostic intent marker; it grants no authority and
    # changes no env. The replacement logs it once at boot.
    printf 'native-tray %s %s\n' "$pid" "$(date +%s)" > "$dir/service-restart.reason"
    kill -TERM "$pid"
    ;;
  *) exit 2 ;;
esac
