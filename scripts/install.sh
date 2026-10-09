#!/usr/bin/env bash
set -euo pipefail

# Substituted by scripts/build-release.sh.
channel_root="@@CHANNEL_ROOT@@"
release_channel="@@CHANNEL@@"
# COSIGN_TRUST_ARGS is expanded by build-release.sh, not by this template.
# shellcheck disable=SC2016
cosign_trust_args='@@COSIGN_TRUST_ARGS@@'

printf 'eru %s installer from %s (%s)\n' "${release_channel}" "${channel_root}" "${cosign_trust_args}"
