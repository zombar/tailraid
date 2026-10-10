# The Windows golden image: an unattended Windows 11 ARM64 install under QEMU/HVF.
#
# Windows ARM64 ships drivers for NVMe, USB storage and the ramfb display, and nothing
# for virtio, so the disk is NVMe, both CD-ROMs are USB (the install ISO and the rig's
# unattend ISO, built by scripts/vms/images.sh with the answer file, the virtio network
# driver, OpenSSH, Tailscale and the rig's SSH key), and the virtio-net driver is injected
# by the answer file. The qemu plugin only knows ide/sata/scsi/virtio disks, so every
# drive and device is spelled out in qemuargs. The result is tailraid-win.qcow2 plus the
# UEFI variable store the install wrote (the boot entry lives there).
variable "iso" { type = string }          # the Windows 11 ARM64 ISO (downloaded by hand once)
variable "unattend_iso" { type = string } # built by images.sh
variable "efivars" { type = string }      # a writable copy of edk2-arm-vars.fd
variable "firmware" { type = string }     # edk2-aarch64-code.fd

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
  output_directory = "build/win"
  headless         = true
  display          = "none"
  net_device       = "virtio-net-pci"
  qemuargs = [
    ["-drive", "if=pflash,format=raw,readonly=on,file=${var.firmware}"],
    ["-drive", "if=pflash,format=raw,file=${var.efivars}"],
    ["-drive", "file={{ .OutputDir }}/{{ .Name }},if=none,id=nvm,format=qcow2"],
    ["-device", "nvme,serial=tailraid,drive=nvm"],
    ["-drive", "file=${var.iso},if=none,id=installer,media=cdrom,readonly=on"],
    ["-device", "usb-storage,drive=installer"],
    ["-drive", "file=${var.unattend_iso},if=none,id=unattend,media=cdrom,readonly=on"],
    ["-device", "usb-storage,drive=unattend"],
    ["-device", "virtio-net-pci,netdev=user.0"],
    ["-device", "ramfb"],
    ["-device", "qemu-xhci"],
    ["-device", "usb-kbd"],
    ["-device", "usb-tablet"],
    ["-device", "virtio-rng-pci"],
  ]
  # the installer's "Press any key to boot from CD": Enter a few times over the window
  boot_wait    = "4s"
  boot_command = ["<enter><wait3><enter><wait3><enter><wait3><enter><wait3><enter>"]
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
