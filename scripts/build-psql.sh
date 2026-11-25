#!/usr/bin/env bash
set -euo pipefail

# Candidate only: native Linux builds on a Debian-family build host. macOS
# needs a separate, tested install_name/dependency implementation. Run on the
# oldest supported Linux baseline, not on an arbitrary developer workstation.
#
# Usage: TMPDIR=/path/to/scratch scripts/build-psql.sh /new/output/directory
# Output: psql, pg_dump, pg_restore, lib/, licenses/, and dependencies.txt. Sign the entire bundle,
# not just psql: its private shared libraries are executable release content.

version=18.7
# https://ftp.postgresql.org/pub/source/v18.7/postgresql-18.7.tar.bz2.sha256
source_sha256=441dfae4280cae3e0425e5f876f80559fb0731c140f1aae22b3e5a54d0411bfd
output="${1:?usage: scripts/build-psql.sh /new/output/directory}"
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64 | Linux/aarch64) ;;
  *) echo "unsupported native build host; macOS sidecars are not implemented" >&2; exit 1 ;;
esac
for tool in curl sha256sum tar make cc perl bison flex m4 patchelf ldd dpkg-query readlink; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done
if [ -e "$output" ] || [ -L "$output" ]; then
  echo "output already exists: $output" >&2
  exit 1
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/vaire-psql.XXXXXXXX")"
echo "build directory retained at $work" >&2
curl -fLsS "https://ftp.postgresql.org/pub/source/v${version}/postgresql-${version}.tar.bz2" -o "$work/source.tar.bz2"
echo "$source_sha256  $work/source.tar.bz2" | sha256sum -c -
tar -xjf "$work/source.tar.bz2" -C "$work"
src="$work/postgresql-$version"
mkdir -p "$work/bundle/lib" "$work/bundle/licenses"
cp "$src/COPYRIGHT" "$work/bundle/licenses/postgresql.txt"

(
  cd "$src"
  # No optional authentication or compression integrations; TLS and libedit
  # are required. OpenSSL itself may have extra shared dependencies.
  ./configure --prefix="$work/unused-prefix" --disable-rpath --with-openssl \
    --with-libedit-preferred --without-icu --without-zlib --without-gssapi
  make -C src/backend generated-headers
  make -j"${JOBS:-2}" -C src/bin/psql
  make -j"${JOBS:-2}" -C src/bin/pg_dump
)
cp "$src/src/bin/psql/psql" "$work/bundle/psql"
cp "$src/src/bin/pg_dump/pg_dump" "$src/src/bin/pg_dump/pg_restore" "$work/bundle/"
cp -L "$src/src/interfaces/libpq/libpq.so.5" "$work/bundle/lib/libpq.so.5"

# The loader supplies only glibc components. Every other dependency must be
# explicitly reviewed here and accompanied by the host package's notice.
# ldd resolves the transitive dependency graph, including OpenSSL dependencies.
deps="$work/deps"
: > "$deps"
for tool in psql pg_dump pg_restore; do
  if ! LD_LIBRARY_PATH="$work/bundle/lib" ldd "$work/bundle/$tool" >> "$deps"; then
    echo "could not inspect $tool dependencies" >&2; exit 1
  fi
done
if grep -q 'not found' "$deps"; then
  cat "$deps" >&2; exit 1
fi
for required in libpq.so.5 libssl.so. libcrypto.so. libedit.so.; do
  grep -Fq "$required" "$deps" || { echo "missing required dynamic dependency: $required" >&2; exit 1; }
done
while read -r name arrow path rest; do
  if [ "$arrow" != "=>" ]; then
    case "$name" in linux-vdso.so.* | /lib*/ld-linux*.so.*) continue ;; esac
    echo "unhandled loader entry: $name $arrow $path" >&2; exit 1
  fi
  case "$name" in
    libpq.so.5 | libc.so.6 | libm.so.6 | libdl.so.2 | libpthread.so.0 | libresolv.so.2 | librt.so.1) continue ;;
    libssl.so.* | libcrypto.so.* | libz.so.* | libzstd.so.* | libedit.so.* | libbsd.so.* | libmd.so.* | libtinfo.so.* | libncurses.so.* | libncursesw.so.*) ;;
    *) echo "unhandled non-system dependency: $name" >&2; exit 1 ;;
  esac
  [ -f "$path" ] || { echo "missing library: $name" >&2; exit 1; }
  real="$(readlink -f "$path")"
  owner="$(dpkg-query -S "$real")" || { echo "no package owns $real" >&2; exit 1; }
  package="${owner%%:*}"
  notice="/usr/share/doc/$package/copyright"
  [ -f "$notice" ] || { echo "missing license notice for $package" >&2; exit 1; }
  cp -L "$path" "$work/bundle/lib/$name"
  cp -L "$notice" "$work/bundle/licenses/$package.txt"
  printf '%s %s %s\n' "$name" "$package" "$(dpkg-query -W -f='${Version}' "$package")" >> "$work/bundle/dependencies.txt"
done < "$deps"
for lib in "$work/bundle/lib/"*; do patchelf --set-rpath "\$ORIGIN" "$lib"; done
for tool in psql pg_dump pg_restore; do patchelf --set-rpath "\$ORIGIN/lib" "$work/bundle/$tool"; done
printf 'postgresql %s %s\n' "$version" "$source_sha256" >> "$work/bundle/dependencies.txt"

# Verify the result resolves its private libraries without a build-time path.
for tool in psql pg_dump pg_restore; do
env -u LD_LIBRARY_PATH ldd "$work/bundle/$tool" > "$work/final-deps"
if grep -q 'not found' "$work/final-deps"; then cat "$work/final-deps" >&2; exit 1; fi
while read -r name arrow path rest; do
  if [ "$arrow" != "=>" ]; then
    case "$name" in linux-vdso.so.* | /lib*/ld-linux*.so.*) continue ;; esac
    echo "unhandled loader entry: $name $arrow $path" >&2; exit 1
  fi
  case "$name" in
    libc.so.6 | libm.so.6 | libdl.so.2 | libpthread.so.0 | libresolv.so.2 | librt.so.1) ;;
    *) case "$path" in "$work/bundle/lib/"*) ;; *) echo "unbundled library: $name => $path" >&2; exit 1 ;; esac ;;
  esac
done < "$work/final-deps"
env -u LD_LIBRARY_PATH "$work/bundle/$tool" --version
done
cp -a "$work/bundle" "$output"
