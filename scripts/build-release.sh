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
  stable | edge | rc | stable-[0-9]*.x) ;;
  *)
    echo "channel must be stable, stable-<major>.x (for example stable-1.x), rc or edge" >&2
    exit 1
    ;;
esac
if [[ ! "${version}" =~ ^edge-[0-9a-f]{12}$ &&
  ! "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$ ]]; then
  echo "versions must be edge-<12 hex characters> or MAJOR.MINOR.PATCH, optionally -alpha.N, -beta.N or -rc.N" >&2
  exit 1
fi

# The manifest channel is the version's release family: an edge hash, alpha
# and beta are edge, an rc is rc, and a final release is stable. Every named
# version is signed by the stable workflow's tag identity.
case "${version}" in
  edge-* | *-alpha.* | *-beta.*) signing_channel=edge ;;
  *-rc.*) signing_channel=rc ;;
  *) signing_channel=stable ;;
esac
case "${channel}" in
  edge)
    [ "${signing_channel}" = edge ] ||
      { echo "edge channel requires an edge hash, alpha or beta version" >&2; exit 1; }
    ;;
  rc)
    [ "${signing_channel}" = rc ] ||
      { echo "rc channel requires an rc version" >&2; exit 1; }
    ;;
  stable | stable-[0-9]*.x)
    [ "${signing_channel}" = stable ] ||
      { echo "stable channel requires a final release" >&2; exit 1; }
    ;;
esac
line_dir=""
if [[ "${channel}" == stable-[0-9]*.x ]]; then
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

platforms=()
if [ -n "${RELEASE_PLATFORMS:-}" ]; then
  # RELEASE_PLATFORMS is a whitespace-separated allowlist of platforms.
  # shellcheck disable=SC2086
  for platform in ${RELEASE_PLATFORMS}; do
    case "${platform}" in
      linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64) ;;
      *)
        echo "unknown release platform ${platform}" >&2
        exit 1
        ;;
    esac
    for selected in "${platforms[@]}"; do
      if [ "${selected}" = "${platform}" ]; then
        echo "duplicate release platform ${platform}" >&2
        exit 1
      fi
    done
    platforms+=("${platform}")
  done
fi
if [ "${#platforms[@]}" -eq 0 ]; then
  platforms=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)
fi

channel_root="${base_url}"
if [ "${channel}" = edge ]; then
  channel_root="${base_url}/edge"
elif [ "${channel}" = rc ]; then
  channel_root="${base_url}/rc"
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

pkg="github.com/mbvlabs/vaire/internal/buildinfo"
commit="$(git rev-parse --short=12 HEAD)"
ldflags="-s -w -X ${pkg}.Version=${version} -X ${pkg}.Commit=${commit} -X ${pkg}.Channel=${signing_channel}"
build_args=(go build -trimpath -ldflags "${ldflags}")

build_one() {
  local os="$1"
  local arch="$2"
  local platform_dir="${out}/releases/${version}/${os}/${arch}"
  mkdir -p "${platform_dir}" || return
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" "${build_args[@]}" -o "${platform_dir}/vaire" ./cmd/vaire || return
  local hex
  hex="$(sha256_hex "${platform_dir}/vaire")" || return
  printf '%s  vaire\n' "${hex}" >"${platform_dir}/vaire.sha256" || return
  printf '%s' "${hex}"
}

release_base_url="${base_url}/releases/${version}"
artifacts_json='{}'
usql_json='{}'
usql_version=v0.21.4
for platform in "${platforms[@]}"; do
  sha="$(build_one "${platform%/*}" "${platform#*/}")"
  artifacts_json="$(jq -c \
    --arg platform "${platform}" \
    --arg url "${release_base_url}/${platform}/vaire" \
    --arg sha "${sha}" \
    '. + {($platform): {"url": $url, "sha256": $sha}}' <<<"${artifacts_json}")"
  case "${platform}" in
    linux/amd64) upstream_sha=78bd9b221e223d7a954d41f51e9eca98bdd94b401618367ba0f3887abebd44fc ;;
    linux/arm64) upstream_sha=ccad89d6f4c67a9bf595df0aa8a550e0a9e3d6a6f9356356ba1e164e311335e4 ;;
    darwin/amd64) upstream_sha=60feb0d73b2e29e4ecf6b36bf34b23c230d8233c778893b0b56811036d16d17e ;;
    darwin/arm64) upstream_sha=cee9fd2117c17fd622fe0df37b334c356b80cf48d6e9bb164084830ae4ce6d05 ;;
  esac
  platform_dir="${out}/releases/${version}/${platform}"
  archive="${platform_dir}/usql.tar.bz2"
  curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/xo/usql/releases/download/${usql_version}/usql-${usql_version#v}-${platform%/*}-${platform#*/}.tar.bz2" -o "${archive}"
  [ "$(sha256_hex "${archive}")" = "${upstream_sha}" ] || { echo "usql archive checksum mismatch for ${platform}" >&2; exit 1; }
  tar -xjf "${archive}" -C "${platform_dir}" usql LICENSE
  rm "${archive}"
  mv "${platform_dir}/LICENSE" "${platform_dir}/usql.LICENSE"
  chmod 0755 "${platform_dir}/usql"
  usql_sha="$(sha256_hex "${platform_dir}/usql")"
  printf '%s  usql\n' "${usql_sha}" >"${platform_dir}/usql.sha256"
  usql_json="$(jq -c --arg platform "${platform}" --arg sha "${usql_sha}" \
    --arg url "${release_base_url}/${platform}/usql" \
    '. + {($platform): {"url": $url, "sha256": $sha}}' <<<"${usql_json}")"
done

jq -n \
  --arg channel "${signing_channel}" \
  --arg version "${version}" \
  --arg releaseBaseUrl "${release_base_url}" \
  --argjson artifacts "${artifacts_json}" \
  --argjson usqlArtifacts "${usql_json}" \
  '{
    schemaVersion: 1,
    channel: $channel,
    version: $version,
    releaseBaseUrl: $releaseBaseUrl,
    artifacts: $artifacts,
    usqlArtifacts: $usqlArtifacts
  }' >"${out}/releases/${version}/manifest.json"

if [ "${channel}" = edge ]; then
  channel_dir="${out}/edge"
elif [ "${channel}" = rc ]; then
  channel_dir="${out}/rc"
elif [ -n "${line_dir}" ]; then
  channel_dir="${out}/${line_dir}"
else
  channel_dir="${out}"
fi
mkdir -p "${channel_dir}"
cp "${out}/releases/${version}/manifest.json" "${channel_dir}/manifest.json"

render_installer() {
  local root="$1" family="$2" pointer="$3" target="$4" pinned="${5:-}"
  local expressions=(
    -e "s|@@CHANNEL_ROOT@@|$(sed_escape "${root}")|g"
    -e "s|@@CHANNEL@@|$(sed_escape "${family}")|g"
    -e "s|@@POINTER_CHANNEL@@|$(sed_escape "${pointer}")|g"
    -e "s|@@ORIGIN@@|$(sed_escape "${base_url}")|g"
  )
  if [ -n "${pinned}" ]; then
    expressions+=(-e "s|^pinned_version=\"\"$|pinned_version=\"$(sed_escape "${pinned}")\"|")
  fi
  sed "${expressions[@]}" scripts/install.sh >"${target}"
  chmod 0755 "${target}"
}

render_installer "${channel_root}" "${signing_channel}" "${channel}" "${channel_dir}/install.sh"

# Version-specific installers must read the immutable manifest, not a channel pointer.
render_installer "${release_base_url}" "${signing_channel}" "${channel}" \
  "${out}/releases/${version}/install.sh" "${version}"

# A line release may also promote the root pointer. Prepare that installer
# without rebuilding or overwriting the immutable version's artifacts.
if [ -n "${line_dir}" ]; then
  render_installer "${base_url}" "${signing_channel}" stable "${out}/install.sh"
fi

installers=("${channel_dir}/install.sh" "${out}/releases/${version}/install.sh")
[ -z "${line_dir}" ] || installers+=("${out}/install.sh")
if grep -q '@@' "${installers[@]}"; then
  echo "install.sh still contains unsubstituted placeholders" >&2
  exit 1
fi
grep -Fxq "pinned_version=\"${version}\"" "${out}/releases/${version}/install.sh" || {
  echo "pinned installer is not pinned to ${version}" >&2
  exit 1
}

printf 'Built %s %s release in %s\n' "${channel}" "${version}" "${out}"
find "${out}" -print | sort
