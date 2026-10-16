#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to write release manifests" >&2
  exit 1
fi

version="${1:?usage: scripts/build-release.sh VERSION CHANNEL BASE_URL}"
channel="${2:?usage: scripts/build-release.sh VERSION CHANNEL BASE_URL}"
base_url="${3:?usage: scripts/build-release.sh VERSION CHANNEL BASE_URL}"

case "${channel}" in
  stable | edge | stable-[0-9]*.x) ;;
  *)
    echo "channel must be stable, stable-<major>.x (for example stable-1.x) or edge" >&2
    exit 1
    ;;
esac
if [ "${channel}" = edge ] && [[ ! "${version}" =~ ^edge-[0-9a-f]{12}$ ]]; then
  echo "edge versions must use edge- followed by a 12-character commit hash" >&2
  exit 1
fi
if [ "${channel}" != edge ] && [[ ! "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$ ]]; then
  echo "stable versions must use MAJOR.MINOR.PATCH, optionally -alpha.N, -beta.N or -rc.N" >&2
  exit 1
fi

# The signing channel: the root and every major line are published by the
# stable workflow, so a line's manifest says stable. A binary from a line's
# tag still defaults to its own line (upgrade.SelectChannel reads the version).
signing_channel="${channel}"
line_dir=""
if [[ "${channel}" == stable-[0-9]*.x ]]; then
  signing_channel=stable
  major="${channel#stable-}"
  major="${major%.x}"
  if [ "${version%%.*}" != "${major}" ]; then
    echo "version ${version} is not on the ${channel} channel" >&2
    exit 1
  fi
  line_dir="${major}.x"
fi
base_url="${base_url%/}"
if [[ ! "${base_url}" =~ ^https://[^[:space:]\"\\]+$ ]]; then
  echo "base URL must be an HTTPS URL without whitespace, quotes, or backslashes" >&2
  exit 1
fi

channel_root="${base_url}"
if [ "${channel}" = edge ]; then
  channel_root="${base_url}/edge"
elif [ -n "${line_dir}" ]; then
  channel_root="${base_url}/${line_dir}"
fi

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

sed_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/&/\\&/g; s/|/\\|/g'
}

out="dist/release-${channel}"
rm -rf "${out}"
mkdir -p "${out}/releases/${version}"

pkg="github.com/mbvlabs/eru/internal/buildinfo"
commit="$(git rev-parse --short=12 HEAD)"
ldflags="-s -w -X ${pkg}.Version=${version} -X ${pkg}.Commit=${commit} -X ${pkg}.Channel=${signing_channel}"
build_args=(go build -trimpath -ldflags "${ldflags}")

linux_amd64_sha=""
linux_arm64_sha=""
darwin_amd64_sha=""
darwin_arm64_sha=""

build_one() {
  local os="$1"
  local arch="$2"
  local platform_dir="${out}/releases/${version}/${os}/${arch}"
  mkdir -p "${platform_dir}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" "${build_args[@]}" -o "${platform_dir}/eru" ./cmd/eru
  local hex
  hex="$(sha256_hex "${platform_dir}/eru")"
  printf '%s  eru\n' "${hex}" >"${platform_dir}/eru.sha256"
  printf '%s' "${hex}"
}

linux_amd64_sha="$(build_one linux amd64)"
linux_arm64_sha="$(build_one linux arm64)"
darwin_amd64_sha="$(build_one darwin amd64)"
darwin_arm64_sha="$(build_one darwin arm64)"

release_base_url="${base_url}/releases/${version}"
jq -n \
  --arg channel "${signing_channel}" \
  --arg version "${version}" \
  --arg releaseBaseUrl "${release_base_url}" \
  --arg linux_amd64 "${linux_amd64_sha}" \
  --arg linux_arm64 "${linux_arm64_sha}" \
  --arg darwin_amd64 "${darwin_amd64_sha}" \
  --arg darwin_arm64 "${darwin_arm64_sha}" \
  '{
    schemaVersion: 1,
    channel: $channel,
    version: $version,
    releaseBaseUrl: $releaseBaseUrl,
    artifacts: {
      "linux/amd64":  {"url": ($releaseBaseUrl + "/linux/amd64/eru"),  "sha256": $linux_amd64},
      "linux/arm64":  {"url": ($releaseBaseUrl + "/linux/arm64/eru"),  "sha256": $linux_arm64},
      "darwin/amd64": {"url": ($releaseBaseUrl + "/darwin/amd64/eru"), "sha256": $darwin_amd64},
      "darwin/arm64": {"url": ($releaseBaseUrl + "/darwin/arm64/eru"), "sha256": $darwin_arm64}
    }
  }' >"${out}/releases/${version}/manifest.json"

if [ "${channel}" = edge ]; then
  channel_dir="${out}/edge"
elif [ -n "${line_dir}" ]; then
  channel_dir="${out}/${line_dir}"
else
  channel_dir="${out}"
fi
mkdir -p "${channel_dir}"
cp "${out}/releases/${version}/manifest.json" "${channel_dir}/manifest.json"

if [ "${signing_channel}" = stable ]; then
  trust_args="--certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity-regexp '^https://github\\.com/mbvlabs/eru/\\.github/workflows/publish-stable\\.yml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+(-(alpha|beta|rc)\\.[0-9]+)?$'"
else
  trust_args="--certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity-regexp '^https://github\\.com/mbvlabs/eru/\\.github/workflows/publish-edge\\.yml@refs/heads/master$'"
fi

sed \
  -e "s|@@CHANNEL_ROOT@@|$(sed_escape "${channel_root}")|g" \
  -e "s|@@CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
  -e "s|@@COSIGN_TRUST_ARGS@@|$(sed_escape "${trust_args}")|g" \
  scripts/install.sh >"${channel_dir}/install.sh"
chmod 0755 "${channel_dir}/install.sh"

# A line release may also promote the root pointer. Prepare that installer
# without rebuilding or overwriting the immutable version's artifacts.
if [ -n "${line_dir}" ]; then
  sed \
    -e "s|@@CHANNEL_ROOT@@|$(sed_escape "${base_url}")|g" \
    -e "s|@@CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
    -e "s|@@COSIGN_TRUST_ARGS@@|$(sed_escape "${trust_args}")|g" \
    scripts/install.sh >"${out}/install.sh"
  chmod 0755 "${out}/install.sh"
fi
if grep -q '@@' "${channel_dir}/install.sh"; then
  echo "install.sh still contains unsubstituted placeholders" >&2
  exit 1
fi

printf 'Built %s %s release in %s\n' "${channel}" "${version}" "${out}"
find "${out}" -print | sort
