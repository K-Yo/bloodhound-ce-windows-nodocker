#!/usr/bin/env bash
# Fetch and trim the PostgreSQL binaries that are embedded into the launcher.
#
#   scripts/fetch-postgres.sh [windows|linux|all]
#
# Uses the zonky embedded-postgres distribution (same binaries as the upstream
# BloodHound container's PostgreSQL), plus the Microsoft VC++ runtime DLLs that
# the Windows build links against.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PG_VERSION="${PG_VERSION:-18.7.0}"
TARGET="${1:-all}"
MAVEN="https://repo1.maven.org/maven2/io/zonky/test/postgres"

fetch_pg() {
    local platform=$1 dest=$2
    local jar="embedded-postgres-binaries-${platform}-${PG_VERSION}.jar"
    local url="$MAVEN/embedded-postgres-binaries-${platform}/${PG_VERSION}/${jar}"

    echo "==> fetching PostgreSQL $PG_VERSION for $platform"
    rm -rf "$dest"
    mkdir -p "$dest"
    curl -sSL "$url" -o "$dest/$jar"
    (cd "$dest" && unzip -o -q "$jar" && tar -xf postgres-*.txz && rm -f "$jar" postgres-*.txz)

    rm -rf "$dest/share/locale" "$dest/share/doc" "$dest/share/psqlrc.sample"
    case "$platform" in
        windows-amd64)
            # wxWidgets/ECPG are not used by the server; drop ~18 MB.
            rm -f "$dest"/bin/wx*.dll "$dest"/bin/libecpg* "$dest"/bin/libpgtypes.dll "$dest"/bin/testplug.dll
            ;;
        linux-amd64)
            rm -f "$dest"/lib/*.a
            ;;
    esac
}

fetch_vc_runtime() {
    local dest="$ROOT/pgbin/win/bin"
    local work="$ROOT/build/vcredist"
    echo "==> fetching Microsoft VC++ runtime DLLs"
    mkdir -p "$work"
    curl -sSL https://aka.ms/vs/17/release/vc_redist.x64.exe -o "$work/vc_redist.x64.exe"

    rm -rf "$work/burn" "$work/cabs" "$work/inner"
    mkdir -p "$work/burn" "$work/cabs" "$work/inner"
    # The Burn bootstrapper stores its payload in container "4.cab"; the exact
    # index can change, so extract every container and then look inside the
    # nested cabs for the amd64 runtime binaries.
    7z x -y -t# "$work/vc_redist.x64.exe" -o"$work/burn" >/dev/null
    for cab in "$work"/burn/*.cab; do
        7z x -y "$cab" -o"$work/cabs" >/dev/null
    done
    for inner in "$work"/cabs/*; do
        [ -f "$inner" ] || continue
        7z x -y "$inner" -o"$work/inner" >/dev/null 2>&1 || true
    done

    for dll in msvcp140.dll vcruntime140.dll vcruntime140_1.dll; do
        found="$(find "$work/inner" -name "${dll}_amd64" -print -quit)"
        [ -n "$found" ] || { echo "could not find $dll in VC++ redistributable" >&2; exit 1; }
        cp "$found" "$dest/$dll"
    done
}

case "$TARGET" in
    windows) fetch_pg windows-amd64 "$ROOT/pgbin/win"; fetch_vc_runtime ;;
    linux)   fetch_pg linux-amd64 "$ROOT/pgbin/linux" ;;
    all)     fetch_pg windows-amd64 "$ROOT/pgbin/win"; fetch_vc_runtime
             fetch_pg linux-amd64 "$ROOT/pgbin/linux" ;;
    *) echo "usage: $0 [windows|linux|all]" >&2; exit 2 ;;
esac
