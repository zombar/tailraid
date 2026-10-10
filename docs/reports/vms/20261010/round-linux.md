# Round: first = linux (20261010-114733)

| Step | Result | Detail |
|---|---|---|
| 1 binaries | OK | v0.4.1-24-g1f1cd8b |
| 2 clean slate | OK | config: C:\ProgramData\tailraid\config.yaml |
| 3 init on linux | OK | + sudo mount -t nfs -o port=20490,mountport=20490,nfsvers=3,tcp,nolock,noacl,soft 127.0.0.1:/ /home/admin/TailRAID |
| 4 join by name | OK | T: was deleted successfully. |
| 5 membership | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 6 restart after join | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 7 cross-OS data | OK | rename, remove and mtime agree across the three mounts |
| 7b promote | OK | voters: 3 |
| 8 verify | FAIL | verify exit 1; skipped phases: "phase":"14 diskless host" "phase":"4 parity" |
| 10 teardown | OK | torn down |
