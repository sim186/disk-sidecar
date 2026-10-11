#!/bin/sh
# Mac: double click. Starts the local server and opens the catalog in your browser.
cd "$(dirname "$0")" || exit 1
if command -v python3 >/dev/null 2>&1 && python3 -c 'import sys; sys.exit(sys.version_info < (3, 9))'; then
  exec python3 serve.py
fi
echo "Sidecar needs Python 3.9 or newer, and it was not found."
echo "Install it from https://www.python.org/downloads/ (or run: brew install python)"
echo "and then double click this file again."
printf "Press Enter to close. "
read _
