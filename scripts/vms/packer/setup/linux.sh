#!/usr/bin/env bash
# Packer provisioner inside the Ubuntu base image (docs/vms.md).
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
echo "$SSH_PUBLIC_KEY" >> "$HOME/.ssh/authorized_keys" && chmod 600 "$HOME/.ssh/authorized_keys"
sudo -n true || { echo "admin needs passwordless sudo"; exit 1; }

sudo apt-get update -q
sudo apt-get install -y -q nfs-common curl ca-certificates
curl -fsSL https://tailscale.com/install.sh | sudo sh
sudo systemctl enable tailscaled
sudo systemctl disable --now unattended-upgrades 2>/dev/null || true
mkdir -p "$HOME/tailraid-d1" "$HOME/tailraid-d2"
sudo hostnamectl set-hostname tailraid-linux
tailscale version
echo "linux image ready"
