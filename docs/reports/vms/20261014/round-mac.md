# Round: first = mac (20261014-211704)

| Step | Result | Detail |
|---|---|---|
| 1 binaries | OK | v0.8.3-57-gc545d06-dirty |
| 2 clean slate | OK | config: C:\ProgramData\tailraid\config.yaml |
| 3 init on mac | OK | join code from mac: ae4vdzcu2i4ggkfa… (356 chars) |
| 4 join by code | OK | win paths: tailraid-mac=wan tailraid-linux=wan |
| 5 membership | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 6 restart after join | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 7 cross-OS data | OK | rename, remove and mtime agree across the three mounts |
| 7b promote | OK | voters: 3 |
| 8 verify | OK | verify exit 0; skipped phases: "phase":"14 diskless host" "phase":"15 s3 bucket" "phase":"19 snapshots" "phase":"22 copies under faults" "phase":"23 many users  |
| 8b update --restart | OK | v0.8.3 predates the mesh and cannot load this config (docs/20: a hard cut); nothing was swapped, nothing to roll back |
| 9 add-disk, snapshot, recover | OK | recovered as a one-voter cluster; files still read |
| 10 teardown | OK | torn down |
