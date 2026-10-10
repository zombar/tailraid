#!/usr/bin/env bash
# images.sh build [mac|linux|win]... | clean — the golden images, built by Packer from
# test/vms/packer/*.pkr.hcl (docs/vms.md). Built once; rebuilt when a template changes.
#
# mac and linux come from Cirrus Labs' Tart base images with the rig's SSH key and
# Tailscale added (Tart keeps them under ~/.tart as tailraid-<os>-image). win is an
# unattended Windows 11 ARM64 install from the ISO in test/vms/iso/ (downloaded by hand
# from Microsoft once), with the virtio network driver, OpenSSH and Tailscale fetched
# here and handed to the installer on a second CD; the result lands in test/vms/images/.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

PACKER_DIR="$VMS_ROOT/test/vms/packer"
CACHE="$PACKER_DIR/cache"
VIRTIO_WIN_URL=${VIRTIO_WIN_URL:-https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/latest-virtio/virtio-win.iso}
OPENSSH_URL=${OPENSSH_URL:-https://github.com/PowerShell/Win32-OpenSSH/releases/latest/download/OpenSSH-ARM64.msi}
TAILSCALE_MSI_VERSION=${TAILSCALE_MSI_VERSION:-1.104.1}
TAILSCALE_MSI_URL="https://pkgs.tailscale.com/stable/tailscale-setup-$TAILSCALE_MSI_VERSION-arm64.msi"

[ -f "$SSH_KEY.pub" ] || die "no $SSH_KEY.pub (run make vms-setup)"
pubkey=$(cat "$SSH_KEY.pub")

fetch() { # <url> <file>
  [ -f "$2" ] || { log "fetching $(basename "$2")"; curl -fL --progress-bar -o "$2.part" "$1" && mv "$2.part" "$2"; }
}

build_tart() { # <mac|linux>
  local os=$1
  log "building $(image_name "$os") (Packer tart-cli)"
  (cd "$PACKER_DIR" && PATH="$(dirname "$TART"):$PATH" "$PACKER" build -var "ssh_public_key=$pubkey" "$os.pkr.hcl")
}

build_win() {
  local iso
  iso=$(ls "$STATE"/iso/*.iso 2>/dev/null | head -1)
  [ -n "$iso" ] || die "no Windows 11 ARM64 ISO in $STATE/iso/: download it from https://www.microsoft.com/software-download/windows11arm64"
  mkdir -p "$CACHE" "$PACKER_DIR/build"
  fetch "$VIRTIO_WIN_URL" "$CACHE/virtio-win.iso"
  fetch "$OPENSSH_URL" "$CACHE/OpenSSH-ARM64.msi"
  fetch "$TAILSCALE_MSI_URL" "$CACHE/tailscale-setup-$TAILSCALE_MSI_VERSION-arm64.msi"

  # the unattend CD: the answer file at its root, the virtio-net driver for the
  # installer, the installers and the key for the first logon
  local cd="$PACKER_DIR/build/unattend"
  rm -rf "$cd" && mkdir -p "$cd/drivers"
  cp "$PACKER_DIR/setup/autounattend.xml" "$PACKER_DIR/setup/firstlogon.ps1" "$CACHE/OpenSSH-ARM64.msi" "$CACHE"/tailscale-setup-*-arm64.msi "$cd/"
  cp "$SSH_KEY.pub" "$cd/authorized_keys"
  local mnt
  mnt=$(hdiutil attach -nobrowse -readonly -mountrandom /tmp "$CACHE/virtio-win.iso" | awk '/\/tmp\// { print $NF; exit }')
  [ -d "$mnt/NetKVM/w11/ARM64" ] || { hdiutil detach "$mnt" >/dev/null; die "virtio-win.iso has no NetKVM/w11/ARM64"; }
  cp -R "$mnt/NetKVM/w11/ARM64" "$cd/drivers/netkvm"
  [ -d "$mnt/viorng/w11/ARM64" ] && cp -R "$mnt/viorng/w11/ARM64" "$cd/drivers/viorng"
  hdiutil detach "$mnt" >/dev/null
  rm -f "$PACKER_DIR/build/unattend.iso"
  hdiutil makehybrid -quiet -iso -joliet -default-volume-name UNATTEND -o "$PACKER_DIR/build/unattend.iso" "$cd"

  local vars="$PACKER_DIR/build/efivars.fd"
  cp "$BREW_PREFIX/share/qemu/edk2-arm-vars.fd" "$vars"
  rm -rf "$PACKER_DIR/build/win"
  log "building tailraid-win.qcow2 (Packer qemu; the unattended install takes 30–60 min)"
  (cd "$PACKER_DIR" && "$PACKER" build -var "iso=$iso" -var "unattend_iso=$PACKER_DIR/build/unattend.iso" \
    -var "efivars=$vars" -var "firmware=$BREW_PREFIX/share/qemu/edk2-aarch64-code.fd" windows.pkr.hcl)
  mv "$PACKER_DIR/build/win/tailraid-win.qcow2" "$IMAGES/tailraid-win.qcow2"
  cp "$vars" "$IMAGES/tailraid-win-efivars.fd"
  log "image at $IMAGES/tailraid-win.qcow2"
}

case "${1:-build}" in
  build)
    shift || true
    for os in ${*:-$OSES}; do
      case "$os" in mac|linux) build_tart "$os" ;; win) build_win ;; *) die "unknown os $os" ;; esac
    done
    ;;
  clean)
    for os in mac linux; do "$TART" delete "$(image_name "$os")" 2>/dev/null || true; done
    rm -rf "$IMAGES"/tailraid-win* "$PACKER_DIR/build"
    ;;
  *) echo "usage: images.sh build [mac|linux|win]... | clean" >&2; exit 2 ;;
esac
