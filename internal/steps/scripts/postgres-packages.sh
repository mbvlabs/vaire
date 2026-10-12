#!/usr/bin/env bash
# Step 8 (postgres-packages-v1): install PostgreSQL 18, contrib, PgBouncer and
# pgBackRest from PGDG, and pin them with apt-mark hold. pgBackRest older than
# PGBACKREST_MIN is refused with exit status 3 before it is installed.
set -euo pipefail

: "${PACKAGES:?PACKAGES is required}"
: "${HOLD:?HOLD is required}"
: "${PGDG_SCRIPT:?PGDG_SCRIPT is required}"
: "${PGBACKREST_MIN:?PGBACKREST_MIN is required}"

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

if [ ! -f /etc/apt/sources.list.d/pgdg.list ] && [ ! -f /etc/apt/sources.list.d/pgdg.sources ]; then
  bash "${PGDG_SCRIPT}" -y
fi

missing=()
for package in ${PACKAGES}; do
  installed "${package}" || missing+=("${package}")
done
if ((${#missing[@]})); then
  apt_get update
  if ! installed pgbackrest; then
    candidate="$(apt-cache policy pgbackrest | awk '/Candidate:/ { print $2 }')"
    if [ -z "${candidate}" ] || [ "${candidate}" = "(none)" ]; then
      printf 'no pgbackrest package is available from PGDG\n' >&2
      exit 3
    fi
    if ! dpkg --compare-versions "${candidate}" ge "${PGBACKREST_MIN}"; then
      printf 'PGDG offers pgbackrest %s; eru needs %s or newer\n' "${candidate}" "${PGBACKREST_MIN}" >&2
      exit 3
    fi
  fi
  apt_get install -y "${missing[@]}"
fi

version="$(dpkg-query -W -f='${Version}' pgbackrest)"
if ! dpkg --compare-versions "${version}" ge "${PGBACKREST_MIN}"; then
  printf 'pgbackrest %s is installed; eru needs %s or newer (eru pg update)\n' "${version}" "${PGBACKREST_MIN}" >&2
  exit 3
fi

for package in ${HOLD}; do
  apt-mark hold "${package}" >/dev/null
done

# Debian's pgbouncer package auto-starts with its own configuration. Stop it
# until step 13 renders the real one.
systemctl stop pgbouncer 2>/dev/null || true
systemctl disable pgbouncer 2>/dev/null || true

printf 'postgresql packages installed (pgbackrest %s)\n' "${version}"
