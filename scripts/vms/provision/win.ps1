# Runs inside the Windows guest (an elevated SSH session): name it for the rig
# (docs/vms.md). usage: win.ps1 <hostname>
param([string]$Name)
$ErrorActionPreference = 'Stop'
if ($env:COMPUTERNAME -ieq $Name) {
  Write-Output "hostname $Name"
} else {
  Rename-Computer -NewName $Name -Force | Out-Null
  Write-Output "renamed to $Name (applies at the next boot; the node's name is its config's)"
}
