#!/bin/bash
# Shared setup for the double-click launchers: finds Python, creates .venv,
# installs requirements when they change, then runs: python -m <args>

cd "$(dirname "$0")/.." || exit 1
clear
echo "Starting iMessage Search…"
echo

pause_and_exit() {
  echo
  read -r -n 1 -p "Press any key to close this window."
  exit 1
}

# 1. Python (macOS includes it once Apple's free developer tools are installed)
PY=""
for candidate in python3 /usr/bin/python3 /opt/homebrew/bin/python3 /usr/local/bin/python3; do
  if "$candidate" -c "import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)" >/dev/null 2>&1; then
    PY="$candidate"; break
  fi
done
if [ -z "$PY" ]; then
  echo "This Mac needs Apple's Command Line Tools (free, about 5 minutes)."
  echo "A window will appear: click Install, wait for it to finish,"
  echo "then double-click 'Start iMessage Search' again."
  xcode-select --install >/dev/null 2>&1
  pause_and_exit
fi

# 2. A private Python environment for this app (first run only)
if [ ! -x .venv/bin/python ]; then
  echo "First-time setup, which takes about a minute…"
  "$PY" -m venv .venv || { echo "Couldn't create the Python environment."; pause_and_exit; }
fi

# 3. Helper packages (re-checked only when requirements.txt changes)
if [ ! -f .venv/.installed ] || [ requirements.txt -nt .venv/.installed ]; then
  echo "Installing helpers…"
  if .venv/bin/python -m pip install -q --disable-pip-version-check -r requirements.txt; then
    touch .venv/.installed
  else
    echo "(Couldn't install some helpers. Basic features still work. Check your internet and try again later.)"
  fi
fi

# 4. Run the app (arguments come from the .command file). Closing the window stops it.
exec .venv/bin/python -m "$@"
