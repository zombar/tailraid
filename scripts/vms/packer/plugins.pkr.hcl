# The Packer plugins the golden images need (scripts/vms/host-setup.sh runs packer init).
packer {
  required_plugins {
    tart = {
      version = ">= 1.13.0"
      source  = "github.com/cirruslabs/tart"
    }
    qemu = {
      version = ">= 1.1.0"
      source  = "github.com/hashicorp/qemu"
    }
  }
}
