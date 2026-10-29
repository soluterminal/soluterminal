#!/usr/bin/env bash
# Install soluterminal as a systemd --user unit.
# This auto-starts solud on login and restarts it if it crashes.
#
# Sessions persist across daemon restarts: each pane runs in its own
# Setsid'd agent subprocess, so stopping/restarting the service keeps
# your shells and Claude sessions alive — the next daemon reattaches
# to them automatically.
#
# To enable the cloud relay, create ~/.solu/service.env from
# systemd/service.env.example and restart the service.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
UNIT_SRC="$ROOT/systemd/soluterminal.service"
UNIT_DST="$HOME/.config/systemd/user/soluterminal.service"

# The README's build puts the daemon at daemon/solud; ./build.sh (where the
# checkout has one) puts it at the repository root. With both present, take
# the newer one, so a stale build left in either place is never installed.
if [ -x "$ROOT/daemon/solud" ] && [ -x "$ROOT/solud" ]; then
  if [ "$ROOT/daemon/solud" -nt "$ROOT/solud" ]; then
    DAEMON_BIN="$ROOT/daemon/solud"
  else
    DAEMON_BIN="$ROOT/solud"
  fi
  echo "using $DAEMON_BIN (newer of the two)"
elif [ -x "$ROOT/daemon/solud" ]; then
  DAEMON_BIN="$ROOT/daemon/solud"
elif [ -x "$ROOT/solud" ]; then
  DAEMON_BIN="$ROOT/solud"
else
  echo "error: no solud binary found at $ROOT/daemon/solud or $ROOT/solud."
  if [ -f "$ROOT/build.sh" ]; then
    echo "Run ./build.sh, or follow the build steps in README.md."
  else
    echo "Follow the build steps in README.md."
  fi
  exit 1
fi

mkdir -p "$(dirname "$UNIT_DST")"
cp "$UNIT_SRC" "$UNIT_DST"

# Point the unit at the actual binary location (in case the repo was moved).
sed -i -E "s|%h/wsl-projects/soluterminal/(daemon/)?solud|$DAEMON_BIN|" "$UNIT_DST"

systemctl --user daemon-reload
# Clear any prior `failed` state (e.g. a tripped start-limit from a bad
# release) so a reinstall actually brings the unit back up instead of
# silently refusing to start.
systemctl --user reset-failed soluterminal.service 2>/dev/null || true
systemctl --user enable --now soluterminal.service
echo ""
echo "✓ installed systemd --user unit: $UNIT_DST"
echo ""
echo "Useful commands:"
echo "  journalctl --user -u soluterminal -f    # tail logs"
echo "  systemctl --user status soluterminal    # status"
echo "  systemctl --user restart soluterminal   # restart"
echo "  systemctl --user stop soluterminal      # stop"
echo "  systemctl --user disable --now soluterminal  # remove from startup"
echo "  systemctl --user reset-failed soluterminal   # clear a tripped crash-loop limit"
echo ""
echo "To enable the cloud relay for this machine:"
echo "  cp $ROOT/systemd/service.env.example ~/.solu/service.env"
echo "  chmod 600 ~/.solu/service.env"
echo "  # edit ~/.solu/service.env and fill in your token + backend id (wsl or host), then:"
echo "  systemctl --user restart soluterminal"
echo ""
echo "On WSL2, you may need:  sudo loginctl enable-linger $USER"
echo "so the service keeps running after your shell session ends."
