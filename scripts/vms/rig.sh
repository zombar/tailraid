#!/usr/bin/env bash
# rig.sh up|down|destroy|status — the whole rig: the three guests.
#
# up starts every guest (creating an instance from its golden image when none exists)
# and names a fresh instance; the guests form their mesh per round, by join code
# (docs/20), so nothing else is registered anywhere. down stops everything and keeps
# every state; destroy deletes the instances (the images stay).
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
S="$VMS_ROOT/scripts/vms"

named() { # <os>: the guest already carries its rig name (provisioning ran)
  vm_ssh "$1" 'hostname' 2>/dev/null | tr -d '\r' | grep -qi "^$(vm_name "$1")"
}

case "${1:-}" in
  up)
    for os in ${VMS:-$OSES}; do "$S/vm.sh" "$os" start; done
    for os in ${VMS:-$OSES}; do
      if named "$os"; then log "$os: provisioned already"; else "$S/vm.sh" "$os" provision; fi
    done
    ;;
  down)
    for os in ${VMS:-$OSES}; do "$S/vm.sh" "$os" stop; done
    ;;
  destroy)
    for os in ${VMS:-$OSES}; do "$S/vm.sh" "$os" destroy; done
    rm -rf "$STATE/results"
    ;;
  status)
    for os in $OSES; do "$S/vm.sh" "$os" status; done
    ;;
  *) echo "usage: rig.sh up|down|destroy|status" >&2; exit 2 ;;
esac
