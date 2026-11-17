#!/usr/bin/env bash
# Build the self-contained BloodHound CE executable.
#
#   scripts/build.sh windows   # dist/BloodHoundCE.exe
#   scripts/build.sh linux     # dist/BloodHoundCE-linux (for local verification)
#
# Prerequisites: toolchain/go, src (BloodHound checkout), pgbin/<os>, and for the
# first run a built UI (this script builds it when cmd/ui/dist is missing).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:-windows}"
if [ -x "$ROOT/toolchain/go/bin/go" ]; then
    GO="$ROOT/toolchain/go/bin/go"
else
    GO="$(command -v go)"
fi
export PATH="$(dirname "$GO"):$PATH"
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0

VERSION="$(git -C "$ROOT/src" rev-parse --short HEAD)"
PAYLOAD_ID="bhce-$VERSION-$(date -u +%Y%m%d%H%M%S)"

build_ui() {
    if [ ! -f "$ROOT/src/cmd/ui/dist/index.html" ]; then
        echo "==> building UI"
        local -a yarn=(corepack yarn)
        if [ -f "$ROOT/src/.yarn/releases/yarn-4.13.0.cjs" ]; then
            yarn=(node "$ROOT/src/.yarn/releases/yarn-4.13.0.cjs")
        fi
        (cd "$ROOT/src" && "${yarn[@]}" install && "${yarn[@]}" build)
    fi
    echo "==> installing UI assets into embed tree"
    rm -rf "$ROOT/src/cmd/api/src/api/static/assets"
    cp -a "$ROOT/src/cmd/ui/dist" "$ROOT/src/cmd/api/src/api/static/assets"
}

build_api() {
    local goos=$1 out=$2
    echo "==> building bhapi for $goos"
    (cd "$ROOT/src/cmd/api/src" && \
        CGO_ENABLED=0 GOOS="$goos" GOARCH=amd64 "$GO" build -trimpath -ldflags "-s -w" -o "$out" ./cmd/bhapi)
}

stage_payload() {
    local goos=$1 bh=$2 pg=$3 zip_out=$4
    local dir="$ROOT/build/payload-$goos"
    local exe=""
    [ "$goos" = windows ] && exe=".exe"

    echo "==> staging payload for $goos"
    rm -rf "$dir"
    mkdir -p "$dir/collectors/sharphound" "$dir/collectors/azurehound" "$dir/extensions"

    cp "$bh" "$dir/bloodhound$exe"
    cp -a "$pg" "$dir/pgsql"

    # SharpHound is fetched and verified by the caller; normalize the file name
    # to the layout the collector download endpoint expects.
    if [ -f "$ROOT/build/collectors/SharpHound_v2.18.0_windows_x86.zip" ]; then
        cp "$ROOT/build/collectors/SharpHound_v2.18.0_windows_x86.zip" "$dir/collectors/sharphound/sharphound-v2.18.0.zip"
        sha256sum "$dir/collectors/sharphound/sharphound-v2.18.0.zip" | sed 's# .*/#  #' > "$dir/collectors/sharphound/sharphound-v2.18.0.zip.sha256"
    fi
    touch "$dir/collectors/azurehound/.keep" "$dir/extensions/.keep"

    echo "$PAYLOAD_ID-$goos" > "$dir/payload.id"
    mkdir -p "$(dirname "$zip_out")"
    rm -f "$zip_out"
    (cd "$dir" && zip -q -r -9 "$zip_out" .)
    rm -rf "$dir"
}

build_launcher() {
    local goos=$1 out=$2
    echo "==> building launcher for $goos"
    (cd "$ROOT/launcher" && CGO_ENABLED=0 GOOS="$goos" GOARCH=amd64 "$GO" build -trimpath -ldflags "-s -w" -o "$out" .)
}

mkdir -p "$ROOT/dist" "$ROOT/build/collectors"

if [ ! -f "$ROOT/build/collectors/SharpHound_v2.18.0_windows_x86.zip" ]; then
    echo "==> fetching SharpHound"
    (cd "$ROOT/build/collectors" && \
        curl -sSLO https://github.com/SpecterOps/SharpHound/releases/download/v2.18.0/SharpHound_v2.18.0_windows_x86.zip && \
        curl -sSLO https://github.com/SpecterOps/SharpHound/releases/download/v2.18.0/SharpHound_v2.18.0_windows_x86.zip.sha256 && \
        sha256sum -c SharpHound_v2.18.0_windows_x86.zip.sha256)
fi

build_ui

case "$TARGET" in
    windows)
        build_api windows "$ROOT/build/bloodhound.exe"
        stage_payload windows "$ROOT/build/bloodhound.exe" "$ROOT/pgbin/win" "$ROOT/launcher/payload/payload.zip"
        build_launcher windows "$ROOT/dist/BloodHoundCE.exe"
        echo "==> $ROOT/dist/BloodHoundCE.exe"
        ;;
    linux)
        build_api linux "$ROOT/build/bloodhound-linux"
        stage_payload linux "$ROOT/build/bloodhound-linux" "$ROOT/pgbin/linux" "$ROOT/launcher/payload/payload.zip"
        build_launcher linux "$ROOT/dist/BloodHoundCE-linux"
        echo "==> $ROOT/dist/BloodHoundCE-linux"
        ;;
    *)
        echo "usage: $0 [windows|linux]" >&2
        exit 2
        ;;
esac
