#!/usr/bin/env bash
# Runs inside the macOS guest: name it for the rig (docs/vms.md). usage: mac.sh <hostname>
set -euo pipefail
host=$1
sudo scutil --set HostName "$host"
sudo scutil --set LocalHostName "$host"
sudo scutil --set ComputerName "$host"
echo "hostname: $(hostname)"
