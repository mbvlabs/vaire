#!/usr/bin/env bash
# e2etest stub: write STUB_VALUE to STUB_FILE.
set -euo pipefail
: "${STUB_FILE:?}"
: "${STUB_VALUE:?}"

install -d -m 0755 "$(dirname "$STUB_FILE")"
printf '%s' "$STUB_VALUE" >"$STUB_FILE.tmp"
mv "$STUB_FILE.tmp" "$STUB_FILE"
echo "wrote $STUB_FILE"
