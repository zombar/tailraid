# Runs inside the Windows guest through its own mount (docs/vms.md step 7c); the same
# probes as perm.sh. The SMB mapping lives in this SSH session, so the script maps it
# first and drops it at the end: every run is a fresh session, which is what a user's
# Explorer gets.
param([string]$Mount, [string]$Op, [string]$Run, [string]$Sub)
$ErrorActionPreference = 'Stop'
tailraid mount | Out-Null
try {
  $root = "$Mount\_vms\$Run\perm"
  # Outcome: ok, refused (STATUS_ACCESS_DENIED as .NET reports it) or the error's text
  function Outcome([scriptblock]$b) {
    try { & $b | Out-Null; 'ok' } catch {
      $m = $_.Exception.Message
      if ($m -match 'denied|unauthorized') { 'refused' }
      elseif ($m -match 'not found|cannot find|does not exist') { 'missing' }
      else { 'err:' + ($m -replace '\s+', '_') }
    }
  }
  function Rd($p) { [IO.File]::ReadAllBytes($p) }
  # a write at a non-zero offset: open for writing, 3 bytes at 4 KiB, no truncation
  function Wr($p) {
    $fs = [IO.File]::Open($p, 'Open', 'ReadWrite')
    try { $fs.Seek(4096, 'Begin') | Out-Null; $fs.Write([Text.Encoding]::ASCII.GetBytes('sam'), 0, 3); $fs.Flush($true) } finally { $fs.Close() }
  }
  function Ls($p) { (Get-ChildItem -Force $p | Sort-Object Name | ForEach-Object Name) -join ' ' }
  switch ($Op) {
    'setup' {
      foreach ($d in 'r', 'w', 'other') {
        New-Item -ItemType Directory -Force "$root\$d" | Out-Null
        $buf = New-Object byte[] 8192
        [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buf)
        [IO.File]::WriteAllBytes("$root\$d\f", $buf)
      }
      Write-Output 'setup ok'
    }
    'probe' {
      $rr = Outcome { Rd "$root\r\f" }
      $wr = Outcome { Wr "$root\r\f" }
      $ww = Outcome { Wr "$root\w\f" }
      $so = Outcome { Get-Item "$root\other\f" }
      try { $ls = Ls $root } catch { $ls = Outcome { throw $_.Exception } }
      Write-Output "read-r=$rr write-r=$wr write-w=$ww stat-other=$so list=$ls"
    }
    'diag' {
      # what this mount's view holds, for a probe that failed: each folder's listing and
      # each file's stat, with the error text when there is one
      foreach ($d in 'r', 'w', 'other') {
        try { $l = (Get-ChildItem -Force "$root\$d" | ForEach-Object Name) -join ' ' } catch { $l = 'ERR ' + $_.Exception.Message }
        try { $i = Get-Item -Force "$root\$d\f"; $st = "size=$($i.Length)" } catch { $st = 'ERR ' + $_.Exception.Message }
        Write-Output "diag ${d}: list=[$l] f: $st"
      }
    }
    'read' { Write-Output (Outcome { Rd "$root\$Sub\f" }) }
    'write' { Write-Output (Outcome { Wr "$root\$Sub\f" }) }
    default { throw "unknown op $Op" }
  }
} finally {
  tailraid umount 2>$null | Out-Null
}
