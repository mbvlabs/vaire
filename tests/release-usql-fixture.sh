#!/usr/bin/env bash
# Sourced by offline release tests after their normal mocks are on PATH.
# Only upstream usql downloads and their archive hashes are faked.
fixture_dir="${work}/usql-fixture"
mkdir -p "${fixture_dir}/bin" "${fixture_dir}/archive"
printf '#!/bin/sh\necho fixture-usql\n' >"${fixture_dir}/archive/usql"
printf 'fixture license\n' >"${fixture_dir}/archive/LICENSE"
tar -cjf "${fixture_dir}/archive.tar.bz2" -C "${fixture_dir}/archive" usql LICENSE
export TEST_USQL_FIXTURE="${fixture_dir}/archive.tar.bz2"
export TEST_CURL_FALLBACK="$(command -v curl)"
export TEST_SHA_FALLBACK="$(command -v sha256sum)"
cat >"${fixture_dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
for arg in "$@"; do
  case "$arg" in
    https://github.com/xo/usql/releases/download/*)
      while [ "$1" != -o ]; do shift; done
      cp "${TEST_USQL_FIXTURE}" "$2"
      exit
      ;;
  esac
done
exec "${TEST_CURL_FALLBACK}" "$@"
EOF
cat >"${fixture_dir}/bin/sha256sum" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  */linux/amd64/usql.tar.bz2) hash=78bd9b221e223d7a954d41f51e9eca98bdd94b401618367ba0f3887abebd44fc ;;
  */linux/arm64/usql.tar.bz2) hash=ccad89d6f4c67a9bf595df0aa8a550e0a9e3d6a6f9356356ba1e164e311335e4 ;;
  */darwin/amd64/usql.tar.bz2) hash=60feb0d73b2e29e4ecf6b36bf34b23c230d8233c778893b0b56811036d16d17e ;;
  */darwin/arm64/usql.tar.bz2) hash=cee9fd2117c17fd622fe0df37b334c356b80cf48d6e9bb164084830ae4ce6d05 ;;
  *) exec "${TEST_SHA_FALLBACK}" "$@" ;;
esac
[ "${TEST_BAD_USQL_ARCHIVE:-0}" = 0 ] || hash=bad
printf '%s  %s\n' "$hash" "$1"
EOF
chmod +x "${fixture_dir}"/bin/*
export PATH="${fixture_dir}/bin:${PATH}"
