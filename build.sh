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

# Smoke check: the new binary must start and answer. It runs on a throwaway
# SQLite file even if the build environment carries DATABASE_URL: a build
# must never touch the real database. PORT=0 lets the kernel pick a free
# port, so the check cannot collide with, or be answered by, anything else
# on the machine; the address is read from the binary's own start line,
# which it prints only once it holds the port.
tmp="$(mktemp -d)"
env -u DATABASE_URL DEV_MODE=1 HOST=127.0.0.1 PORT=0 DB_PATH="$tmp/smoke.db" bin/hive.new >"$tmp/log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true; rm -rf "$tmp"' EXIT

smoke_fail() {
    echo "build.sh: smoke check failed at step: $1" >&2
    [ -n "${2:-}" ] && echo "  $2" >&2
    if kill -0 $pid 2>/dev/null; then echo "  server process: still running" >&2; else echo "  server process: exited" >&2; fi
    echo "  server log:" >&2
    sed 's/^/    /' "$tmp/log" >&2
    rm -f bin/hive.new
    exit 1
}

# fetch PATH: sets $status and $body. --noproxy keeps a proxy configured in
# the build environment from intercepting a request to loopback.
fetch() {
    status="$(curl -s --noproxy '*' --max-time 5 -o "$tmp/body" -w '%{http_code}' "http://$addr$1" 2>"$tmp/curl.err")" || true
    body="$(head -c 400 "$tmp/body" 2>/dev/null || true)"
}

addr=
for _ in $(seq 1 50); do
    addr="$(sed -n 's/.* listening on \([^,]*\),.*/\1/p' "$tmp/log" | head -n 1)"
    [ -n "$addr" ] && break
    kill -0 $pid 2>/dev/null || smoke_fail "start" "the server exited before it was listening"
    sleep 0.2
done
[ -n "$addr" ] || smoke_fail "start" "no 'listening on' line within 10s"

fetch /healthz
[ "$status" = 200 ] && [ "$body" = ok ] ||
    smoke_fail "GET http://$addr/healthz" "wanted 200 with body 'ok'; got status ${status:-none}, body '$body' $(cat "$tmp/curl.err")"
fetch /llms.txt
[ "$status" = 200 ] && grep -q 'Agentic Hive' "$tmp/body" ||
    smoke_fail "GET http://$addr/llms.txt" "wanted 200 containing 'Agentic Hive'; got status ${status:-none}, body starting '$body' $(cat "$tmp/curl.err")"
mv bin/hive.new bin/hive
echo "build.sh: built bin/hive ($(bin/hive version))"
