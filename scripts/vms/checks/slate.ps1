# Runs inside the Windows guest (elevated SSH): wipe every trace of tailraid and write a
# fresh config for the round (docs/vms.md step 2). usage: slate.ps1 [<endpoint>]: the
# host:port the other guests reach this one at (the host's forwarded UDP port, since the
# guest sits behind QEMU's NAT; docs/20 §5 mesh.endpoint).
param([string]$Endpoint = '')
$ErrorActionPreference = 'Continue'
$cfgdir = 'C:\ProgramData\tailraid'
tailraid umount 2>$null | Out-Null
tailraid uninstall --forget-identity 2>$null | Out-Null
tailraid reset --yes --discard-dirty 2>$null | Out-Null
net use T: /delete /y 2>$null | Out-Null
sc.exe delete tailraid 2>$null | Out-Null
# the SMB client's metadata caches at the Windows defaults, pinned each round: the
# setting is the machine's and outlives a round, and a guest left with them off (one
# round on 2026-10-15) failed every Rename-Item with "The parameter is incorrect"
Set-SmbClientConfiguration -DirectoryCacheLifetime 10 -FileInfoCacheLifetime 10 -FileNotFoundCacheLifetime 5 -Force 2>$null | Out-Null
Remove-Item -Recurse -Force $cfgdir, 'C:\tailraid\d1', 'C:\tailraid\d2', 'C:\tailraid\snap' -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $cfgdir, 'C:\tailraid\d1', 'C:\tailraid\d2' | Out-Null
$ErrorActionPreference = 'Stop'
# one derived disk, the SMB gateway on its loopback port, T: as the mount, every /dev
# endpoint on, the soak-length repair knobs; init and join add the share password
@"
node:
  name: tailraid-win
  data_dir: C:\ProgramData\tailraid\data
  disks:
    - path: C:\tailraid\d1
listen:
  raft: 20480
  rpc: 20481
  web: 20482
  nfs: ""
  smb: 127.0.0.1:20445
mount:
  path: "T:"
repair:
  grace: 20s
  fill_after: 20s
log:
  level: info
dev:
  enabled: true
mesh:
  portmap: false
"@ | Set-Content -Encoding ascii "$cfgdir\config.yaml"
if ($Endpoint -ne '') { Add-Content -Encoding ascii "$cfgdir\config.yaml" "  endpoint: $Endpoint" }
Write-Output "config: $cfgdir\config.yaml"
