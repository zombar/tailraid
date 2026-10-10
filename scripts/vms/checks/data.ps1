# Runs inside the Windows guest through its own mount (docs/vms.md step 7); the same
# operations as data.sh. The SMB mapping lives in this SSH session, so the script maps it
# first and drops it at the end.
param([string]$Mount, [string]$Op, [string]$A, [string]$B, [string]$C)
$ErrorActionPreference = 'Stop'
tailraid mount | Out-Null
try {
  $root = "$Mount\_vms"
  function Sha($p) { (Get-FileHash -Algorithm SHA256 $p).Hash.ToLower() }
  switch ($Op) {
    'write' {
      $os = $A; $run = $B; $dir = "$root\$run"
      New-Item -ItemType Directory -Force $dir | Out-Null
      $buf = New-Object byte[] (3 * 1MB)
      [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buf)
      [IO.File]::WriteAllBytes("$dir\$os.bin", $buf)
      Set-Content -NoNewline "$dir\$os-small.txt" "hello from $os at $((Get-Date).ToUniversalTime().ToString('o'))`n"
      Write-Output "$os.bin $(Sha "$dir\$os.bin")"
      Write-Output "$os-small.txt $(Sha "$dir\$os-small.txt")"
    }
    'read' {
      $run = $A; $name = $B; $want = $C; $f = "$root\$run\$name"
      if (-not (Test-Path $f)) { throw "missing $f" }
      $got = Sha $f
      if ($got -ne $want) { throw "${name}: sha $got, want $want" }
      Write-Output "$name ok"
    }
    'overwrite' {
      $run = $A; $name = $B; $f = "$root\$run\$name"
      $buf = New-Object byte[] (64 * 1KB)
      [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buf)
      $fs = [IO.File]::Open($f, 'Open', 'ReadWrite')
      try { $fs.Seek(1MB + 7, 'Begin') | Out-Null; $fs.Write($buf, 0, $buf.Length); $fs.Flush($true) } finally { $fs.Close() }
      Write-Output "$name $(Sha $f)"
    }
    'rename' { $run = $A; Rename-Item "$root\$run\$B" $C; Write-Output "renamed $B to $C" }
    'remove' { $run = $A; Remove-Item "$root\$run\$B"; Write-Output "removed $B" }
    'exists' { $run = $A; if (Test-Path "$root\$run\$B") { Write-Output present } else { Write-Output absent } }
    'mtime' { $run = $A; Write-Output ([int][double]::Parse((Get-Date (Get-Item "$root\$run\$B").LastWriteTimeUtc -UFormat %s))) }
    default { throw "unknown op $Op" }
  }
} finally {
  tailraid umount 2>$null | Out-Null
}
