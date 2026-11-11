#!/usr/bin/env bash
# Step 8 (postgres-packages-v1): install PostgreSQL 18, contrib, PgBouncer and
# pgBackRest from PGDG, and pin them with apt-mark hold. EXTENSION_PACKAGES are
# the resolved APT packages the databases' effective extension selections need
# (Phase 7): installed and held the same way, never removed. A prestart hook
# calls the same script with PACKAGES/HOLD empty, PGDG_SCRIPT empty (no
# automatic repository provisioning), PGBACKREST_MIN empty and
# STOP_PGBOUNCER=0, so only extension packages are installed and held.
# pgBackRest older than PGBACKREST_MIN is refused with exit status 3 before it
# is installed.
set -euo pipefail

PACKAGES="${PACKAGES:-}"
HOLD="${HOLD:-}"
PGDG_SCRIPT="${PGDG_SCRIPT:-}"
PGBACKREST_MIN="${PGBACKREST_MIN:-}"
EXTENSION_PACKAGES="${EXTENSION_PACKAGES:-}"
STOP_PGBOUNCER="${STOP_PGBOUNCER:-1}"

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

if [ -n "${PGDG_SCRIPT}" ] && [ ! -f /etc/apt/sources.list.d/pgdg.list ] && [ ! -f /etc/apt/sources.list.d/pgdg.sources ]; then
  bash "${PGDG_SCRIPT}" -y
fi

missing=()
for package in ${PACKAGES} ${EXTENSION_PACKAGES}; do
  installed "${package}" || missing+=("${package}")
done
if ((${#missing[@]})); then
  apt_get update
  if [ -n "${PGBACKREST_MIN}" ] && ! installed pgbackrest; then
    candidate="$(apt-cache policy pgbackrest | awk '/Candidate:/ { print $2 }')"
    if [ -z "${candidate}" ] || [ "${candidate}" = "(none)" ]; then
      printf 'no pgbackrest package is available from PGDG\n' >&2
      exit 3
    fi
    if ! dpkg --compare-versions "${candidate}" ge "${PGBACKREST_MIN}"; then
      printf 'PGDG offers pgbackrest %s; vaire needs %s or newer\n' "${candidate}" "${PGBACKREST_MIN}" >&2
      exit 3
    fi
  fi
  apt_get install -y "${missing[@]}"
fi

version=""
if installed pgbackrest; then
  version="$(dpkg-query -W -f='${Version}' pgbackrest)"
fi
if [ -n "${PGBACKREST_MIN}" ]; then
  if [ -z "${version}" ] || ! dpkg --compare-versions "${version}" ge "${PGBACKREST_MIN}"; then
    printf 'pgbackrest %s is installed; vaire needs %s or newer (vaire pg update)\n' "${version:-missing}" "${PGBACKREST_MIN}" >&2
    exit 3
  fi
fi

for package in ${HOLD} ${EXTENSION_PACKAGES}; do
  apt-mark hold "${package}" >/dev/null
done

if [ "${STOP_PGBOUNCER}" = "1" ] && ! grep -q 'managed by vaire' /etc/pgbouncer/pgbouncer.ini 2>/dev/null; then
  # Debian's pgbouncer package auto-starts with its own configuration. Stop it
  # until step 13 renders the real one. Once vaire manages pgbouncer.ini, a
  # later package step must not stop the live poolers.
  systemctl stop pgbouncer 2>/dev/null || true
  systemctl disable pgbouncer 2>/dev/null || true
fi

if [ -n "${EXTENSION_PACKAGES}" ]; then
  printf 'extension packages installed and held: %s\n' "${EXTENSION_PACKAGES}"
fi
if [ -n "${version}" ]; then
  printf 'postgresql packages installed (pgbackrest %s)\n' "${version}"
else
  printf 'postgresql packages installed\n'
fi
