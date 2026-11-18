#!/usr/bin/env bash
# Offline checks for release channels, exact signer identities and pointer
# guards. Nothing here reaches the network, a real bucket or a real signer.
set -euo pipefail

source_root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/repo/scripts" "${work}/bin" "${work}/fixtures" "${work}/dest"
cp "${source_root}"/scripts/{build-release,upload-release,sign-release,install}.sh \
  "${work}/repo/scripts/"

cat >"${work}/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ "$1" != -o ]; do shift; done
printf binary >"$2"
EOF
cat >"${work}/bin/git" <<'EOF'
#!/usr/bin/env bash
printf 'abcdef012345\n'
EOF
cat >"${work}/bin/uname" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  -s) echo Linux ;;
  -m) echo x86_64 ;;
esac
EOF
cat >"${work}/bin/id" <<'EOF'
#!/usr/bin/env bash
echo 1000
EOF
real_sha256="$(command -v sha256sum)"
cat >"${work}/bin/sha256sum" <<EOF
#!/usr/bin/env bash
set -euo pipefail
case "\$1" in
  */cosign) printf '4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71  %s\n' "\$1" ;;
  *) exec "${real_sha256}" "\$@" ;;
esac
EOF
cat >"${work}/bin/aws" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${TEST_WORK}/aws.log"
EOF
cat >"${work}/bin/cosign" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${TEST_WORK}/cosign.log"
EOF
cat >"${work}/fixtures/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
file="" bundle="" identity="" issuer=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    verify-blob) shift; file="$1" ;;
    --bundle) shift; bundle="$1" ;;
    --certificate-identity) shift; identity="$1" ;;
    --certificate-identity-regexp) echo "unexpected signer regexp" >&2; exit 1 ;;
    --certificate-oidc-issuer) shift; issuer="$1" ;;
  esac
  shift
done
printf '%s\n' "${identity}" >>"${TEST_WORK}/identities"
[ "${issuer}" = https://token.actions.githubusercontent.com ]
[ "${identity}" = "${TEST_EXPECTED_IDENTITY:?}" ]
[ -f "${file}" ] && [ -f "${bundle}" ]
[ "$(cat "${bundle}")" = "${TEST_EXPECTED_IDENTITY}" ]
EOF
cat >"${work}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
url="" out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://*) url="$1" ;;
    -o) shift; out="$1" ;;
  esac
  shift
done
printf '%s\n' "${url}" >>"${TEST_WORK}/urls"
case "${url}" in
  *cosign-*) cp "${TEST_WORK}/fixtures/cosign" "${out}" ;;
  */manifest.json.sigstore.json) cp "${TEST_WORK}/fixtures/bundle" "${out}" ;;
  */releases/*/manifest.json) cp "${TEST_WORK}/fixtures/immutable-manifest" "${out}" ;;
  */manifest.json) cp "${TEST_WORK}/fixtures/channel-manifest" "${out}" ;;
  */vaire.sigstore.json) cp "${TEST_WORK}/fixtures/bundle" "${out}" ;;
  */vaire) cp "${TEST_WORK}/fixtures/vaire" "${out}" ;;
  *) exit 1 ;;
esac
EOF
chmod +x "${work}"/bin/* "${work}/fixtures/cosign"
export PATH="${work}/bin:${PATH}" TEST_WORK="${work}"
cd "${work}/repo"

build() { bash scripts/build-release.sh "$1" "$2" https://vaire.example >/dev/null; }
expect_build_fail() {
  local version="$1" channel="$2"
  if build "${version}" "${channel}" >"${work}/out" 2>&1; then
    echo "build-release accepted ${version} on ${channel}" >&2
    exit 1
  fi
}

# Channel/version matrix, manifest family and generated installers.
for spec in \
  'edge-abcdef012345 edge edge https://vaire.example/edge edge' \
  '1.0.0-alpha.1 edge edge https://vaire.example/edge edge' \
  '1.0.0-beta.2 edge edge https://vaire.example/edge edge' \
  '1.0.0-rc.3 rc rc https://vaire.example/rc rc' \
  '1.0.0 stable stable https://vaire.example .' \
  '1.2.3 stable-1.x stable https://vaire.example/1.x 1.x'; do
  read -r version channel family channel_root channel_path <<<"${spec}"
  build "${version}" "${channel}"
  out="dist/release-${channel}"
  manifest="${out}/releases/${version}/manifest.json"
  [ "$(jq -r '.channel' "${manifest}")" = "${family}" ]
  [ "$(jq -r '.version' "${manifest}")" = "${version}" ]
  [ -f "${out}/${channel_path}/install.sh" ]
  [ -f "${out}/releases/${version}/install.sh" ]
  grep -Fxq "channel_root=\"${channel_root}\"" "${out}/${channel_path}/install.sh"
  grep -Fxq "channel_root=\"https://vaire.example/releases/${version}\"" \
    "${out}/releases/${version}/install.sh"
  grep -Fxq "pinned_version=\"${version}\"" "${out}/releases/${version}/install.sh"
  grep -Fxq 'pinned_version=""' "${out}/${channel_path}/install.sh"
  if grep -Eq '@@|--certificate-identity-regexp' "${out}/${channel_path}/install.sh" \
    "${out}/releases/${version}/install.sh"; then
    echo "installer for ${version} has placeholders or a broad signer" >&2
    exit 1
  fi
  if [ "${channel}" = stable-1.x ]; then
    [ -f "${out}/install.sh" ]
    grep -Fxq 'channel_root="https://vaire.example"' "${out}/install.sh"
    grep -Fxq 'pointer_channel="stable"' "${out}/install.sh"
  fi
done

expect_build_fail 1.0.0 edge
expect_build_fail 1.0.0-rc.1 edge
expect_build_fail 1.0.0-alpha.1 stable
expect_build_fail 1.0.0-beta.1 stable-1.x
expect_build_fail 1.0.0-rc.1 stable-1.x
expect_build_fail 1.0.0 rc
expect_build_fail 1.0.0-alpha.1 rc
expect_build_fail 1.0.0-beta.1 rc
expect_build_fail edge-abcdef012345 rc
expect_build_fail edge-abcdef012345 stable
expect_build_fail 2.0.0 stable-1.x
expect_build_fail 1.0.0 stable-2.x
expect_build_fail 1.0.0-alpha.01 edge
expect_build_fail edge-abc edge
expect_build_fail 1.0.0 beta

make_installer() {
  local family="$1" pointer="$2" root="$3" target="$4" pinned="${5:-}"
  local args=(
    -e "s|@@CHANNEL_ROOT@@|${root}|g"
    -e "s|@@CHANNEL@@|${family}|g"
    -e "s|@@POINTER_CHANNEL@@|${pointer}|g"
    -e "s|@@ORIGIN@@|https://vaire.example|g"
  )
  [ -z "${pinned}" ] ||
    args+=(-e "s|^pinned_version=\"\"$|pinned_version=\"${pinned}\"|")
  sed "${args[@]}" scripts/install.sh >"${target}"
}

make_fixture() {
  local version="$1" channel="$2" hash identity
  cat >"${work}/fixtures/vaire" <<EOF
#!/usr/bin/env bash
printf '{"version": "${version}", "channel": "${channel}"}\n'
EOF
  chmod +x "${work}/fixtures/vaire"
  hash="$("${real_sha256}" "${work}/fixtures/vaire" | awk '{print $1}')"
  cat >"${work}/fixtures/immutable-manifest" <<EOF
{
  "schemaVersion": 1,
  "channel": "${channel}",
  "version": "${version}",
  "releaseBaseUrl": "https://vaire.example/releases/${version}",
  "artifacts": {
    "linux/amd64": {
      "url": "https://vaire.example/releases/${version}/linux/amd64/vaire",
      "sha256": "${hash}"
    }
  }
}
EOF
  cp "${work}/fixtures/immutable-manifest" "${work}/fixtures/channel-manifest"
  if [[ "${version}" =~ ^edge-[0-9a-f]{12}$ ]]; then
    identity='https://github.com/mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master'
  else
    identity="https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v${version}"
  fi
  printf '%s' "${identity}" >"${work}/fixtures/bundle"
  export TEST_EXPECTED_IDENTITY="${identity}"
}

run_install() {
  rm -rf "${work}/dest"
  mkdir -p "${work}/dest"
  : >"${work}/urls"
  : >"${work}/identities"
  VAIRE_INSTALL_DIR="${work}/dest" VAIRE_INSTALL_ONLY=1 \
    bash "$1" >"${work}/out" 2>&1
}

expect_install_fail() {
  local label="$1" installer="$2" pattern="$3"
  if run_install "${installer}"; then
    echo "${label} unexpectedly installed" >&2
    exit 1
  fi
  grep -Fq "${pattern}" "${work}/out" || {
    echo "${label} failed without '${pattern}':" >&2
    cat "${work}/out" >&2
    exit 1
  }
}

make_installer edge edge https://vaire.example/edge "${work}/edge.sh"
make_installer rc rc https://vaire.example/rc "${work}/rc.sh"
make_installer stable stable https://vaire.example "${work}/stable.sh"
make_installer stable stable-1.x https://vaire.example/1.x "${work}/line.sh"
make_installer stable stable https://vaire.example/releases/1.2.0 "${work}/pinned.sh" 1.2.0

# A hash is signed by the edge workflow; alpha, beta, rc and final versions by
# the exact stable tag identity even when they arrive through another pointer.
make_fixture edge-abcdef012345 edge
run_install "${work}/edge.sh"
grep -Fxq 'https://github.com/mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master' \
  "${work}/identities"
grep -Fxq 'https://vaire.example/edge/manifest.json' "${work}/urls"

make_fixture 1.0.0-alpha.1 edge
run_install "${work}/edge.sh"
grep -Fxq 'https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.1' \
  "${work}/identities"

make_fixture 1.0.0-beta.2 edge
run_install "${work}/edge.sh"

make_fixture 1.0.0-rc.2 rc
run_install "${work}/rc.sh"
grep -Fxq 'https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-rc.2' \
  "${work}/identities"

make_fixture 1.2.0 stable
run_install "${work}/stable.sh"
grep -Fxq 'https://vaire.example/manifest.json' "${work}/urls"
run_install "${work}/line.sh"
grep -Fxq 'https://vaire.example/1.x/manifest.json' "${work}/urls"
run_install "${work}/pinned.sh"
grep -Fxq 'https://vaire.example/releases/1.2.0/manifest.json' "${work}/urls"
if grep -Fxq 'https://vaire.example/manifest.json' "${work}/urls"; then
  echo 'pinned installer read a moving channel pointer' >&2
  exit 1
fi

# A channel never accepts another family's version.
make_fixture 1.0.0-alpha.1 stable
expect_install_fail 'stable pointer with alpha' "${work}/stable.sh" \
  'stable channel requires a final release'
make_fixture 1.0.0-beta.1 rc
expect_install_fail 'rc pointer with beta' "${work}/rc.sh" \
  'rc channel requires an rc release'
make_fixture 1.2.0 edge
expect_install_fail 'edge pointer with a final release' "${work}/edge.sh" \
  'edge channel requires an edge, alpha or beta release'
make_fixture 1.0.0-rc.1 edge
expect_install_fail 'edge pointer with an rc' "${work}/edge.sh" \
  'edge channel requires an edge, alpha or beta release'
make_fixture 2.0.0 stable
expect_install_fail 'line pointer with another major' "${work}/line.sh" \
  'release manifest is on the wrong major line'

# The channel pointer must be byte-identical to the immutable version manifest.
make_fixture 1.2.0 stable
printf '\n' >>"${work}/fixtures/channel-manifest"
expect_install_fail 'channel pointer mismatch' "${work}/stable.sh" \
  'channel manifest differs from its immutable version manifest'

# The exact identity is checked, so an edge-workflow signature cannot stand in
# for a named tag (the previous broad edge-pointer regexp allowed this).
make_fixture 1.0.0-alpha.1 edge
export TEST_EXPECTED_IDENTITY='https://github.com/mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master'
expect_install_fail 'alpha signed by the edge workflow' "${work}/edge.sh" \
  'release manifest signature verification failed'
grep -Fxq 'https://github.com/mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.1' \
  "${work}/identities"

# A pinned installer never reads a moving pointer and refuses a manifest with
# another version.
make_fixture 1.2.0 stable
run_install "${work}/pinned.sh"
grep -Fxq 'https://vaire.example/releases/1.2.0/manifest.json' "${work}/urls"
if grep -Fxq 'https://vaire.example/manifest.json' "${work}/urls"; then
  echo 'pinned installer read a moving channel pointer' >&2
  exit 1
fi
make_fixture 1.2.1 stable
expect_install_fail 'pinned installer with another version' "${work}/pinned.sh" \
  'release manifest version is invalid'

make_upload_tree() {
  local root="$1" channel_dir="$2" version="$3" manifest_channel="$4" dir
  rm -rf "${root}"
  mkdir -p "${root}/releases/${version}"
  printf 'manifest' >"${root}/releases/${version}/manifest.json"
  [ -z "${channel_dir}" ] && return 0
  dir="${root}/${channel_dir}"
  [ "${channel_dir}" != . ] || dir="${root}"
  mkdir -p "${dir}"
  printf '#!/bin/sh\n' >"${dir}/install.sh"
  printf '{"channel": "%s", "version": "%s"}\n' "${manifest_channel}" "${version}" \
    >"${dir}/manifest.json"
  printf '{}' >"${dir}/manifest.json.sigstore.json"
}

expect_upload_fail() {
  local label="$1" root="$2" channel="$3" pattern="$4" mode="$5"
  : >"${work}/aws.log"
  if bash scripts/upload-release.sh "${root}" "${channel}" bucket https://example.invalid "${mode}" \
    >"${work}/out" 2>&1; then
    echo "${label} unexpectedly uploaded" >&2
    exit 1
  fi
  grep -Fq "${pattern}" "${work}/out" || {
    echo "${label} failed without '${pattern}':" >&2
    cat "${work}/out" >&2
    exit 1
  }
  [ ! -s "${work}/aws.log" ] || {
    echo "${label} reached the bucket" >&2
    cat "${work}/aws.log" >&2
    exit 1
  }
}

# Artifacts-only does not need a channel directory or channel files.
make_upload_tree "${work}/tree" "" 1.2.0 stable
: >"${work}/aws.log"
bash scripts/upload-release.sh "${work}/tree" stable bucket https://example.invalid artifacts >/dev/null
grep -Fq 's3 cp' "${work}/aws.log"
grep -Fq -- '--recursive' "${work}/aws.log"
if grep -Fq 'install.sh' "${work}/aws.log"; then
  echo 'artifacts-only mode uploaded channel files' >&2
  exit 1
fi

# Channel-only uploads no immutable artifacts, including on rc.
make_upload_tree "${work}/tree" rc 1.0.0-rc.2 rc
: >"${work}/aws.log"
bash scripts/upload-release.sh "${work}/tree" rc bucket https://example.invalid channel >/dev/null
grep -Fq 's3://bucket/rc/install.sh' "${work}/aws.log"
if grep -Fq -- '--recursive' "${work}/aws.log"; then
  echo 'channel-only mode uploaded immutable artifacts' >&2
  exit 1
fi

make_upload_tree "${work}/tree" edge 1.0.0-alpha.1 edge
: >"${work}/aws.log"
bash scripts/upload-release.sh "${work}/tree" edge bucket https://example.invalid channel >/dev/null
grep -Fq 's3://bucket/edge/install.sh' "${work}/aws.log"

make_upload_tree "${work}/tree" 1.x 1.2.0 stable
: >"${work}/aws.log"
bash scripts/upload-release.sh "${work}/tree" stable-1.x bucket https://example.invalid channel >/dev/null
grep -Fq 's3://bucket/1.x/install.sh' "${work}/aws.log"

make_upload_tree "${work}/tree" . 1.0.0-alpha.1 stable
expect_upload_fail 'stable pointer with alpha' "${work}/tree" stable \
  'refusing to publish prerelease' channel
make_upload_tree "${work}/tree" 1.x 2.0.0 stable
expect_upload_fail 'line pointer with another major' "${work}/tree" stable-1.x \
  'refusing to publish 2.0.0' channel
make_upload_tree "${work}/tree" rc 1.0.0-beta.1 edge
expect_upload_fail 'rc pointer with beta' "${work}/tree" rc \
  'refusing to publish 1.0.0-beta.1 as the rc pointer' channel
make_upload_tree "${work}/tree" rc 1.0.0-rc.2 stable
expect_upload_fail 'rc pointer with the stable family' "${work}/tree" rc \
  'channel manifest names stable, expected rc' channel
make_upload_tree "${work}/tree" edge 1.2.0 stable
expect_upload_fail 'edge pointer with a final release' "${work}/tree" edge \
  'refusing to publish 1.2.0 as the edge pointer' channel
make_upload_tree "${work}/tree" edge 1.0.0-rc.1 rc
expect_upload_fail 'edge pointer with an rc' "${work}/tree" edge \
  'refusing to publish 1.0.0-rc.1 as the edge pointer' channel

# sign-release signs the channel manifest for every channel, including rc, and
# rejects a channel manifest that does not match the immutable version.
export GITHUB_WORKFLOW_REF='mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-rc.2'
build 1.0.0-rc.2 rc
: >"${work}/cosign.log"
bash scripts/sign-release.sh dist/release-rc
grep -Fq 'dist/release-rc/rc/manifest.json' "${work}/cosign.log"
grep -Fq 'refs/tags/v1.0.0-rc.2' "${work}/cosign.log"

export GITHUB_WORKFLOW_REF='mbvlabs/vaire/.github/workflows/publish-edge.yml@refs/heads/master'
build 1.0.0-alpha.1 edge
if bash scripts/sign-release.sh dist/release-edge >"${work}/out" 2>&1; then
  echo 'sign-release accepted a tag version from the edge workflow' >&2
  exit 1
fi
grep -Fq 'does not match release' "${work}/out"

export GITHUB_WORKFLOW_REF='mbvlabs/vaire/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.1'
build 1.0.0-alpha.1 edge
jq '.channel = "stable"' dist/release-edge/edge/manifest.json >"${work}/bad-manifest"
mv "${work}/bad-manifest" dist/release-edge/edge/manifest.json
if bash scripts/sign-release.sh dist/release-edge >"${work}/out" 2>&1; then
  echo 'sign-release accepted a mismatched channel manifest' >&2
  exit 1
fi

build 1.0.0-alpha.1 edge
jq '.channel = "stable"' dist/release-edge/releases/1.0.0-alpha.1/manifest.json \
  >"${work}/bad-manifest"
mv "${work}/bad-manifest" dist/release-edge/releases/1.0.0-alpha.1/manifest.json
if bash scripts/sign-release.sh dist/release-edge >"${work}/out" 2>&1; then
  echo 'sign-release accepted a mismatched immutable manifest' >&2
  exit 1
fi

echo 'release channel tests passed'
