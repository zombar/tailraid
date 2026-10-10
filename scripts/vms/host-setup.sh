#!/usr/bin/env bash
# host-setup.sh — install what the three-VM rig needs on this Mac (docs/vms.md), once.
#
# Tart (from its release tarball: the Homebrew formula does not load under Homebrew 7),
# Packer with the tart and qemu plugins, QEMU, the rig's SSH key, and a check that Docker
# runs. Nothing needs sudo. Re-running is harmless.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

TART_VERSION=2.40.1

need brew
need docker
docker info >/dev/null 2>&1 || die "Docker is not running"
[ "$(uname -m)" = arm64 ] || die "the rig runs arm64 guests under HVF and Virtualization.framework: an Apple Silicon Mac is required"

if ! "$TART" --version >/dev/null 2>&1; then
  log "installing tart $TART_VERSION under ~/.local/tart"
  mkdir -p "$HOME/.local/tart" "$HOME/.local/bin"
  curl -fsSL -o "$HOME/.local/tart/tart.tar.gz" "https://github.com/cirruslabs/tart/releases/download/$TART_VERSION/tart.tar.gz"
  curl -fsSL -o "$HOME/.local/tart/sums.txt" "https://github.com/cirruslabs/tart/releases/download/$TART_VERSION/tart_${TART_VERSION}_checksums.txt"
  (cd "$HOME/.local/tart" && grep tart.tar.gz sums.txt | shasum -a 256 -c - && tar xzf tart.tar.gz)
  ln -sf "$HOME/.local/tart/tart.app/Contents/MacOS/tart" "$HOME/.local/bin/tart"
  TART="$HOME/.local/bin/tart"
fi
log "tart $("$TART" --version)"

brew list --formula hashicorp/tap/packer >/dev/null 2>&1 || { brew tap hashicorp/tap >/dev/null; brew install hashicorp/tap/packer; }
brew list --formula qemu >/dev/null 2>&1 || brew install qemu
log "packer $("$PACKER" version | head -1), $("$QEMU" --version | head -1)"

# the Packer plugins are pinned in scripts/vms/packer/plugins.pkr.hcl
(cd "$RIG/packer" && "$PACKER" init plugins.pkr.hcl)

if [ ! -f "$SSH_KEY" ]; then
  ssh-keygen -q -t ed25519 -N "" -C tailraid-vms -f "$SSH_KEY"
  log "generated $SSH_KEY (baked into the golden images: rebuild them if it changes)"
fi

for iso in "$STATE"/iso/*.iso; do [ -f "$iso" ] && log "Windows ISO: $iso"; done
[ -n "$(ls "$STATE"/iso/*.iso 2>/dev/null)" ] || log "no Windows 11 ARM64 ISO in $STATE/iso/ yet: download it from https://www.microsoft.com/software-download/windows11arm64 before make vms-images"
log "host ready; next: make vms-images"
