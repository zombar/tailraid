#!/usr/bin/env bash
# images.sh build [mac|linux|win]... | clean — the golden images, built by Packer from
# scripts/vms/packer/*.pkr.hcl (docs/vms.md). Built once; rebuilt when a template changes.
#
# mac and linux come from Cirrus Labs' Tart base images with the rig's SSH key and the
# NFS client added (Tart keeps them under ~/.tart as tailraid-<os>-image). win is an
# unattended Windows 11 ARM64 install from the ISO in test/vms/iso/ (downloaded by hand
# from Microsoft once), with the virtio network driver and OpenSSH fetched here and
# handed to the installer on a second CD; the result lands in test/vms/images/.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

PACKER_DIR="$RIG/packer"       # the templates
BUILD="$STATE/packer/build"      # Packer's output and the unattend CD (state)
CACHE="$STATE/packer/cache"      # downloaded installers and the virtio ISO (state)
VIRTIO_WIN_URL=${VIRTIO_WIN_URL:-https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/latest-virtio/virtio-win.iso}
OPENSSH_TAG=${OPENSSH_TAG:-10.0.0.0p2-Preview} # the ARM64 MSI's name carries the version, so the tag is pinned
OPENSSH_URL=${OPENSSH_URL:-https://github.com/PowerShell/Win32-OpenSSH/releases/download/$OPENSSH_TAG/OpenSSH-ARM64-v${OPENSSH_TAG%%p*}.msi}

[ -f "$SSH_KEY.pub" ] || die "no $SSH_KEY.pub (run make vms-setup)"
pubkey=$(cat "$SSH_KEY.pub")

fetch() { # <url> <file>
  [ -f "$2" ] || { log "fetching $(basename "$2")"; curl -fL --progress-bar -o "$2.part" "$1" && mv "$2.part" "$2"; }
}

build_tart() { # <mac|linux>
  local os=$1
  log "building $(image_name "$os") (Packer tart-cli)"
  (cd "$PACKER_DIR" && PATH="$(dirname "$TART"):$PATH" PACKER_CACHE_DIR="$CACHE/packer" "$PACKER" build -var "ssh_public_key=$pubkey" "$os.pkr.hcl")
}

build_win() {
  local iso
  for iso in "$STATE"/iso/*.iso; do break; done # the first, or the unmatched pattern
  [ -e "$iso" ] || die "no Windows 11 ARM64 ISO in $STATE/iso/: download it from https://www.microsoft.com/software-download/windows11arm64"
  mkdir -p "$CACHE" "$BUILD"
  fetch "$VIRTIO_WIN_URL" "$CACHE/virtio-win.iso"
  fetch "$OPENSSH_URL" "$CACHE/OpenSSH-ARM64.msi"

  # the unattend CD: the answer file at its root, the virtio-net driver for the
  # installer, the installers and the key for the first logon
  local cd="$BUILD/unattend"
  [ -d "$cd" ] && chmod -R u+w "$cd" # files copied off an ISO are read-only
  rm -rf "$cd" && mkdir -p "$cd/drivers"
  cp "$PACKER_DIR/setup/firstlogon.ps1" "$CACHE/OpenSSH-ARM64.msi" "$cd/"
  # the answer file's language must be one the ISO carries: en-GB media (Microsoft names
  # it in the file) gets en-GB, anything else the en-US the file is written with
  case "$(basename "$iso")" in
    *en-gb*|*en-GB*) sed -e 's/en-US/en-GB/g; s/0409:00000409/0809:00000809/g' "$PACKER_DIR/setup/autounattend.xml" > "$cd/autounattend.xml" ;;
    *) cp "$PACKER_DIR/setup/autounattend.xml" "$cd/" ;;
  esac
  cp "$SSH_KEY.pub" "$cd/authorized_keys"
  local mnt
  mnt=$(hdiutil attach -nobrowse -readonly -mountrandom /tmp "$CACHE/virtio-win.iso" | awk '/\/tmp\// { print $NF; exit }')
  [ -d "$mnt/NetKVM/w11/ARM64" ] || { hdiutil detach "$mnt" >/dev/null; die "virtio-win.iso has no NetKVM/w11/ARM64"; }
  cp -R "$mnt/NetKVM/w11/ARM64" "$cd/drivers/netkvm"
  [ -d "$mnt/viorng/w11/ARM64" ] && cp -R "$mnt/viorng/w11/ARM64" "$cd/drivers/viorng"
  hdiutil detach "$mnt" >/dev/null
  chmod -R u+w "$cd/drivers"
  rm -f "$BUILD/unattend.iso"
  hdiutil makehybrid -quiet -iso -joliet -default-volume-name UNATTEND -o "$BUILD/unattend.iso" "$cd"

  # the installer's "press any key to boot from CD" cannot be answered reliably (the
  # firmware reaches it a minute into the boot, a keypress later lands on the setup
  # window), so a copy of the ISO carries Microsoft's no-prompt boot image instead
  local noprompt="$BUILD/windows-noprompt.iso"
  mnt=$(hdiutil attach -nobrowse -readonly -mountrandom /tmp "$iso" | awk '/\/tmp\// { print $NF; exit }')
  python3 "$PACKER_DIR/setup/noprompt.py" "$iso" "$mnt" "$noprompt" || { hdiutil detach "$mnt" >/dev/null; die "patching the ISO failed"; }
  hdiutil detach "$mnt" >/dev/null
  iso=$noprompt

  local vars="$BREW_PREFIX/share/qemu/edk2-arm-vars.fd" # Packer copies it to build/win/efivars.fd
  rm -rf "$BUILD/win"
  log "building tailraid-win.qcow2 (Packer qemu; the unattended install takes 30–60 min)"
  (cd "$PACKER_DIR" && PACKER_CACHE_DIR="$CACHE/packer" "$PACKER" build -var "iso=$iso" -var "unattend_iso=$BUILD/unattend.iso" \
    -var "efivars=$vars" -var "firmware=$BREW_PREFIX/share/qemu/edk2-aarch64-code.fd" -var "out=$BUILD/win" windows.pkr.hcl)
  mv "$BUILD/win/tailraid-win.qcow2" "$IMAGES/tailraid-win.qcow2"
  cp "$BUILD/win/efivars.fd" "$IMAGES/tailraid-win-efivars.fd" # the boot entry the install wrote
  log "image at $IMAGES/tailraid-win.qcow2"
}

case "${1:-build}" in
  build)
    shift || true
    # shellcheck disable=SC2048,SC2086 # the arguments or OSES, a space-separated list
    for os in ${*:-$OSES}; do
      case "$os" in mac|linux) build_tart "$os" ;; win) build_win ;; *) die "unknown os $os" ;; esac
    done
    ;;
  clean)
    for os in mac linux; do "$TART" delete "$(image_name "$os")" 2>/dev/null || true; done
    rm -rf "$IMAGES"/tailraid-win* "$BUILD"
    ;;
  *) echo "usage: images.sh build [mac|linux|win]... | clean" >&2; exit 2 ;;
esac
