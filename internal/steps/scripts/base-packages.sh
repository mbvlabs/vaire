#!/usr/bin/env bash
# Step 1 (base-packages-v1): wait for cloud-init, then install the base
# packages that are missing. OPTIONAL_PACKAGES are skipped when the release
# has no candidate for them.
set -euo pipefail

: "${PACKAGES:?PACKAGES is required}"
: "${OPTIONAL_PACKAGES:=}"

if command -v cloud-init >/dev/null 2>&1; then
  # Exit status 2 means "done with recoverable errors"; that is fine here.
  cloud-init status --wait >/dev/null || true
fi

installed() {
  dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'
}

apt_get() {
  apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$@"
}

missing=()
for package in ${PACKAGES}; do
  installed "${package}" || missing+=("${package}")
done
optional_missing=()
for package in ${OPTIONAL_PACKAGES}; do
  installed "${package}" || optional_missing+=("${package}")
done

if ((${#missing[@]} == 0 && ${#optional_missing[@]} == 0)); then
  printf 'all base packages are installed\n'
  exit 0
fi

apt_get update
for package in "${optional_missing[@]}"; do
  if apt-cache policy "${package}" 2>/dev/null | grep -q 'Candidate: (none)' \
    || ! apt-cache policy "${package}" 2>/dev/null | grep -q 'Candidate:'; then
    printf 'skipping %s: no candidate on this release\n' "${package}"
    continue
  fi
  missing+=("${package}")
done

if ((${#missing[@]} > 0)); then
  printf 'installing %s\n' "${missing[*]}"
  apt_get install -y "${missing[@]}"
fi
