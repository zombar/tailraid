# Round: first = linux (20261014-211704)

| Step | Result | Detail |
|---|---|---|
| 1 binaries | OK | v0.8.3-58-gc977014 |
| 2 clean slate | OK | config: C:\ProgramData\tailraid\config.yaml |
| 3 init on linux | OK | join code from linux: ahh4tpozjd6w36cd… (282 chars) |
| 4 join by code | OK | win paths: tailraid-linux=wan tailraid-mac=wan |
| 5 membership | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 6 restart after join | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 7 cross-OS data | OK | rename, remove and mtime agree across the three mounts |
| 7b promote | OK | voters: 3 |
| 8 verify | OK | verify exit 0; skipped phases: "phase":"14 diskless host" "phase":"15 s3 bucket" "phase":"19 snapshots" "phase":"22 copies under faults" "phase":"23 many users  |
| 8b update --restart | OK | v0.8.3 predates the mesh and cannot load this config (docs/20: a hard cut); nothing was swapped, nothing to roll back |
| 9 add-disk, snapshot, recover | OK | recovered as a one-voter cluster; files still read |
| 10 teardown | OK | torn down |
