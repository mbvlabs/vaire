#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/scripts" "${work}/internal/buildinfo" "${work}/mock"
cp "${source_root}"/scripts/{build-release,upload-release,backfill-installer,install}.sh "${work}/scripts/"
cp "${source_root}/internal/buildinfo/buildinfo.go" "${work}/internal/buildinfo/"
cat >"${work}/mock/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ "$1" != -o ]; do shift; done
printf binary >"$2"
EOF
cat >"${work}/mock/git" <<'EOF'
#!/usr/bin/env bash
printf 'abcdef012345\n'
EOF
cat >"${work}/mock/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${TEST_WORK}/aws.log"
if [ "$1" = s3api ]; then
  [[ " $* " == *' --if-none-match * '* ]] || exit 1
  [[ " $* " == *' --key releases/1.0.0-alpha.4/install.sh '* ]] || exit 1
  while [ "$1" != --body ]; do shift; done
  [ ! -e "${TEST_WORK}/published" ] || exit 1
  cp "$2" "${TEST_WORK}/published"
fi
EOF
cat >"${work}/mock/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ "$1" != -o ]; do
  case "$1" in */manifest.json) file=manifest ;; */manifest.json.sigstore.json) file=bundle ;; */install.sh) file=published ;; esac
  shift
done
cp "${TEST_WORK}/${file:?}" "$2"
EOF
cat >"${work}/mock/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${TEST_BAD_SIGNATURE:-0}" = 0 ] || exit 1
[[ " $* " == *' --certificate-identity https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.4 '* ]]
EOF
chmod +x "${work}"/mock/*
export PATH="${work}/mock:${PATH}" TEST_WORK="${work}"
cd "${work}"
for channel in stable-1.x edge; do
  version=1.2.0
  [ "${channel}" != edge ] || version=edge-abcdef012345
  bash scripts/build-release.sh "${version}" "${channel}" https://vaire.mbvlabs.com >/dev/null
  manifest="dist/release-${channel}/releases/${version}/manifest.json"
  [ "$(jq -r '.artifacts | keys | join(" ")' "${manifest}")" = 'darwin/amd64 darwin/arm64 linux/amd64 linux/arm64' ]
  installer="dist/release-${channel}/releases/${version}/install.sh"
  grep -Fxq "channel_root=\"https://vaire.mbvlabs.com/releases/${version}\"" "${installer}"
  if grep -q '@@' "${installer}"; then echo 'unsubstituted placeholder' >&2; exit 1; fi
  if grep -q -- '--certificate-identity-regexp' "${installer}"; then
    echo 'installer uses a broad signer regexp' >&2; exit 1
  fi
  if [ "${channel}" = edge ]; then
    grep -Fq 'publish-edge.yml@refs/heads/master' "${installer}"
  else
    # The pinned installer derives the tag identity from its manifest version.
    # shellcheck disable=SC2016
    grep -Fq 'publish-stable.yml@refs/tags/v${manifest_version}' "${installer}"
  fi
  channel_dir="dist/release-${channel}/${channel#stable-}"
  [ "${channel}" != edge ] || channel_dir=dist/release-edge/edge
  touch "${channel_dir}/manifest.json.sigstore.json"
  bash scripts/upload-release.sh "dist/release-${channel}" "${channel}" bucket https://example.com artifacts
done
bash scripts/build-release.sh 1.0.0-alpha.4 edge https://vaire.mbvlabs.com >/dev/null
grep -Fq 's3 cp dist/release-stable-1.x/releases s3://bucket/releases --recursive' "${work}/aws.log"
if grep -Fq 'no-cache' "${work}/aws.log"; then echo 'channel uploaded in artifacts mode' >&2; exit 1; fi
cp dist/release-edge/releases/1.0.0-alpha.4/manifest.json "${work}/manifest"
touch "${work}/bundle"
export VERSION=1.0.0-alpha.4 RELEASE_BASE_URL=https://vaire.mbvlabs.com RELEASE_BUCKET=bucket R2_ENDPOINT=https://example.com
if TEST_BAD_SIGNATURE=1 bash scripts/backfill-installer.sh; then echo 'accepted bad signature' >&2; exit 1; fi
jq '.version = "1.0.0-alpha.3"' "${work}/manifest" >"${work}/invalid-manifest"
mv "${work}/manifest" "${work}/valid-manifest"
mv "${work}/invalid-manifest" "${work}/manifest"
if bash scripts/backfill-installer.sh; then echo 'accepted wrong manifest version' >&2; exit 1; fi
mv "${work}/valid-manifest" "${work}/manifest"
[ ! -e "${work}/published" ]
bash scripts/backfill-installer.sh
cmp "${work}/published" dist/release-edge/releases/1.0.0-alpha.4/install.sh
if bash scripts/backfill-installer.sh; then echo 'overwrote installer' >&2; exit 1; fi
if VERSION=1.0.0-alpha.04 bash scripts/backfill-installer.sh; then echo 'accepted invalid version' >&2; exit 1; fi
grep -Fq 'workflow_dispatch:' "${source_root}/.github/workflows/backfill-installer.yml"
grep -Fq 'environment: stable' "${source_root}/.github/workflows/backfill-installer.yml"
grep -Fq 'run: bash scripts/backfill-installer.sh' "${source_root}/.github/workflows/backfill-installer.yml"
echo 'versioned installer tests passed'
