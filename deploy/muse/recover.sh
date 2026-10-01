#!/usr/bin/env bash
# Restore the systemd link and start GrubDrops after a VM replacement.
# Replace /home/USER before installing. Run with sudo/root privileges.
set -euo pipefail

BASE="/home/USER/workspace/grubdrops"
UNIT_SRC="$BASE/systemd/grubdrops.service"
UNIT_DST="/etc/systemd/system/grubdrops.service"

if [ "$(id -u)" -ne 0 ]; then
  echo "run with sudo: sudo $BASE/bin/recover.sh" >&2
  exit 1
fi
case "$BASE" in
  /home/USER/*) echo "replace /home/USER in this script before running it" >&2; exit 1 ;;
esac
[ -f "$UNIT_SRC" ] || { echo "missing $UNIT_SRC" >&2; exit 1; }
[ -x "$BASE/bin/run-miner.sh" ] || { echo "missing launcher" >&2; exit 1; }
[ -r "$BASE/secrets/master.key" ] || { echo "missing master key" >&2; exit 1; }
[ -x "$BASE/bin/grubdrops" ] || { echo "missing binary" >&2; exit 1; }
[ -d "$BASE/data" ] || { echo "missing data directory" >&2; exit 1; }
[ -d "$BASE/logs" ] || { echo "missing logs directory" >&2; exit 1; }
if grep -Eq '(/home/USER/|User=USER|Group=USER|MUSE_EGRESS_PROXY_HOST)' "$UNIT_SRC"; then
  echo "replace the home, user/group, and proxy placeholders in $UNIT_SRC" >&2
  exit 1
fi

if [ ! -L "$UNIT_DST" ] || [ "$(readlink "$UNIT_DST")" != "$UNIT_SRC" ]; then
  ln -sfn "$UNIT_SRC" "$UNIT_DST"
fi
systemctl daemon-reload
systemctl enable --quiet grubdrops.service
if ! systemctl is-active --quiet grubdrops.service; then
  systemctl start grubdrops.service
fi

for _ in $(seq 1 20); do
  if curl -fsS --max-time 3 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    echo "grubdrops.service healthz OK"
    exit 0
  fi
  sleep 2
done
echo "grubdrops.service healthz did not respond after 40 seconds" >&2
exit 1
