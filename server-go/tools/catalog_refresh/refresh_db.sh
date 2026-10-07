#!/bin/bash
# Compatibility entry point. Deploy next to refresh_catalog.py as akki.
# No rm/cp of live SQLite/WAL files; Python owns staging, locking and recovery.
set -eu
umask 077
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PYTHON=${PYTHON:-python3}
exec "$PYTHON" "$HERE/refresh_catalog.py" "$@"