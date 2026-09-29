#!/usr/bin/env bash
# Take a consistent, read-only snapshot of your Messages database into ./data/
# Requires Full Disk Access for the terminal app you run this from (see README).
set -euo pipefail

SRC="$HOME/Library/Messages/chat.db"
DEST_DIR="$(cd "$(dirname "$0")/.." && pwd)/data"
DEST="$DEST_DIR/chat.db"

mkdir -p "$DEST_DIR"

if ! [ -r "$SRC" ]; then
  echo "Can't read $SRC."
  echo "Give your terminal Full Disk Access: System Settings > Privacy & Security > Full Disk Access."
  exit 1
fi

# sqlite3 .backup folds in the -wal file, so recent messages aren't lost.
rm -f "$DEST"
sqlite3 "file:$SRC?mode=ro" ".backup '$DEST'"
echo "Copied $(sqlite3 "$DEST" 'SELECT COUNT(*) FROM message') messages to $DEST"
