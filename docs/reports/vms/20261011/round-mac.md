# Round: first = mac (20261011-082040)

| Step | Result | Detail |
|---|---|---|
| 1 binaries | OK | v0.4.6-7-g007ed72-dirty |
| 2 clean slate | OK | config: C:\ProgramData\tailraid\config.yaml |
| 3 init on mac | OK | Warning: Identity file /Users/gurgeh/play/tailraid/.claude/worktrees/hotpath-readahead/test/vms/id_ed25519 not accessible: No such file or directory. |
| 4 join by name | OK | T: was deleted successfully. |
| 5 membership | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 6 restart after join | OK | win: 3/3 3/3 policy 2+1 auto=true |
| 7 cross-OS data | OK | rename, remove and mtime agree across the three mounts |
| 7b promote | OK | voters: 3 |
| 8 verify | OK | verify exit 0; skipped phases: "phase":"14 diskless host" "phase":"15 s3 bucket" "phase":"4 parity" |
| 8b update --restart | OK | win: updated to v0.4.6 and rolled back |
| 9 add-disk, snapshot, recover | OK | recovered as a one-voter cluster; files still read |
| 10 teardown | OK | torn down |
