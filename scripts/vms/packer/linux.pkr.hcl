# The Linux golden image: Cirrus Labs' Ubuntu for Tart, plus the rig's SSH key, the NFS
# client and the disk directories. Instances are `tart clone`s of tailraid-linux-image
# (scripts/vms/vm.sh).
variable "ssh_public_key" { type = string }

source "tart-cli" "linux" {
  vm_base_name = "ghcr.io/cirruslabs/ubuntu:latest"
  vm_name      = "tailraid-linux-image"
  cpu_count    = 2
  memory_gb    = 4
  disk_size_gb = 40
  ssh_username = "admin"
  ssh_password = "admin"
  ssh_timeout  = "300s"
  headless     = true
}

build {
  sources = ["source.tart-cli.linux"]
  provisioner "shell" {
    environment_vars = ["SSH_PUBLIC_KEY=${var.ssh_public_key}"]
    script           = "setup/linux.sh"
  }
}
