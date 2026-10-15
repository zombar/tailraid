# Runs inside the Windows guest (an elevated SSH session): register with the rig's headscale.
# usage: win.ps1 <headscale url> <pre-auth key> <hostname>
param([string]$Url, [string]$Key, [string]$Name)
$ErrorActionPreference = 'Stop'
$ts = 'C:\Program Files\Tailscale\tailscale.exe'
if (-not (Test-Path $ts)) { throw "tailscale is not installed at $ts" }
if ($env:COMPUTERNAME -ne $Name) {
  Rename-Computer -NewName $Name -Force | Out-Null
  Write-Output "renamed to $Name (applies at the next boot; tailscale uses --hostname meanwhile)"
}
# the rig's CA: headscale and its DERP relay serve TLS from it
certutil -addstore -f Root C:\tailraid\rig-ca.crt | Out-Null
Restart-Service Tailscale -ErrorAction SilentlyContinue
Start-Service Tailscale -ErrorAction SilentlyContinue
& $ts up --login-server $Url --authkey $Key --hostname $Name --unattended --accept-dns=true --reset
if ($LASTEXITCODE -ne 0) { throw "tailscale up failed with $LASTEXITCODE" }
for ($i = 0; $i -lt 60; $i++) {
  $st = & $ts status --json 2>$null | ConvertFrom-Json
  if ($st -and $st.BackendState -eq 'Running') {
    Write-Output ("tailnet: " + ($st.TailscaleIPs -join ' ') + " " + $st.Self.DNSName)
    exit 0
  }
  Start-Sleep -Seconds 1
}
& $ts status
throw 'tailscale did not reach Running'
