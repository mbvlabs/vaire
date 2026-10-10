#!/usr/bin/env bash
# e2etest stub: report whether a restart was requested.
set -euo pipefail
: "${STUB_RESTART:?}"

echo "restart requested: $STUB_RESTART"
