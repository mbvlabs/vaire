#!/usr/bin/env bash
set -euo pipefail

# Candidate, not yet verified on macOS. Build natively on each supported
# architecture and the oldest supported macOS/SDK baseline. System libedit
# and its Apple-provided dependencies are part of that baseline, not bundled.
# OpenSSL is bundled dynamically; verify-full uses an explicit PostgreSQL root.crt,
# not the macOS Keychain. Revisit pinned versions for security updates.
#
# Usage: TMPDIR=/path/without/spaces scripts/build-psql-darwin.sh /new/bundle
# CI must run on BOTH native arm64 and x86_64 macOS runners:
#   scripts/build-psql-darwin.sh "$PWD/psql-bundle"
#   otool -L psql-bundle/psql; otool -L psql-bundle/lib/libpq.5.dylib
#   lipo -info psql-bundle/psql; lipo -info psql-bundle/lib/libpq.5.dylib
#   mv psql-bundle relocated-psql
#   env -u DYLD_LIBRARY_PATH -u DYLD_FALLBACK_LIBRARY_PATH relocated-psql/psql --version
# Also test against a disposable TLS PostgreSQL server with a test CA: connect
# with sslmode=verify-full, sslrootcert=<CA>, and a matching host; then check
# that an otherwise identical connection with a nonmatching certificate name
# fails specifically with a hostname mismatch. Test interactive editing in a
# terminal. Sign and notarize the complete bundle only after those checks.

pg_version=18.7
# https://ftp.postgresql.org/pub/source/v18.7/postgresql-18.7.tar.bz2.sha256
pg_sha256=441dfae4280cae3e0425e5f876f80559fb0731c140f1aae22b3e5a54d0411bfd
ssl_version=3.5.5
# https://github.com/openssl/openssl/releases/download/openssl-3.5.5/openssl-3.5.5.tar.gz.sha256
ssl_sha256=b28c91532a8b65a1f983b4c28b7488174e4a01008e29ce8e69bd789f28bc2a89
output="${1:?usage: scripts/build-psql-darwin.sh /new/output/directory}"
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) target=darwin64-arm64-cc; arch=arm64 ;;
  Darwin/x86_64) target=darwin64-x86_64-cc; arch=x86_64 ;;
  *) echo "unsupported native build host" >&2; exit 1 ;;
esac
for tool in curl shasum tar make clang perl otool install_name_tool lipo codesign; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done
if [ -e "$output" ] || [ -L "$output" ]; then
  echo "output already exists: $output" >&2; exit 1
fi
# Do not allow host package-manager flags or loader overrides to affect the build.
unset CFLAGS CPPFLAGS CXXFLAGS LDFLAGS LIBS CC CXX SDKROOT
unset DYLD_LIBRARY_PATH DYLD_FALLBACK_LIBRARY_PATH DYLD_INSERT_LIBRARIES
work="$(mktemp -d "${TMPDIR:-/tmp}/vaire-psql.XXXXXXXX")"
case "$work" in *[[:space:]]*) echo "scratch path must not contain whitespace: $work" >&2; exit 1 ;; esac
echo "build directory retained at $work" >&2
curl -fLsS "https://ftp.postgresql.org/pub/source/v${pg_version}/postgresql-${pg_version}.tar.bz2" -o "$work/pg.tar.bz2"
curl -fLsS "https://github.com/openssl/openssl/releases/download/openssl-${ssl_version}/openssl-${ssl_version}.tar.gz" -o "$work/ssl.tar.gz"
echo "$pg_sha256  $work/pg.tar.bz2" | shasum -a 256 -c -
echo "$ssl_sha256  $work/ssl.tar.gz" | shasum -a 256 -c -
tar -xjf "$work/pg.tar.bz2" -C "$work"
tar -xzf "$work/ssl.tar.gz" -C "$work"
pg="$work/postgresql-$pg_version"
ssl="$work/openssl-$ssl_version"
prefix="$work/openssl"
mkdir -p "$work/bundle/lib" "$work/bundle/licenses"
cp "$pg/COPYRIGHT" "$work/bundle/licenses/postgresql.txt"
cp "$ssl/LICENSE.txt" "$work/bundle/licenses/openssl.txt"

(
  cd "$ssl"
  ./Configure "$target" shared no-module no-tests --prefix="$prefix" --libdir=lib --openssldir=/etc/ssl
  make -j"${JOBS:-2}"
  make install_sw
)
(
  cd "$pg"
  # libedit and ncurses come from the macOS SDK; no Homebrew dependencies.
  CPPFLAGS="-I$prefix/include" LDFLAGS="-L$prefix/lib -L$prefix/lib64" \
    ./configure --prefix="$work/unused-prefix" --with-openssl \
    --with-libedit-preferred --without-icu --without-zlib --without-gssapi
  make -C src/backend generated-headers
  # These subdirectory targets recursively build shared dependencies. Parallel
  # makes can race each other while recreating libpgcommon and libpgport.
  make -j1 -C src/bin/psql
  make -j1 -C src/bin/pg_dump
)
cp "$pg/src/bin/psql/psql" "$work/bundle/psql"
cp "$pg/src/bin/pg_dump/pg_dump" "$pg/src/bin/pg_dump/pg_restore" "$work/bundle/"
cp -L "$pg/src/interfaces/libpq/libpq.5.dylib" "$work/bundle/lib/libpq.5.dylib"
cp -L "$prefix/lib/libssl.3.dylib" "$prefix/lib/libcrypto.3.dylib" "$work/bundle/lib/"

# Rewrite only edges to the OpenSSL libraries just built and the expected
# libpq edge. Reject every other non-system dependency below.
psql="$work/bundle/psql"
libpq="$work/bundle/lib/libpq.5.dylib"
libssl="$work/bundle/lib/libssl.3.dylib"
libcrypto="$work/bundle/lib/libcrypto.3.dylib"
ssl_id="$(otool -D "$libssl" | tail -n +2)"
crypto_id="$(otool -D "$libcrypto" | tail -n +2)"
[ -n "$ssl_id" ] && [ -n "$crypto_id" ] || { echo "missing OpenSSL install name" >&2; exit 1; }
for binary in "$psql" "$work/bundle/pg_dump" "$work/bundle/pg_restore"; do
otool -L "$binary" | tail -n +2 | awk '{print $1}' > "$work/psql-deps"
pq_count=0
while IFS= read -r dep; do
  case "$dep" in
    */libpq.5.dylib)
      install_name_tool -change "$dep" '@executable_path/lib/libpq.5.dylib' "$binary"
      pq_count=$((pq_count + 1)) ;;
  esac
done < "$work/psql-deps"
[ "$pq_count" -eq 1 ] || { echo "expected exactly one libpq dependency" >&2; exit 1; }
done

# Skip each dylib's own ID, which otool -L includes in its output.
for binary in "$psql" "$work/bundle/pg_dump" "$work/bundle/pg_restore" "$libpq" "$libssl"; do
  otool -L "$binary" | tail -n +2 | awk '{print $1}' > "$work/openssl-deps"
  while IFS= read -r dep; do
    if [ "$dep" = "$ssl_id" ] && [ "$binary" != "$libssl" ]; then
      case "$binary" in
        "$psql" | "$work/bundle/pg_dump" | "$work/bundle/pg_restore")
          install_name_tool -change "$dep" '@executable_path/lib/libssl.3.dylib' "$binary" ;;
        *) install_name_tool -change "$dep" '@loader_path/libssl.3.dylib' "$binary" ;;
      esac
    elif [ "$dep" = "$crypto_id" ]; then
      case "$binary" in
        "$psql" | "$work/bundle/pg_dump" | "$work/bundle/pg_restore")
          install_name_tool -change "$dep" '@executable_path/lib/libcrypto.3.dylib' "$binary" ;;
        *) install_name_tool -change "$dep" '@loader_path/libcrypto.3.dylib' "$binary" ;;
      esac
    fi
  done < "$work/openssl-deps"
done
install_name_tool -id '@loader_path/libpq.5.dylib' "$libpq"
install_name_tool -id '@loader_path/libssl.3.dylib' "$libssl"
install_name_tool -id '@loader_path/libcrypto.3.dylib' "$libcrypto"

for binary in "$psql" "$work/bundle/pg_dump" "$work/bundle/pg_restore" "$libpq" "$libssl" "$libcrypto"; do
  lipo -verify_arch "$arch" "$binary"
  otool -L "$binary" | tail -n +2 | awk '{print $1}' > "$work/current-deps"
  while IFS= read -r dep; do
    case "$dep" in
      '@executable_path/lib/libpq.5.dylib' | '@loader_path/libpq.5.dylib' | \
      '@executable_path/lib/libssl.3.dylib' | '@executable_path/lib/libcrypto.3.dylib' | \
      '@loader_path/libssl.3.dylib' | '@loader_path/libcrypto.3.dylib' | \
      /usr/lib/libSystem.B.dylib | /usr/lib/libedit.3.dylib | \
      /usr/lib/libncurses.5.4.dylib | /usr/lib/libiconv.2.dylib) ;;
      *) echo "unhandled dependency in $binary: $dep" >&2; exit 1 ;;
    esac
  done < "$work/current-deps"
done
# Ensure the bundled TLS libraries are actually used.
for dep in '@loader_path/libssl.3.dylib' '@loader_path/libcrypto.3.dylib'; do
  otool -L "$libpq" | awk '{print $1}' | grep -Fxq "$dep" || {
    echo "libpq missing dependency: $dep" >&2; exit 1
  }
done
otool -L "$libssl" | awk '{print $1}' | grep -Fxq '@loader_path/libcrypto.3.dylib' || {
  echo "libssl missing bundled libcrypto dependency" >&2; exit 1
}
# Require the intended BSD line editor rather than silently building without it.
otool -L "$psql" | grep -Fq '/usr/lib/libedit.3.dylib' || {
  echo "psql is not linked to system libedit" >&2; exit 1
}
printf 'postgresql %s %s\nopenssl %s %s\nmacos-system libedit, libSystem and other allowlisted libraries (provided by macOS)\n' \
  "$pg_version" "$pg_sha256" "$ssl_version" "$ssl_sha256" > "$work/bundle/dependencies.txt"
# install_name_tool invalidates existing Mach-O signatures (including arm64
# ad-hoc signatures). Re-sign every modified file before executing it.
for binary in "$libcrypto" "$libssl" "$libpq" "$psql" "$work/bundle/pg_dump" "$work/bundle/pg_restore"; do
  codesign --force --sign - "$binary"
  codesign --verify --strict "$binary"
done
for binary in "$psql" "$work/bundle/pg_dump" "$work/bundle/pg_restore"; do
  env -u DYLD_LIBRARY_PATH -u DYLD_FALLBACK_LIBRARY_PATH "$binary" --version
done
cp -R "$work/bundle" "$output"
