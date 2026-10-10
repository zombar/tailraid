# Runs inside the Windows guest (elevated SSH): wipe every trace of tailraid and write a
# fresh config for the round (docs/vms.md step 2).
$ErrorActionPreference = 'Continue'
$cfgdir = 'C:\ProgramData\tailraid'
tailraid umount 2>$null | Out-Null
tailraid uninstall 2>$null | Out-Null
tailraid reset --yes --discard-dirty 2>$null | Out-Null
net use T: /delete /y 2>$null | Out-Null
sc.exe delete tailraid 2>$null | Out-Null
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
"@ | Set-Content -Encoding ascii "$cfgdir\config.yaml"
Write-Output "config: $cfgdir\config.yaml"
