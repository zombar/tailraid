#!/usr/bin/env bash
# common.sh — helpers shared by the three-VM rig (docs/vms.md). Sourced, never run.
#
# The rig: one macOS and one Linux guest under Tart (bridged onto the host's LAN), one
# Windows guest under QEMU with user-mode networking (it reaches the host and, through
# it, the Tart guests; SSH and the mesh's UDP port come in on forwarded ports), forming a
# mesh of their own by join code (docs/20): no control server. Every guest is reached
# over SSH as `admin` with the key in test/vms/, which holds nothing but state and is
# ignored by git as a whole.
set -euo pipefail
export LC_ALL=C

VMS_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# Ignored state: ssh key, images, run files, results, Packer's cache and builds. It lives
# under the main checkout's test/vms/ whatever worktree runs the rig (the key is baked
# into the golden images and the guests run once per host), or where VMS_STATE points.
STATE=${VMS_STATE:-"$(dirname "$(git -C "$VMS_ROOT" rev-parse --path-format=absolute --git-common-dir)")/test/vms"}
# shellcheck disable=SC2034 # used by the scripts that source this file
RIG="$VMS_ROOT/scripts/vms"      # the rig's sources: scripts and Packer templates
RUN="$STATE/run"                  # pid files, logs, qcow2 overlays of running instances
IMAGES="$STATE/images"            # the Windows golden image (Tart keeps its own under ~/.tart)
SSH_KEY="$STATE/id_ed25519"
mkdir -p "$RUN" "$IMAGES" "$STATE/results"

# Tools. Tart's Homebrew formula is broken under Homebrew 7, so host-setup installs the
# release tarball under ~/.local; packer and qemu come from Homebrew.
TART=${TART:-$(command -v tart || echo "$HOME/.local/bin/tart")}
PACKER=${PACKER:-$(command -v packer || echo /opt/homebrew/bin/packer)}
QEMU=${QEMU:-$(command -v qemu-system-aarch64 || echo /opt/homebrew/bin/qemu-system-aarch64)}
BREW_PREFIX=${BREW_PREFIX:-$(brew --prefix 2>/dev/null || echo /opt/homebrew)}

# The guests. Instance names are what `tart list` and the pid files show; the golden
# images carry an -image suffix so an instance is always a clone.
OSES=${OSES:-"mac linux win"} # OSES="mac linux" runs the rig without the Windows guest
VM_USER="admin"
vm_name() { echo "tailraid-$1"; }
image_name() { echo "tailraid-$1-image"; }
WIN_SSH_PORT=2222 # the Windows guest's SSH, forwarded by QEMU to 127.0.0.1

log() { printf '[vms %s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "error: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is not installed (run make vms-setup)"; }

# wait_for <seconds> <description> <command...>: polls the command once a second.
wait_for() {
  local secs=$1 what=$2; shift 2
  local i=0
  until "$@"; do
    i=$((i + 1))
    [ "$i" -lt "$secs" ] || die "timed out after ${secs}s waiting for $what"
    sleep 1
  done
}

# ---- addresses ---------------------------------------------------------------------------
# The Tart guests are bridged onto the host's LAN interface (VMS_BRIDGE, en0 by default)
# and take their addresses from the LAN's DHCP, found by ARP (`tart ip --resolver arp`:
# bridged guests leave no lease on the host). Not vmnet NAT: macOS hands each vmnet
# consumer a /24 of its own (192.168.64.0/24 to the first, .65 to the next), and a host
# route for one of those subnets (a static route to the LAN router, a VPN) sends every
# packet for the guests out the wrong interface, which is what stranded the rig on
# 2026-10-14. The Windows guest, behind QEMU's user-mode NAT, reaches the LAN through
# the host, and the host forwards the mesh's UDP port (MESH_PORT) to it (vm.sh), so the
# Windows guest's config names <host LAN address>:MESH_PORT as its endpoint (docs/20 §5,
# checks/slate.ps1) and the Tart guests reach it there.
# shellcheck disable=SC2034 # used by vm.sh and round.sh, which source this file
MESH_PORT=20483
VMS_BRIDGE=${VMS_BRIDGE:-en0}
# tart_gateway: the address the guests reach the host at, the host's own on the bridge
# interface (the name is from the vmnet days, when it was the subnet's .1)
tart_gateway() { ipconfig getifaddr "$VMS_BRIDGE" 2>/dev/null || true; }

# lan_sweep: one ping to every address of the bridge's /24, in parallel, so the host's
# ARP table learns every guest. The ARP resolver reads that table, and it only fills
# for a guest the host has exchanged packets with: the macOS guest announces itself
# (Bonjour), the Ubuntu guest never does, so without the sweep `tart ip` finds it never.
lan_sweep() {
  local host prefix i
  host=$(ipconfig getifaddr "$VMS_BRIDGE" 2>/dev/null) || return 0
  prefix=${host%.*}
  for i in $(seq 1 254); do ping -c 1 -W 1 -q "$prefix.$i" >/dev/null 2>&1 & done
  wait
}

# vm_ip <os>: the guest's current address, or nothing when it is not up.
vm_ip() {
  case "$1" in
    mac|linux)
      local ip
      ip=$("$TART" ip --resolver arp "$(vm_name "$1")" 2>/dev/null || true)
      [ -n "$ip" ] || { lan_sweep; ip=$("$TART" ip --resolver arp "$(vm_name "$1")" 2>/dev/null || true); }
      echo "$ip"
      ;;
    win) nc -z 127.0.0.1 "$WIN_SSH_PORT" 2>/dev/null && echo 127.0.0.1 || true ;;
    *) die "unknown os $1" ;;
  esac
}
vm_port() { case "$1" in win) echo "$WIN_SSH_PORT" ;; *) echo 22 ;; esac; }
# scp_mode <os>: Win32-OpenSSH's SFTP subsystem does not start under the rig's install,
# so copies to Windows use scp's legacy protocol (the remote runs scp.exe through
# PowerShell, which works)
scp_mode() { case "$1" in win) echo "-O $WIN_SSH_OPTS" ;; esac; }

# ---- ssh --------------------------------------------------------------------------------
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
  -o ConnectTimeout=8 -o ServerAliveInterval=15) # no BatchMode: it would refuse the Windows password logon

# Windows is reached with password authentication (admin/admin, set by the answer file):
# a key-authenticated logon on Windows has no LSA logon session the SMB redirector can
# use, so every mapping of the gateway from such a session fails with error 1312, while a
# password logon maps it fine. OpenSSH takes the password from SSH_ASKPASS when forced.
WIN_ASKPASS="$RUN/askpass.sh"
[ -x "$WIN_ASKPASS" ] || { printf '#!/bin/sh\necho admin\n' > "$WIN_ASKPASS"; chmod 700 "$WIN_ASKPASS"; }
export SSH_ASKPASS="$WIN_ASKPASS" SSH_ASKPASS_REQUIRE=force DISPLAY=${DISPLAY:-none} # only a password prompt consults it
WIN_SSH_OPTS="-o PreferredAuthentications=password -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1"

# vm_ssh <os> <command...>: runs a command in the guest. On macOS and Linux it is a bash
# command line; on Windows (whose OpenSSH default shell is PowerShell) PowerShell.
vm_ssh() {
  local os=$1; shift
  local ip
  ip=$(vm_ip "$os")
  [ -n "$ip" ] || die "$os: no address (is it running?)"
  case "$os" in
    win)
      # shellcheck disable=SC2086 # WIN_SSH_OPTS is a list of options, split on purpose
      ssh "${SSH_OPTS[@]}" $WIN_SSH_OPTS -p "$(vm_port "$os")" "$VM_USER@$ip" "$@" ;;
    # a non-interactive SSH shell has no ~/.local/bin (tailraid), /usr/local/bin or
    # Homebrew on its PATH
    *) ssh "${SSH_OPTS[@]}" -p "$(vm_port "$os")" "$VM_USER@$ip" "export PATH=\$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:\$PATH; $*" ;;
  esac
}

# vm_scp <os> <local> <remote path>
vm_scp() {
  local os=$1 ip
  ip=$(vm_ip "$os")
  [ -n "$ip" ] || die "$os: no address (is it running?)"
  # shellcheck disable=SC2046 # scp_mode prints a list of options (or none), split on purpose
  scp -q $(scp_mode "$os") "${SSH_OPTS[@]}" -P "$(vm_port "$os")" "$2" "$VM_USER@$ip:$3"
}

vm_scp_from() { # <os> <remote path> <local>
  local os=$1 ip
  ip=$(vm_ip "$os")
  [ -n "$ip" ] || die "$os: no address (is it running?)"
  # shellcheck disable=SC2046 # scp_mode prints a list of options (or none), split on purpose
  scp -q $(scp_mode "$os") "${SSH_OPTS[@]}" -P "$(vm_port "$os")" "$VM_USER@$ip:$2" "$3"
}

vm_ssh_ok() { vm_ssh "$1" 'exit 0' >/dev/null 2>&1; } # exit 0: a command bash and PowerShell both know

# vm_run_script <os> <script> [args...]: copies a script into the guest and runs it there
# (bash on macOS and Linux, PowerShell on Windows), streaming its output.
vm_run_script() {
  local os=$1 script=$2; shift 2
  local base
  base=$(basename "$script")
  case "$os" in
    win)
      vm_scp "$os" "$script" "C:/tailraid/$base"
      vm_ssh "$os" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\tailraid\\$base $*"
      ;;
    *)
      vm_scp "$os" "$script" "/tmp/$base"
      vm_ssh "$os" "bash /tmp/$base $*"
      ;;
  esac
}

# ---- results ----------------------------------------------------------------------------
# A round appends one JSON line per step to RESULTS (set by round.sh).
record() { # <step> <ok|fail|skip> <detail>; the detail is whatever the guests printed, so a real encoder
  [ -n "${RESULTS:-}" ] || return 0
  python3 -c 'import json, sys, datetime
print(json.dumps({"t": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "first": sys.argv[1], "step": sys.argv[2], "result": sys.argv[3], "detail": sys.argv[4]}))' \
    "${FIRST:-}" "$1" "$2" "$3" >> "$RESULTS"
}
