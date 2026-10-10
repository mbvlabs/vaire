#!/usr/bin/env bash
# e2etest stub: fail while STUB_FAIL_FILE exists.
set -euo pipefail
: "${STUB_FAIL_FILE:?}"

if [[ -e "$STUB_FAIL_FILE" ]]; then
	echo "failing because $STUB_FAIL_FILE exists" >&2
	exit 1
fi
echo "no failure requested"
