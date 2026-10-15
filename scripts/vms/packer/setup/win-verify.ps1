# Packer provisioner over SSH: the Windows golden image is what the rig needs.
$ErrorActionPreference = 'Stop'
$build = [Environment]::OSVersion.Version.Build
Write-Output "Windows build $build ($env:COMPUTERNAME, $env:PROCESSOR_ARCHITECTURE)"
if ($build -lt 26100) { throw "build $build is older than 24H2 (26100): the SMB loopback mount needs /TCPPORT" }
if ($env:PROCESSOR_ARCHITECTURE -ne 'ARM64') { throw "not an ARM64 Windows" }
if ((Get-Service sshd).StartType -ne 'Automatic') { throw 'sshd is not automatic' }
if (-not (Test-Path 'C:\Program Files\Tailscale\tailscale.exe')) { throw 'tailscale is not installed' }
& 'C:\Program Files\Tailscale\tailscale.exe' version
if (-not (Test-Path C:\tailraid\d1)) { throw 'C:\tailraid\d1 missing' }
$smb = Get-SmbClientConfiguration
if (-not $smb.RequireSecuritySignature -or $smb.EnableInsecureGuestLogons) { throw 'SMB client settings not applied' }
Get-Content C:\tailraid\firstlogon.log -Tail 6
$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if ($path -notlike '*C:\tailraid*') { throw "C:\tailraid not on PATH: $path" }
Write-Output 'windows image verified'
