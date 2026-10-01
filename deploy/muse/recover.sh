#!/bin/bash
# Re-installs / enables / starts the GrubDrops systemd unit on a Muse cloud VM.
# Used at initial deploy and after a VM replacement (the /etc copy does not
# persist; this script + the canonical unit under ~/workspace do).
# Safe to re-run: idempotent.
#
# Replace /home/USER with your actual home directory before installing.
set -euo pipefail
BASE="/home/USER/workspace/grubdrops"
UNIT_SRC="$BASE/systemd/grubdrops.service"
UNIT_DST="/etc/systemd/system/grubdrops.service"

[ -f "$UNIT_SRC" ] || { echo "missing $UNIT_SRC"; exit 1; }
[ -f "$BASE/secrets/master.key" ] || { echo "missing master key"; exit 1; }
[ -x "$BASE/bin/grubdrops" ] || { echo "missing binary"; exit 1; }
mkdir -p "$BASE/data" "$BASE/logs"
chmod 700 "$BASE/data" "$BASE/secrets"

if [ ! -L "$UNIT_DST" ] || [ "$(readlink "$UNIT_DST")" != "$UNIT_SRC" ]; then
  ln -sf "$UNIT_SRC" "$UNIT_DST"
  echo "linked $UNIT_DST"
fi
systemctl daemon-reload
systemctl enable --quiet grubdrops.service || true
if systemctl is-active --quiet grubdrops.service; then
  echo "grubdrops.service already active"
else
  systemctl start grubdrops.service
  echo "grubdrops.service started"
fi
# liveness probe (localhost only)
for i in $(seq 1 20); do
  if curl -sS --max-time 3 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    echo "healthz OK"
    exit 0
  fi
  sleep 2
done
echo "healthz NOT responding after 40s"; exit 1
