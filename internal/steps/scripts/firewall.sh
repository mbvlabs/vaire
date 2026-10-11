#!/usr/bin/env bash
# Step 5 (firewall-v1): converge ufw to exactly UFW_RULES (one rule spec per
# line, in "ufw show added" form) and enable it. Expected rules are added
# before anything is removed, and added again afterwards, so SSH is never
# without an allow rule even if ufw prints a rule differently.
set -euo pipefail

: "${UFW_RULES:?UFW_RULES is required}"

ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null

mapfile -t want <<< "${UFW_RULES}"

add_all() {
  local rule args
  for rule in "${want[@]}"; do
    [ -n "${rule}" ] || continue
    read -r -a args <<< "${rule}"
    ufw "${args[@]}" >/dev/null
  done
}

wanted() {
  local rule
  for rule in "${want[@]}"; do
    [ "${rule}" = "$1" ] && return 0
  done
  return 1
}

add_all
mapfile -t have < <(ufw show added | sed -n 's/^ufw //p')
for rule in "${have[@]}"; do
  [ -n "${rule}" ] || continue
  wanted "${rule}" && continue
  read -r -a args <<< "${rule}"
  if ufw delete "${args[@]}" >/dev/null; then
    printf 'removed rule: %s\n' "${rule}"
  else
    printf 'could not remove rule: %s\n' "${rule}"
  fi
done
add_all

ufw --force enable >/dev/null
ufw status verbose
