# The Windows golden image: an unattended Windows 11 ARM64 install under QEMU/HVF.
#
# Windows ARM64 ships drivers for NVMe, USB storage and the ramfb display, and nothing
# for virtio, so the disk is NVMe, both CD-ROMs are USB (the install ISO and the rig's
# unattend ISO, built by scripts/vms/images.sh with the answer file, the virtio network
# driver, OpenSSH, Tailscale and the rig's SSH key), and the virtio-net driver is injected
# by the answer file. The qemu plugin only knows ide/sata/scsi/virtio disks, so every
# drive and device is spelled out in qemuargs. The result is tailraid-win.qcow2 plus the
# UEFI variable store the install wrote (the boot entry lives there).
variable "iso" { type = string }          # the Windows 11 ARM64 ISO, with the no-prompt boot image (images.sh)
variable "unattend_iso" { type = string } # built by images.sh
variable "efivars" { type = string }      # the edk2-arm-vars.fd template
variable "firmware" { type = string }     # edk2-aarch64-code.fd
variable "out" { type = string }          # the build directory (test/vms/packer/build/win: state, not source)

source "qemu" "win" {
  qemu_binary      = "qemu-system-aarch64"
  accelerator      = "hvf"
  machine_type     = "virt"
  cpu_model        = "host"
  cpus             = 4
  memory           = 6144
  iso_url          = var.iso
  iso_checksum     = "none"
  disk_size        = "64G"
  format           = "qcow2"
  vm_name          = "tailraid-win.qcow2"
  output_directory = var.out
  headless         = true
  display          = "none"
  net_device       = "virtio-net-pci"
  # efi_boot keeps the plugin from passing -boot once=d, which qemu-system-aarch64
  # rejects; it copies the variable store to <output>/efivars.fd, which the pflash
  # drive below (the override replaces the plugin's own drive list) and the instances use
  efi_boot          = true
  efi_firmware_code = var.firmware
  efi_firmware_vars = var.efivars
  qemuargs = [
    ["-drive", "if=pflash,format=raw,readonly=on,file=${var.firmware}"],
    ["-drive", "if=pflash,format=raw,file={{ .OutputDir }}/efivars.fd"],
    ["-drive", "file={{ .OutputDir }}/{{ .Name }},if=none,id=nvm,format=qcow2"],
    ["-device", "nvme,serial=tailraid,drive=nvm"],
    ["-device", "qemu-xhci"], # before the USB devices: QEMU wants the bus first
    ["-drive", "file=${var.iso},if=none,id=installer,media=cdrom,readonly=on"],
    ["-device", "usb-storage,drive=installer"],
    ["-drive", "file=${var.unattend_iso},if=none,id=unattend,media=cdrom,readonly=on"],
    ["-device", "usb-storage,drive=unattend"],
    ["-device", "virtio-net-pci,netdev=user.0"],
    ["-device", "ramfb"],
    ["-device", "usb-kbd"],
    ["-device", "usb-tablet"],
    ["-device", "virtio-rng-pci"],
  ]
  # no boot_command: the ISO images.sh hands over carries the no-prompt EFI boot image,
  # so the installer starts on its own (a keypress at the wrong moment would reach the
  # setup window's Cancel button instead)
  boot_wait    = "1s"
  boot_command = []
  # the answer file installs OpenSSH and the rig's key; the install plus first logon take
  # a while on first boot
  communicator     = "ssh"
  ssh_username     = "admin"
  ssh_password     = "admin"
  ssh_timeout      = "90m"
  ssh_handshake_attempts = 200
  shutdown_command = "shutdown /s /t 5 /f /d p:4:1 /c \"packer\""
  shutdown_timeout = "10m"
}

build {
  sources = ["source.qemu.win"]
  provisioner "powershell" {
    script = "setup/win-verify.ps1"
  }
}
