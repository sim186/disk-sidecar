#!/bin/sh
# Builds Sidecar for every system into dist/. Needs only Go; no C compiler, no other tools.
#   ./build.sh            all systems
#   ./build.sh native     only this computer, as dist/sidecar
set -e
cd "$(dirname "$0")"
mkdir -p dist
flags="-s -w"
if [ "$1" = "native" ]; then
  go build -trimpath -ldflags "$flags" -o dist/sidecar ./cmd/sidecar
  echo "built dist/sidecar ($(dist/sidecar version))"
  exit 0
fi
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}
  out="dist/sidecar-$os-$arch"; [ "$os" = windows ] && out="$out.exe"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$flags" -o "$out" ./cmd/sidecar
  echo "built $out"
done
ls -l dist | awk 'NR>1 {printf "%-34s %5.1f MB\n", $9, $5/1e6}'
