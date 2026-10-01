#!/usr/bin/env bash
# Reads the age master key at each process start; do not commit runtime files.
set -euo pipefail

BASE="/home/USER/workspace/grubdrops"
KEY_FILE="$BASE/secrets/master.key"
BIN="$BASE/bin/grubdrops"

[ -r "$KEY_FILE" ] || { echo "missing or unreadable $KEY_FILE" >&2; exit 1; }
[ -x "$BIN" ] || { echo "missing or non-executable $BIN" >&2; exit 1; }

export GRUB_MASTER_KEY
GRUB_MASTER_KEY="$(<"$KEY_FILE")"
export GRUB_DB_PATH="$BASE/data/miner.db"
export GRUB_HTTP_ADDR="127.0.0.1:8080"
export GRUB_SECURE_COOKIES="0"
export TZ="${TZ:-UTC}"

exec "$BIN"
