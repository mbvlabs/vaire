#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?}" "${RELEASE_BASE_URL:?}" "${RELEASE_BUCKET:?}" "${R2_ENDPOINT:?}"
if [[ ! "${VERSION}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$ ]]; then
  echo "invalid stable version" >&2; exit 1
fi
if [[ ! "${RELEASE_BASE_URL}" =~ ^https://[^[:space:]\"\\]+$ ]] ||
  ! grep -Fxq "const ReleaseBaseURL = \"${RELEASE_BASE_URL}\"" internal/buildinfo/buildinfo.go; then
  echo "invalid release base URL" >&2; exit 1
fi
release_url="${RELEASE_BASE_URL}/releases/${VERSION}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "${release_url}/manifest.json" -o "${work}/manifest.json"
curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "${release_url}/manifest.json.sigstore.json" -o "${work}/bundle.json"
cosign verify-blob "${work}/manifest.json" --bundle "${work}/bundle.json" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v${VERSION}"
jq -e --arg version "${VERSION}" --arg url "${release_url}" \
  '.schemaVersion == 1 and .channel == "stable" and .version == $version and .releaseBaseUrl == $url' \
  "${work}/manifest.json" >/dev/null

trust="--certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity-regexp '^https://github\\.com/mbvlabs/vaire/\\.github/workflows/publish-stable\\.yml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+(-(alpha|beta|rc)\\.[0-9]+)?$'"
escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/&/\\\&/g; s/|/\\|/g'; }
sed -e "s|@@CHANNEL_ROOT@@|$(escape "${release_url}")|g" \
    -e 's|@@CHANNEL@@|stable|g' \
    -e "s|@@COSIGN_TRUST_ARGS@@|$(escape "${trust}")|g" \
    scripts/install.sh >"${work}/install.sh"
if grep -q '@@' "${work}/install.sh"; then
  echo "installer contains unsubstituted placeholders" >&2; exit 1
fi
# Conditional creation is atomic: never replace an existing immutable object.
aws s3api put-object --bucket "${RELEASE_BUCKET}" --key "releases/${VERSION}/install.sh" \
  --body "${work}/install.sh" --if-none-match '*' --content-type text/x-shellscript \
  --cache-control 'public,max-age=31536000,immutable' --endpoint-url "${R2_ENDPOINT}" >/dev/null
curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "${release_url}/install.sh" -o "${work}/published.sh"
cmp "${work}/install.sh" "${work}/published.sh"
