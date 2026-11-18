#!/usr/bin/env bash
set -euo pipefail

release_root="${1:?usage: scripts/sign-release.sh RELEASE_ROOT}"
issuer="https://token.actions.githubusercontent.com"
workflow_ref="${GITHUB_WORKFLOW_REF:?GITHUB_WORKFLOW_REF is required}"
identity="https://github.com/${workflow_ref}"

shopt -s nullglob
release_directories=("${release_root}"/releases/*)
if [ "${#release_directories[@]}" -ne 1 ] || [ ! -d "${release_directories[0]}" ]; then
  echo "release root must contain exactly one version directory" >&2
  exit 1
fi
version_root="${release_directories[0]}"

manifest_value() {
  sed -n "s/.*\"${1}\": *\"\\([^\"]*\\)\".*/\\1/p" "$2" | head -1
}

version_manifest="${version_root}/manifest.json"
if ! jq -e '.artifacts | type == "object"' "${version_manifest}" >/dev/null 2>&1; then
  echo "version manifest is invalid: ${version_manifest}" >&2
  exit 1
fi
version="$(manifest_value version "${version_manifest}")"
[ -n "${version}" ] || { echo "version manifest is missing its version" >&2; exit 1; }
if [[ ! "${version}" =~ ^edge-[0-9a-f]{12}$ &&
  ! "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$ ]]; then
  echo "version manifest has an invalid version: ${version}" >&2
  exit 1
fi
if [[ "${version}" =~ ^edge-[0-9a-f]{12}$ ]]; then
  expected_channel=edge
  expected_identity="https://github.com/mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master"
else
  expected_identity="https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v${version}"
  case "${version}" in
    *-alpha.* | *-beta.*) expected_channel=edge ;;
    *-rc.*) expected_channel=rc ;;
    *) expected_channel=stable ;;
  esac
fi
if [ "${identity}" != "${expected_identity}" ]; then
  echo "workflow identity ${identity} does not match release ${version}" >&2
  exit 1
fi
manifest_channel="$(manifest_value channel "${version_manifest}")"
if [ "${manifest_channel}" != "${expected_channel}" ]; then
  echo "version manifest channel ${manifest_channel} does not match ${expected_channel}" >&2
  exit 1
fi

if [ -f "${release_root}/manifest.json" ]; then
  channel_manifest="${release_root}/manifest.json"
elif [ -f "${release_root}/edge/manifest.json" ]; then
  channel_manifest="${release_root}/edge/manifest.json"
elif [ -f "${release_root}/rc/manifest.json" ]; then
  channel_manifest="${release_root}/rc/manifest.json"
else
  channel_manifests=("${release_root}"/[0-9]*.x/manifest.json)
  if [ "${#channel_manifests[@]}" -ne 1 ]; then
    echo "channel manifest is missing under ${release_root}" >&2
    exit 1
  fi
  channel_manifest="${channel_manifests[0]}"
fi
channel_version="$(manifest_value version "${channel_manifest}")"
channel_channel="$(manifest_value channel "${channel_manifest}")"
if [ "${channel_version}" != "${version}" ] || [ "${channel_channel}" != "${expected_channel}" ]; then
  echo "channel manifest does not match the immutable version manifest" >&2
  exit 1
fi

artifacts=(
  "${channel_manifest}"
  "${version_manifest}"
)
platforms=()
while IFS= read -r platform; do
  case "${platform}" in
    linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64) ;;
    *)
      echo "version manifest contains an unsupported platform: ${platform}" >&2
      exit 1
      ;;
  esac
  platforms+=("${platform}")
done < <(jq -r '.artifacts | keys_unsorted[]' "${version_manifest}")
[ "${#platforms[@]}" -gt 0 ] || {
  echo "version manifest has no artifacts: ${version_manifest}" >&2
  exit 1
}
for platform in "${platforms[@]}"; do
  artifacts+=("${version_root}/${platform}/vaire")
done

for artifact in "${artifacts[@]}"; do
  [ -f "${artifact}" ] || { echo "release artifact is missing: ${artifact}" >&2; exit 1; }
  cosign sign-blob --yes --bundle "${artifact}.sigstore.json" "${artifact}"
  cosign verify-blob "${artifact}" \
    --bundle "${artifact}.sigstore.json" \
    --certificate-identity "${identity}" \
    --certificate-oidc-issuer "${issuer}"
done
