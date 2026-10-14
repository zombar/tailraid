#!/usr/bin/env bash
# vm.sh <mac|linux|win> create|start|stop|ip|ssh|provision|destroy|status — one guest.
#
# mac and linux are Tart clones of the golden images Packer built (images.sh); win is a
# qcow2 overlay of the Windows golden image run by qemu-system-aarch64 under HVF with
# user-mode networking (SSH forwarded to 127.0.0.1:2222, the mesh's UDP port forwarded
# from every host address, common.sh). `provision` names a fresh instance; `destroy`
# removes the instance and keeps the image.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

os=${1:-}; action=${2:-}
case "$os" in mac|linux|win) ;; *) echo "usage: vm.sh <mac|linux|win> <create|start|stop|ip|ssh|provision|destroy|status>" >&2; exit 2 ;; esac
name=$(vm_name "$os")
image=$(image_name "$os")
pidfile="$RUN/$name.pid"
logfile="$RUN/$name.log"

running() {
  [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null
}

create() {
  case "$os" in
    mac|linux)
      "$TART" list --quiet 2>/dev/null | grep -qx "$image" || die "no image $image: run make vms-images"
      if "$TART" list --quiet | grep -qx "$name"; then log "$name exists"; return; fi
      "$TART" clone "$image" "$name"
      ;;
    win)
      [ -f "$IMAGES/tailraid-win.qcow2" ] || die "no image $IMAGES/tailraid-win.qcow2: run make vms-images"
      if [ -f "$RUN/$name.qcow2" ]; then log "$name exists"; return; fi
      qemu-img create -q -f qcow2 -b "$IMAGES/tailraid-win.qcow2" -F qcow2 "$RUN/$name.qcow2"
      cp "$IMAGES/tailraid-win-efivars.fd" "$RUN/$name-efivars.fd"
      ;;
  esac
  log "created $name"
}

start() {
  if running; then log "$name already running"; return; fi
  case "$os" in
    mac|linux)
      "$TART" list --quiet | grep -qx "$name" || create
      # --no-graphics keeps it headless; the login keychain must be unlocked for macOS
      # guests (a logged-in desktop session is enough)
      nohup "$TART" run --no-graphics --no-audio --no-clipboard "$name" > "$logfile" 2>&1 &
      echo $! > "$pidfile"
      ;;
    win)
      [ -f "$RUN/$name.qcow2" ] || create
      # the same devices the Packer build used: UEFI with the instance's own variable
      # store, the disk on NVMe (an inbox driver), virtio-net (its driver was injected at
      # install), ramfb and a USB keyboard for a console nobody watches
      nohup "$QEMU" \
        -name "$name" -M virt -accel hvf -cpu host -smp 4 -m 6144 \
        -drive "if=pflash,format=raw,readonly=on,file=$BREW_PREFIX/share/qemu/edk2-aarch64-code.fd" \
        -drive "if=pflash,format=raw,file=$RUN/$name-efivars.fd" \
        -drive "file=$RUN/$name.qcow2,if=none,id=nvm,format=qcow2" -device nvme,serial=tailraid,drive=nvm \
        -device virtio-net-pci,netdev=net0 -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$WIN_SSH_PORT-:22,hostfwd=udp::$MESH_PORT-:$MESH_PORT" \
        -device ramfb -device qemu-xhci -device usb-kbd -device usb-tablet -device virtio-rng-pci \
        -display none -monitor "unix:$RUN/$name.mon,server,nowait" > "$logfile" 2>&1 &
      echo $! > "$pidfile"
      ;;
  esac
  log "starting $name (pid $(cat "$pidfile"))"
  wait_for 240 "$name address" has_ip
  wait_for 240 "$name ssh" vm_ssh_ok "$os"
  log "$name up at $(vm_ip "$os")"
}
has_ip() { [ -n "$(vm_ip "$os")" ]; }

stop() {
  if ! running; then log "$name not running"; rm -f "$pidfile"; return; fi
  case "$os" in
    mac|linux) "$TART" stop "$name" 2>/dev/null || true ;;
    win)
      # an ACPI power button through the monitor; Windows shuts down cleanly on it
      printf 'system_powerdown\n' | nc -U "$RUN/$name.mon" >/dev/null 2>&1 || true
      ;;
  esac
  local i=0
  while running && [ "$i" -lt 90 ]; do sleep 1; i=$((i + 1)); done
  if running; then log "$name did not stop in 90 s; killing it"; kill "$(cat "$pidfile")" 2>/dev/null || true; sleep 2; fi
  rm -f "$pidfile"
  log "stopped $name"
}

destroy() {
  stop
  case "$os" in
    mac|linux) "$TART" list --quiet | grep -qx "$name" && "$TART" delete "$name" ;;
    win) rm -f "$RUN/$name.qcow2" "$RUN/$name-efivars.fd" "$RUN/$name.mon" ;;
  esac
  log "destroyed $name"
}

provision() { # a fresh instance gets its rig name; the mesh needs nothing else of it
  running || die "$name is not running"
  case "$os" in
    win) vm_run_script win "$VMS_ROOT/scripts/vms/provision/win.ps1" "$name" ;;
    *) vm_run_script "$os" "$VMS_ROOT/scripts/vms/provision/$os.sh" "$name" ;;
  esac
}

status() {
  if running; then echo "$name: running at $(vm_ip "$os") (pid $(cat "$pidfile"))"; else echo "$name: stopped"; fi
}

case "$action" in
  create) create ;;
  start) start ;;
  stop) stop ;;
  ip) vm_ip "$os" ;;
  ssh) shift 2; vm_ssh "$os" "$@" ;;
  provision) provision ;;
  destroy) destroy ;;
  status) status ;;
  *) echo "usage: vm.sh <mac|linux|win> <create|start|stop|ip|ssh|provision|destroy|status>" >&2; exit 2 ;;
esac
