#!/usr/bin/env bash
# Run one production node RPC directly on the lab guest as root.
#
# Usage:
#   node-rpc-case.sh METHOD PARAMS_JSON [CONFIRM] [DRY_RUN]
#
# Environment:
#   VAIRE_LAB_BIN     binary to execute (default /usr/local/bin/vaire)
#   VAIRE_LAB_COMMIT  override client_commit (default: binary's version output)
#   VAIRE_LAB_DRY     set to 1 for a dry run when the fourth argument is absent
#
# The script prints only the result envelope, excluding credential-bearing
# fields and run-log lines. Non-secret restart confirmation tokens remain.
# It is a lab evidence driver, not production code or a credentials handoff.
set -euo pipefail

method=${1:?method is required}
params=${2:-'{}'}
confirm=${3:-}
dry=${4:-${VAIRE_LAB_DRY:-0}}

bin=${VAIRE_LAB_BIN:-/usr/local/bin/vaire}
commit=${VAIRE_LAB_COMMIT:-$("$bin" version --json | jq -er '.data.commit')}
schema=vaire-rpc/v1

case "$dry" in
  1|true) dry_json=true ;;
  *) dry_json=false ;;
esac

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
umask 077

jq -n \
  --arg schema "$schema" \
  --arg method "$method" \
  --argjson params "$params" \
  --arg confirm "$confirm" \
  --arg commit "$commit" \
  --argjson dry "$dry_json" \
  '{
    schema: $schema,
    method: $method,
    params: $params,
    operator: "op-1",
    client_version: "dev",
    client_commit: $commit,
    client_channel: "dev",
    dry_run: $dry
  }
  + (if $confirm == "" then {} else {confirm: $confirm} end)' >"$tmp"

"$bin" node rpc <"$tmp" |
  jq -c '
    select(.type == "result") | .envelope |
    walk(if type == "object" then
      with_entries(select(.key | test(
        "password|passphrase|secret|credential|private|handoff|transfer_bundle|(^|_)(access|refresh|auth)_token|(^|_)(url|uri|dsn)s?($|_)";
        "i"
      ) | not))
    else . end)'
