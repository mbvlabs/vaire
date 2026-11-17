#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/scripts" "${work}/mock"
cp "${source_root}"/scripts/{build-release,sign-release,install}.sh "${work}/scripts/"
cat >"${work}/mock/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${TEST_BUILD_FAIL:-0}" = 0 ] || exit 1
while [ "$1" != -o ]; do shift; done
printf binary >"$2"
EOF
cat >"${work}/mock/git" <<'EOF'
#!/usr/bin/env bash
printf 'abcdef012345\n'
EOF
cat >"${work}/mock/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  sign-blob)
    [ "$2" = --yes ]
    [ "$3" = --bundle ]
    printf '%s\n' "$5" >>"${TEST_SIGNED:?}"
    : >"$4"
    ;;
  verify-blob)
    [ "$3" = --bundle ]
    [ -f "$2" ]
    [ -f "$4" ]
    [ "$5" = --certificate-identity ]
    [ "$6" = "https://github.com/${GITHUB_WORKFLOW_REF}" ]
    [ "$7" = --certificate-oidc-issuer ]
    [ "$8" = https://token.actions.githubusercontent.com ]
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "${work}"/mock/*
export PATH="${work}/mock:${PATH}"
cd "${work}"

version=edge-abcdef012345
root=dist/release-edge
manifest="${root}/releases/${version}/manifest.json"
build() {
  RELEASE_PLATFORMS="$1" bash scripts/build-release.sh "${version}" edge https://vaire.mbvlabs.com
}

mkdir -p "${root}/sentinel"
for selection in linux/386 'linux/amd64 linux/amd64' '../../etc/passwd' 'linux/*'; do
  if build "${selection}" >/dev/null 2>&1; then
    echo "accepted invalid release platforms: ${selection}" >&2
    exit 1
  fi
  [ -d "${root}/sentinel" ]
done

if TEST_BUILD_FAIL=1 build linux/amd64 >/dev/null 2>&1; then
  echo 'reported success after a failed compiler invocation' >&2
  exit 1
fi
[ ! -e "${manifest}" ]

build ' linux/amd64 ' >/dev/null
[ "$(jq -r '.artifacts | keys | join(" ")' "${manifest}")" = linux/amd64 ]
cmp "${manifest}" "${root}/edge/manifest.json"
[ -f "${root}/releases/${version}/linux/amd64/vaire" ]
[ ! -e "${root}/releases/${version}/linux/arm64" ]
[ ! -e "${root}/releases/${version}/darwin" ]
installer="${root}/releases/${version}/install.sh"
grep -Fxq "channel_root=\"https://vaire.mbvlabs.com/releases/${version}\"" "${installer}"
if grep -q '@@' "${installer}"; then
  echo 'single-platform installer has unsubstituted placeholders' >&2
  exit 1
fi

# Manifest URLs must never become local signing paths.
jq '.artifacts["linux/amd64"].url = "https://evil.example/../../etc/passwd"' \
  "${manifest}" >"${work}/manifest.json"
mv "${work}/manifest.json" "${manifest}"
cp "${manifest}" "${root}/edge/manifest.json"
export GITHUB_WORKFLOW_REF='mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master'
export TEST_SIGNED="${work}/signed"
bash scripts/sign-release.sh "${root}"
cat >"${work}/expected-signed" <<EOF
${root}/edge/manifest.json
${root}/releases/${version}/manifest.json
${root}/releases/${version}/linux/amd64/vaire
EOF
cmp "${TEST_SIGNED}" "${work}/expected-signed"

for invalid in '{"../../etc/passwd": {}}' '{}'; do
  : >"${TEST_SIGNED}"
  jq --argjson invalid "${invalid}" '.artifacts = $invalid' \
    "${manifest}" >"${work}/manifest.json"
  mv "${work}/manifest.json" "${manifest}"
  if bash scripts/sign-release.sh "${root}" >/dev/null 2>&1; then
    echo 'signed a manifest with invalid or empty platforms' >&2
    exit 1
  fi
  [ ! -s "${TEST_SIGNED}" ]
done

build $'darwin/arm64\tlinux/amd64' >/dev/null
[ "$(jq -r '.artifacts | keys | join(" ")' "${manifest}")" = 'darwin/arm64 linux/amd64' ]

build '' >/dev/null
[ "$(jq -r '.artifacts | keys | join(" ")' "${manifest}")" = 'darwin/amd64 darwin/arm64 linux/amd64 linux/arm64' ]

echo 'release platform tests passed'
