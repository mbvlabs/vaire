#!/usr/bin/env bash
# e2etest stub: hold the lock for STUB_SLEEP_S seconds.
set -euo pipefail
: "${STUB_SLEEP_S:?}"

if [[ ! "$STUB_SLEEP_S" =~ ^[0-9]+$ ]]; then
	echo "STUB_SLEEP_S must be a number" >&2
	exit 1
fi
echo "sleeping ${STUB_SLEEP_S}s"
sleep "$STUB_SLEEP_S"
echo "slept ${STUB_SLEEP_S}s"
