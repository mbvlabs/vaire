#!/usr/bin/env bash
# Step 8 (postgres-packages-v1): install PostgreSQL 18, contrib and PgBouncer
# from PGDG and pin the major version with apt-mark hold.
set -euo pipefail

: "${PACKAGES:?PACKAGES is required}"
: "${HOLD:?HOLD is required}"
: "${PGDG_SCRIPT:?PGDG_SCRIPT is required}"

installed() {
  dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'
}

apt_get() {
  apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$@"
}

if ! installed postgresql-common; then
  apt_get update
  apt_get install -y postgresql-common
fi

if [ ! -f /etc/apt/sources.list.d/pgdg.list ]; then
  bash "${PGDG_SCRIPT}" -y
fi

missing=()
for package in ${PACKAGES}; do
  installed "${package}" || missing+=("${package}")
done
if ((${#missing[@]})); then
  apt_get update
  apt_get install -y "${missing[@]}"
fi

for package in ${HOLD}; do
  apt-mark hold "${package}" >/dev/null
done

# Debian's pgbouncer package auto-starts with its own configuration. Stop it
# until step 13 renders the real one.
systemctl stop pgbouncer 2>/dev/null || true
systemctl disable pgbouncer 2>/dev/null || true

printf 'postgresql packages installed\n'
