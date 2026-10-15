# First logon inside the Windows golden image (run by autounattend.xml from the unattend
# CD): the virtio network driver (the WinPE driver injection does not reach it under
# QEMU, and Windows ARM64 has no inbox virtio-net), OpenSSH with the rig's key and
# PowerShell as its shell, Tailscale, no sleep, the tailraid directory on PATH, the SMB
# client settings docs/15 wants. Native commands log their output; nothing here stops
# the script, so the log tells what happened.
$ErrorActionPreference = 'Continue'
New-Item -ItemType Directory -Force C:\tailraid, C:\tailraid\d1, C:\tailraid\d2 | Out-Null
Start-Transcript -Path C:\tailraid\firstlogon.log -Append | Out-Null
$cd = Split-Path -Parent $MyInvocation.MyCommand.Path
Write-Output "unattend CD at $cd"

# the virtio-net driver (WHQL-signed, so no prompt), then wait for the adapter to get
# an address from QEMU's DHCP
Write-Output 'installing the virtio drivers'
pnputil /add-driver "$cd\drivers\netkvm\netkvm.inf" /install 2>&1 | Out-String | Write-Output
if (Test-Path "$cd\drivers\viorng\viorng.inf") { pnputil /add-driver "$cd\drivers\viorng\viorng.inf" /install 2>&1 | Out-String | Write-Output }
for ($i = 0; $i -lt 60; $i++) {
  $ip = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -like '10.0.2.*' -or $_.IPAddress -like '192.168.*' }
  if ($ip) { Write-Output "network up: $($ip.IPAddress -join ' ')"; break }
  Start-Sleep -Seconds 2
}
Get-NetAdapter | Format-Table -AutoSize | Out-String | Write-Output
Get-NetConnectionProfile -ErrorAction SilentlyContinue | Set-NetConnectionProfile -NetworkCategory Private -ErrorAction SilentlyContinue

# OpenSSH Server from the Win32-OpenSSH MSI (the capability needs Windows Update)
Write-Output 'installing OpenSSH'
$p = Start-Process msiexec -Wait -PassThru -ArgumentList "/i `"$cd\OpenSSH-ARM64.msi`" /qn /norestart ADDLOCAL=Server"
Write-Output "OpenSSH msiexec exit $($p.ExitCode)"
New-ItemProperty -Path HKLM:\SOFTWARE\OpenSSH -Name DefaultShell -PropertyType String -Force `
  -Value 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' | Out-Null
New-Item -ItemType Directory -Force C:\ProgramData\ssh | Out-Null
Copy-Item "$cd\authorized_keys" C:\ProgramData\ssh\administrators_authorized_keys -Force
icacls.exe C:\ProgramData\ssh\administrators_authorized_keys /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F' 2>&1 | Out-String | Write-Output
Set-Service sshd -StartupType Automatic
if (-not (Get-NetFirewallRule -Name OpenSSH-Server-In-TCP -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -Name OpenSSH-Server-In-TCP -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
}
Restart-Service sshd
Write-Output "sshd: $((Get-Service sshd).Status)"

# Tailscale: unattended mode so the service runs without a logged-in user; the login
# server is given at `tailscale up` time by the rig
Write-Output 'installing Tailscale'
$msi = Get-ChildItem "$cd\tailscale-setup-*.msi" | Select-Object -First 1
$p = Start-Process msiexec -Wait -PassThru -ArgumentList "/i `"$($msi.FullName)`" /quiet /norestart TS_UNATTENDEDMODE=always TS_NOLAUNCH=1 TS_INSTALLUPDATES=never"
Write-Output "Tailscale msiexec exit $($p.ExitCode)"

# never sleep, never hibernate; no Windows Update reboots mid-round
powercfg /change standby-timeout-ac 0
powercfg /change monitor-timeout-ac 0
powercfg /hibernate off
Stop-Service wuauserv -ErrorAction SilentlyContinue
Set-Service wuauserv -StartupType Disabled
# tailraid.exe lands in C:\tailraid (scripts/vms/binaries.sh). The Tailscale MSI writes
# the machine Path too (its own directory) and can land after an earlier write, so the
# entry is added, read back and added again until it holds, and the result is logged.
foreach ($try in 1..10) {
  $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  if ($path -like '*C:\tailraid*') { break }
  [Environment]::SetEnvironmentVariable('Path', "$path;C:\tailraid", 'Machine')
  Start-Sleep -Seconds 3
}
Write-Output "machine Path: $([Environment]::GetEnvironmentVariable('Path', 'Machine'))"
# the SMB client as docs/15 expects it: signed, no guest fallback
Set-SmbClientConfiguration -RequireSecuritySignature $true -EnableInsecureGuestLogons $false -Force
Set-Content C:\tailraid\image-ready.txt (Get-Date -Format o)
Write-Output 'first logon done'
Stop-Transcript | Out-Null
