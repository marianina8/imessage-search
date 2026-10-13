#!/bin/bash
# Shared setup for the double-click launchers: finds Go, builds the app when the
# code has changed, then runs: bin/imessage-search <args>

cd "$(dirname "$0")/.." || exit 1
clear
echo "Starting iMessage Search…"
echo

pause_and_exit() {
  echo
  read -r -n 1 -p "Press any key to close this window."
  exit 1
}

BIN=bin/imessage-search

# 1. Go (only needed to build; skipped when a built app is already up to date)
GO=""
for candidate in go /opt/homebrew/bin/go /usr/local/go/bin/go /usr/local/bin/go; do
  if command -v "$candidate" >/dev/null 2>&1; then GO="$candidate"; break; fi
done

needs_build() {
  [ ! -x "$BIN" ] && return 0
  [ -n "$(find . -path ./.git -prune -o \( -name '*.go' -o -name '*.html' -o -name go.mod -o -name go.sum \) -newer "$BIN" -print -quit)" ]
}

if needs_build; then
  if [ -z "$GO" ]; then
    if [ -x "$BIN" ]; then
      echo "(Go isn't installed, so using the app that's already built.)"
    else
      echo "This app is written in Go. Install Go once (free, about 2 minutes):"
      echo "  https://go.dev/dl/   — download the macOS installer and open it."
      echo "Then double-click this file again."
      pause_and_exit
    fi
  else
    # 2. First time: download the one helper library (pure-Go SQLite).
    if [ ! -f go.sum ]; then
      echo "First-time setup, which takes about a minute…"
      "$GO" mod tidy || { echo "Couldn't download the helper library. Check your internet and try again."; pause_and_exit; }
    fi
    echo "Building…"
    mkdir -p bin
    "$GO" build -o "$BIN" . || { echo "The build failed (see above)."; pause_and_exit; }
  fi
fi

# 3. Run the app (arguments come from the .command file). Closing the window stops it.
exec "$BIN" "$@"
