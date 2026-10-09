#!/bin/bash
# Double-click to check the Go version against the original Python version.
# Both build a search index from the same copy of your messages (data/chat.db),
# then every message is compared. Only counts and message ID numbers are printed.
# Nothing is uploaded, and your messages aren't changed.
cd "$(dirname "$0")" || exit 1
if [ ! -f data/chat.db ]; then
  echo "No data/chat.db yet. Run Start iMessage Search once and finish the setup, then try again."
  read -r -n 1 -p "Press any key to close this window."; exit 1
fi
if [ ! -x .venv/bin/python ]; then
  echo "The Python version's environment (.venv) isn't here, so there's nothing to compare against."
  read -r -n 1 -p "Press any key to close this window."; exit 1
fi
bash scripts/launch.sh compare 2>&1 | tee data/compare-result.txt

# If your messages came from an iPhone backup, also check the backup reader.
if grep -q '"source": *"backup"' data/setup.json 2>/dev/null; then
  echo
  echo "=== Backup check ==="
  echo "If your backup is encrypted, type its password (it isn't shown or saved). Otherwise just press Return."
  read -rs -p "Backup password: " IMSG_BACKUP_PASSWORD; echo
  IMSG_BACKUP_PASSWORD="$IMSG_BACKUP_PASSWORD" bin/imessage-search compare-backup 2>&1 | tee -a data/compare-result.txt
  unset IMSG_BACKUP_PASSWORD
fi
echo
echo "Saved to data/compare-result.txt (no message text in it)."
read -r -n 1 -p "Press any key to close this window."
