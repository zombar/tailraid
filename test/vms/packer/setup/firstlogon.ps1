# First logon inside the Windows golden image (run by autounattend.xml from the unattend
# CD): OpenSSH with the rig's key and PowerShell as its shell, Tailscale, no sleep, the
# tailraid directory on PATH, the SMB client settings docs/15 wants.
$ErrorActionPreference = 'Stop'
$log = 'C:\tailraid\firstlogon.log'
New-Item -ItemType Directory -Force C:\tailraid, C:\tailraid\d1, C:\tailraid\d2 | Out-Null
Start-Transcript -Path $log -Append | Out-Null
$cd = Split-Path -Parent $MyInvocation.MyCommand.Path
Write-Output "unattend CD at $cd"

# OpenSSH Server from the Win32-OpenSSH MSI (the capability needs Windows Update)
Start-Process msiexec -Wait -ArgumentList "/i `"$cd\OpenSSH-ARM64.msi`" /qn /norestart ADDLOCAL=Server"
New-ItemProperty -Path HKLM:\SOFTWARE\OpenSSH -Name DefaultShell -PropertyType String -Force `
  -Value 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' | Out-Null
New-Item -ItemType Directory -Force C:\ProgramData\ssh | Out-Null
Copy-Item "$cd\authorized_keys" C:\ProgramData\ssh\administrators_authorized_keys -Force
icacls.exe C:\ProgramData\ssh\administrators_authorized_keys /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F' | Out-Null
Set-Service sshd -StartupType Automatic
if (-not (Get-NetFirewallRule -Name OpenSSH-Server-In-TCP -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -Name OpenSSH-Server-In-TCP -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
}
Get-NetConnectionProfile | Set-NetConnectionProfile -NetworkCategory Private
Start-Service sshd

# Tailscale: unattended mode so the service runs without a logged-in user; the login
# server is given at `tailscale up` time by the rig
$msi = Get-ChildItem "$cd\tailscale-setup-*.msi" | Select-Object -First 1
Start-Process msiexec -Wait -ArgumentList "/i `"$($msi.FullName)`" /quiet /norestart TS_UNATTENDEDMODE=always TS_NOLAUNCH=1 TS_INSTALLUPDATES=never"

# never sleep, never hibernate; no Windows Update reboots mid-round
powercfg /change standby-timeout-ac 0
powercfg /change monitor-timeout-ac 0
powercfg /hibernate off
Stop-Service wuauserv -ErrorAction SilentlyContinue
Set-Service wuauserv -StartupType Disabled
# tailraid.exe lands in C:\tailraid (scripts/vms/binaries.sh)
$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if ($path -notlike '*C:\tailraid*') { [Environment]::SetEnvironmentVariable('Path', "$path;C:\tailraid", 'Machine') }
# the SMB client as docs/15 expects it: signed, no guest fallback
Set-SmbClientConfiguration -RequireSecuritySignature $true -EnableInsecureGuestLogons $false -Force
Set-Content C:\tailraid\image-ready.txt (Get-Date -Format o)
Write-Output 'first logon done'
Stop-Transcript | Out-Null
