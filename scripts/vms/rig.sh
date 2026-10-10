#!/usr/bin/env bash
# rig.sh up|down|destroy|status — the whole rig: headscale and the three guests.
#
# up starts headscale, then every guest (creating an instance from its golden image when
# none exists) and registers a fresh instance with headscale; an instance that was
# registered before reconnects with its stored node key. down stops everything and keeps
# every state; destroy deletes the instances and headscale's state (the images stay).
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
S="$VMS_ROOT/scripts/vms"

registered() { # <os>: the guest is on the tailnet already (tailscale is on every guest's PATH)
  vm_ssh "$1" 'tailscale status --json' 2>/dev/null | tr -d '\r' | grep -q '"BackendState": *"Running"'
}

case "${1:-}" in
  up)
    # the Tart guests first: headscale's address is the host's on their network
    for os in ${VMS:-$OSES}; do "$S/vm.sh" "$os" start; done
    "$S/headscale.sh" up
    for os in ${VMS:-$OSES}; do
      if registered "$os"; then log "$os: on the tailnet already"; else "$S/vm.sh" "$os" provision; fi
    done
    "$S/headscale.sh" nodes
    ;;
  down)
    for os in ${VMS:-$OSES}; do "$S/vm.sh" "$os" stop; done
    "$S/headscale.sh" down
    ;;
  destroy)
    for os in ${VMS:-$OSES}; do "$S/vm.sh" "$os" destroy; done
    "$S/headscale.sh" destroy
    rm -rf "$STATE/results"
    ;;
  status)
    for os in $OSES; do "$S/vm.sh" "$os" status; done
    docker compose -f "$RIG/headscale/compose.yml" ps 2>/dev/null | tail -n +2 || true
    ;;
  *) echo "usage: rig.sh up|down|destroy|status" >&2; exit 2 ;;
esac
