#!/bin/sh
# Builds bin/hive. Used by the ash10 panel, CI and the Dockerfile alike.
# Needs no root and nothing preinstalled but curl and tar: if there is no
# usable Go toolchain it fetches the pinned one, checksum-verified, into
# $BUILD_CACHE_DIR/tools (or .cache/tools) and reuses it on later builds.
# A build that fails vet, the tests or the smoke check produces no binary,
# so a broken server never becomes a release.
set -eu
cd "$(dirname "$0")"

GO_VERSION=1.26.11
GO_SHA256_amd64=9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec
GO_SHA256_arm64=6c2f29b458b5add0835f18b776f69c8881a6d88428f1484a6da55755e620757e

cache="${BUILD_CACHE_DIR:-$PWD/.cache}"
export GOCACHE="${GOCACHE:-$cache/go-build}" GOMODCACHE="${GOMODCACHE:-$cache/go-mod}"
export GOFLAGS=-mod=readonly CGO_ENABLED=0

# A Go from 1.21 on fetches the exact toolchain go.mod names by itself,
# verified against the Go checksum database. Anything older, or none, is
# replaced by the pinned release.
if ! command -v go >/dev/null 2>&1 || ! go version | grep -Eq 'go1\.(2[1-9]|[3-9][0-9])'; then
    case "$(uname -m)" in
        x86_64) arch=amd64; sum=$GO_SHA256_amd64 ;;
        aarch64|arm64) arch=arm64; sum=$GO_SHA256_arm64 ;;
        *) echo "build.sh: no pinned Go for $(uname -m); install Go $GO_VERSION" >&2; exit 1 ;;
    esac
    goroot="$cache/tools/go$GO_VERSION"
    if [ ! -x "$goroot/bin/go" ]; then
        echo "build.sh: fetching Go $GO_VERSION"
        mkdir -p "$cache/tools"
        tarball="$cache/tools/go$GO_VERSION.linux-$arch.tar.gz"
        curl -fsSL -o "$tarball" "https://go.dev/dl/go$GO_VERSION.linux-$arch.tar.gz"
        echo "$sum  $tarball" | sha256sum -c - >/dev/null || { echo "build.sh: Go tarball checksum mismatch" >&2; rm -f "$tarball"; exit 1; }
        rm -rf "$goroot.tmp" && mkdir -p "$goroot.tmp"
        tar -xzf "$tarball" -C "$goroot.tmp" --strip-components=1
        mv "$goroot.tmp" "$goroot" && rm -f "$tarball"
    fi
    PATH="$goroot/bin:$PATH"
fi
go version

rm -f bin/hive
go vet ./...
go test -count=1 ./...
mkdir -p bin
go build -trimpath -ldflags='-s -w' -o bin/hive.new .

# Smoke check: the binary must start and answer on a spare port.
tmp="$(mktemp -d)"
port=$((20000 + $$ % 20000))
DEV_MODE=1 HOST=127.0.0.1 PORT=$port DB_PATH="$tmp/smoke.db" bin/hive.new >"$tmp/log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true; rm -rf "$tmp"' EXIT
ok=
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if [ "$(curl -fs "http://127.0.0.1:$port/healthz" 2>/dev/null)" = ok ]; then ok=1; break; fi
    sleep 0.5
done
if [ -z "$ok" ] || ! curl -fs "http://127.0.0.1:$port/llms.txt" | grep -q 'Agentic Hive'; then
    echo "build.sh: smoke check failed" >&2; cat "$tmp/log" >&2; rm -f bin/hive.new; exit 1
fi
mv bin/hive.new bin/hive
echo "build.sh: built bin/hive ($(bin/hive version))"
