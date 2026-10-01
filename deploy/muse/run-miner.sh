#!/bin/bash
# GrubDrops launcher for Muse cloud VMs — reads secrets at runtime, never bakes them in.
# - GRUB_MASTER_KEY: read from secrets/master.key (0600) on every start.
# - UI binds 127.0.0.1:8080 only (localhost, no public port).
# - No docker socket: Twitch-only operation.
# - App-level proxy (Settings -> Proxy) is read from the settings store in the DB;
#   the watch leg additionally honors HTTP_PROXY/HTTPS_PROXY from the environment
#   (see the systemd unit).
#
# Replace /home/USER with your actual home directory before installing.
set -euo pipefail
BASE="/home/USER/workspace/grubdrops"
export GRUB_MASTER_KEY
GRUB_MASTER_KEY="$(cat "$BASE/secrets/master.key")"
export GRUB_DB_PATH="$BASE/data/miner.db"
export GRUB_HTTP_ADDR="127.0.0.1:8080"
export GRUB_SECURE_COOKIES="0"
export TZ="Asia/Taipei"
exec "$BASE/bin/grubdrops"
