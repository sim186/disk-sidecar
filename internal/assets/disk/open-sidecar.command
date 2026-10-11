#!/bin/sh
# Mac and Linux: double click. Starts the local server and opens the catalog in your browser.
cd "$(dirname "$0")" || exit 1
exec ./sidecar serve
