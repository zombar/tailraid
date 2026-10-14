#!/usr/bin/env bash
# Runs inside a macOS or Linux guest: wipe every trace of tailraid and write a fresh
# config for the round (docs/vms.md step 2). usage: slate.sh <os>
#
# On macOS serve is a root daemon (docs/20 §9): its config and data live in the system
# home, written here as root, and the user's commands (mount, verify, status) read a copy
# under the user's home; the mesh key and the data are wiped as root.
set -uo pipefail
os=$1
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
case "$os" in
  mac)
    cfgdir="/Library/Application Support/tailraid"
    usercfg="$HOME/Library/Application Support/tailraid"
    sudo tailraid umount >/dev/null 2>&1
    sudo tailraid uninstall --forget-identity >/dev/null 2>&1
    sudo tailraid reset --yes --discard-dirty >/dev/null 2>&1
    sudo tailraid umount >/dev/null 2>&1
    sudo rm -rf "$cfgdir" "$usercfg" /Library/Logs/tailraid "$HOME/Library/Logs/tailraid" "$HOME/tailraid-d1" "$HOME/tailraid-d2" "$HOME/TailRAID" "$HOME/tailraid-snap"
    sudo mkdir -p "$cfgdir"
    mkdir -p "$usercfg" "$HOME/tailraid-d1" "$HOME/tailraid-d2"
    ;;
  *)
    cfgdir="$HOME/.config/tailraid"
    tailraid umount >/dev/null 2>&1
    tailraid uninstall --forget-identity >/dev/null 2>&1
    tailraid reset --yes --discard-dirty >/dev/null 2>&1
    tailraid umount >/dev/null 2>&1
    # a join under sudo leaves root-owned markers until its give-back ran; sudo clears them
    sudo rm -rf "$cfgdir" "$HOME/tailraid-d1" "$HOME/tailraid-d2" "$HOME/TailRAID" "$HOME/tailraid-snap"
    sudo rm -f /etc/systemd/system/tailraid.service; sudo systemctl daemon-reload
    mkdir -p "$cfgdir" "$HOME/tailraid-d1" "$HOME/tailraid-d2"
    ;;
esac
# one derived disk (its budget follows the VM disk's free space: the automation phase
# fills it and watches), the mount under the home directory, every /dev endpoint on,
# and the soak-length repair knobs
config() {
  cat <<EOF
node:
  name: tailraid-$os
  data_dir: $cfgdir/data
  disks:
    - path: $HOME/tailraid-d1
listen:
  raft: 20480
  rpc: 20481
  web: 20482
  nfs: 127.0.0.1:20490
mount:
  path: $HOME/TailRAID
repair:
  grace: 20s
  fill_after: 20s
log:
  level: info
dev:
  enabled: true
mesh:
  portmap: false  # the guests sit on the host's LAN: no mapping on the real router
EOF
}
if [ "$os" = mac ]; then
  config | sudo tee "$cfgdir/config.yaml" >/dev/null
  config > "$usercfg/config.yaml"
else
  config > "$cfgdir/config.yaml"
fi
echo "config: $cfgdir/config.yaml"
