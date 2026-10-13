#!/usr/bin/env bash
# binaries.sh — cross-build tailraid from the working tree for the three guests and copy
# it in: ~/.local/bin/tailraid on macOS and Linux (what `make install` does, and a
# directory the user owns so `tailraid update` can swap the binary beside it),
# C:\tailraid\tailraid.exe on Windows. Every guest must then print the same
# `tailraid version`.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

cd "$VMS_ROOT"
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
out="$VMS_ROOT/dist/vms"
mkdir -p "$out"
for p in darwin/arm64 linux/arm64 windows/arm64; do
  os=${p%/*}; arch=${p#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  log "building $os/$arch ($VERSION)"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -mod=vendor -ldflags "-s -w -X main.version=$VERSION" -o "$out/tailraid_${os}_${arch}${ext}" ./cmd/tailraid
done

for os in ${1:-$OSES}; do
  case "$os" in
    mac|linux)
      arch=linux_arm64; [ "$os" = mac ] && arch=darwin_arm64
      vm_scp "$os" "$out/tailraid_$arch" /tmp/tailraid
      # beside, then rename: macOS kills a binary overwritten in place (a cached code
      # signature), the same reason make install does it
      vm_ssh "$os" "mkdir -p ~/.local/bin && sudo rm -f /usr/local/bin/tailraid && install -m 755 /tmp/tailraid ~/.local/bin/.tailraid.new && mv -f ~/.local/bin/.tailraid.new ~/.local/bin/tailraid"
      ;;
    win)
      # a running service keeps the exe open: stop it before the copy
      # shellcheck disable=SC2016 # PowerShell, expanded in the guest
      vm_ssh win 'if (Get-Service tailraid -ErrorAction SilentlyContinue) { tailraid stop 2>$null | Out-Null }; New-Item -ItemType Directory -Force C:\tailraid | Out-Null' || true
      vm_scp win "$out/tailraid_windows_arm64.exe" "C:/tailraid/tailraid.exe"
      ;;
  esac
  got=$(vm_ssh "$os" "tailraid version" | tr -d '\r')
  [ "$got" = "tailraid $VERSION" ] || die "$os runs \"$got\", want \"tailraid $VERSION\""
  log "$os: $got"
done
echo "$VERSION"
