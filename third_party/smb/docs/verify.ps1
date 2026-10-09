$ErrorActionPreference = 'Continue'
function Step($name) { Write-Output ''; Write-Output ('===== ' + $name) }

# The passwords come from FILES and go straight into the API call. `net use *`
# with redirected stdin does NOT read them -- it authenticates with an EMPTY
# password, which looks exactly like a server that rejects a good one.
$alice = (Get-Content C:\smbtest\alice.pw -Raw).Trim()
$bob   = (Get-Content C:\smbtest\bob.pw -Raw).Trim()

Step 'map the share as alice, who may write'
try { New-SmbMapping -LocalPath Z: -RemotePath \\10.0.2.100\shared -UserName alice -Password $alice -Persistent $false -ErrorAction Stop | Out-Null; 'mapped' }
catch { 'FAILED: ' + $_.Exception.Message }

Step 'what the client says it negotiated'
Get-SmbConnection | Select-Object ServerName,ShareName,Dialect,Signed,Encrypted | Format-Table -AutoSize | Out-String -Width 90

Step 'dir'
cmd /c 'dir Z:\'

Step 'type greeting.txt'
Get-Content Z:\greeting.txt

Step 'copy 512 KiB out and hash it'
Copy-Item Z:\blob.bin C:\smbtest\blob.copy -Force -ErrorAction Continue
if (Test-Path C:\smbtest\blob.copy) { (Get-FileHash C:\smbtest\blob.copy -Algorithm SHA256).Hash }

Step 'read the file in the subdirectory'
Get-Content Z:\sub\inner.txt

Step 'write a file, read it back'
Set-Content -Path Z:\fromwindows.txt -Value 'written by the Windows redirector' -ErrorAction Continue
Get-Content Z:\fromwindows.txt -ErrorAction Continue

Step 'rename it'
Rename-Item Z:\fromwindows.txt Z:\renamed.txt -ErrorAction Continue
Get-ChildItem Z:\ -Name

Step 'and delete it'
Remove-Item Z:\renamed.txt -ErrorAction Continue
'still there: ' + (Test-Path Z:\renamed.txt)

Step 'unmap'
Remove-SmbMapping -LocalPath Z: -Force -ErrorAction Continue

Step 'map as bob, who may only read'
try { New-SmbMapping -LocalPath Y: -RemotePath \\10.0.2.100\shared -UserName bob -Password $bob -Persistent $false -ErrorAction Stop | Out-Null; 'mapped' }
catch { 'FAILED: ' + $_.Exception.Message }
Step 'bob reads'
Get-Content Y:\greeting.txt -ErrorAction Continue
Step 'bob writes -- must fail'
Set-Content -Path Y:\bob.txt -Value 'nope' -ErrorAction Continue
Step 'unmap'
Remove-SmbMapping -LocalPath Y: -Force -ErrorAction Continue

Step 'bob on a share he is not allowed on -- must be refused'
try { New-SmbMapping -LocalPath X: -RemotePath \\10.0.2.100\alices -UserName bob -Password $bob -Persistent $false -ErrorAction Stop | Out-Null; 'MAPPED -- it should not have been' }
catch { 'refused: ' + $_.Exception.Message }

Step 'and alice on the same share -- must work'
try { New-SmbMapping -LocalPath X: -RemotePath \\10.0.2.100\alices -UserName alice -Password $alice -Persistent $false -ErrorAction Stop | Out-Null; 'mapped'; Get-Content X:\greeting.txt; Remove-SmbMapping -LocalPath X: -Force }
catch { 'FAILED: ' + $_.Exception.Message }

Step 'done'
