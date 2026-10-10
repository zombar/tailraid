# The macOS golden image: Cirrus Labs' Tahoe base for Tart, plus the rig's SSH key and
# Tailscale (the open-source daemon from Homebrew, installed as a system daemon).
# Instances are `tart clone`s of tailraid-mac-image (scripts/vms/vm.sh).
variable "ssh_public_key" { type = string }

source "tart-cli" "mac" {
  vm_base_name = "ghcr.io/cirruslabs/macos-tahoe-base:latest"
  vm_name      = "tailraid-mac-image"
  cpu_count    = 4
  memory_gb    = 6
  disk_size_gb = 60
  ssh_username = "admin"
  ssh_password = "admin"
  ssh_timeout  = "300s"
  headless     = true
}

build {
  sources = ["source.tart-cli.mac"]
  provisioner "shell" {
    environment_vars = ["SSH_PUBLIC_KEY=${var.ssh_public_key}"]
    script           = "setup/mac.sh"
  }
}
