#!/usr/bin/env bash
# e2etest stub: print secrets so the test can prove they are redacted.
set -euo pipefail
: "${STUB_SECRET:?}"
: "${STUB_REGISTERED:?}"

echo "stub secret: $STUB_SECRET"
echo "stub secret on stderr: $STUB_SECRET" >&2
while IFS= read -r value; do
	echo "registered: $value"
done <<<"$STUB_REGISTERED"
