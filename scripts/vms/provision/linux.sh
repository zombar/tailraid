#!/usr/bin/env bash
# Runs inside the Linux guest: name it for the rig (docs/vms.md). usage: linux.sh <hostname>
set -euo pipefail
host=$1
sudo hostnamectl set-hostname "$host"
echo "hostname: $(hostname)"
