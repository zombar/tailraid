# The macOS golden image: Cirrus Labs' Tahoe base for Tart, plus the rig's SSH key and
# the disk directories; the mesh needs nothing installed (docs/20).
# Instances are `tart clone`s of tailraid-mac-image (scripts/vms/vm.sh).
variable "ssh_public_key" { type = string }
variable "bridge" { type = string } # the host interface the guest is bridged onto (common.sh VMS_BRIDGE)

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
  run_extra_args = ["--net-bridged", var.bridge] # bridged, as vm.sh runs the instances (common.sh says why)
  ip_extra_args  = ["--resolver", "arp"]       # a bridged guest leaves no DHCP lease on the host
}

build {
  sources = ["source.tart-cli.mac"]
  provisioner "shell" {
    environment_vars = ["SSH_PUBLIC_KEY=${var.ssh_public_key}"]
    script           = "setup/mac.sh"
  }
}
