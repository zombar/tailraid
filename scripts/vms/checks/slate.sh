#!/usr/bin/env bash
# Runs inside a macOS or Linux guest: wipe every trace of tailraid and write a fresh
# config for the round (docs/vms.md step 2). usage: slate.sh <os>
set -uo pipefail
os=$1
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
case "$os" in
  mac) cfgdir="$HOME/Library/Application Support/tailraid" ;;
  *) cfgdir="$HOME/.config/tailraid" ;;
esac
tailraid umount >/dev/null 2>&1
tailraid uninstall >/dev/null 2>&1
tailraid reset --yes --discard-dirty >/dev/null 2>&1
tailraid umount >/dev/null 2>&1
rm -rf "$cfgdir" "$HOME/tailraid-d1" "$HOME/tailraid-d2" "$HOME/TailRAID" "$HOME/tailraid-snap" "$HOME/Library/Logs/tailraid"
if [ "$os" = linux ]; then sudo rm -f /etc/systemd/system/tailraid.service; sudo systemctl daemon-reload; fi
mkdir -p "$cfgdir" "$HOME/tailraid-d1" "$HOME/tailraid-d2"
# one derived disk (its budget follows the VM disk's free space: the automation phase
# fills it and watches), the mount under the home directory, every /dev endpoint on,
# and the soak-length repair knobs
cat > "$cfgdir/config.yaml" <<EOF
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
EOF
echo "config: $cfgdir/config.yaml"
