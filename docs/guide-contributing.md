# Testing a change and releasing

The two rigs that run the soak scenarios without touching your cluster, and how a release is cut. How the code is written and tested is [../CLAUDE.md](../CLAUDE.md); the test tiers are [09-testing-strategy.md](09-testing-strategy.md).

## Testing a change before you ship it

Two rigs run the soak scenarios without touching your cluster. The Docker one is quick and runs anywhere Docker runs; the VM one is slower but real: three operating systems, their service managers and their mount clients.

### The Docker soak

```sh
make soak          # five nodes (four with two disks, one diskless voter), the quick profile, ~10 min
make soak-single   # one node with four disks
make soak-full     # the long profile, ~1 h
```

Each target cross-compiles the binary for the Docker engine's architecture, brings up `test/soak/docker-compose.yml` (fake tailnet identities, `dev.enabled`, 20 s repair knobs, a worker sidecar per node that reaches the node's loopback NFS gateway, an S3 server for the bucket phase), drives every phase from the host with `cmd/soak`, writes `report.html` and `results.jsonl` in the repository root and tears the cluster down. It needs Docker Desktop (or any engine with Compose) and nothing else. Run it on its own: `make check` alongside it turns ten minutes into an hour.

### Three local VMs

On an Apple Silicon Mac, `make vms-rounds` forms a volume from a macOS, a Linux and a Windows guest on a throwaway tailnet, each OS running `tailraid init` in turn while the other two join it by name, then crashes and restarts the joiners through launchd, systemd and the SCM, moves data across the three mounts, runs `tailraid verify --destructive --restarts` from the next OS, updates every node to the latest release with `tailraid update --restart` and rolls it back, adds a disk, takes a metadata export and recovers a one-voter cluster. It is the tier that would have caught the v0.3.2 join defects.

| Step | Command | Notes |
|---|---|---|
| once | `make vms-setup` | Tart, Packer, QEMU and the rig's SSH key; no sudo |
| once | download the Windows 11 ARM64 ISO into `test/vms/iso/` | from [microsoft.com/software-download/windows11arm64](https://www.microsoft.com/software-download/windows11arm64); the page needs a browser |
| once | `make vms-images` | the golden images: a minute each for macOS and Linux, 30–60 minutes for the unattended Windows install |
| each session | `make vms-up` | starts the three guests |
| | `make vms-rounds` | three rounds, about an hour and a quarter; `FULL=1` runs the long verify profile; summary and reports in `docs/reports/vms/<date>/` |
| | `make vms-round FIRST=linux` | one round; `OSES="mac linux"` runs it on two guests while the Windows image is not built yet |
| | `make vms-down` | stops everything and keeps every state, so the next `vms-up` registers nothing |
| | `make vms-destroy` | removes the instances and the control server's state; the images stay |

The guests take about 120 GB of disk. [docs/vms.md](vms.md) explains what each step proves, how the networking is wired (the Tart guests' NAT, the Windows guest behind QEMU's user-mode NAT, headscale over TLS from a CA the rig generates) and what to do when something does not come up.

## Releasing

```sh
make tag VERSION=v0.3.0
```

That is the whole release. The command opens a pull request with the changelog, merges it and pulls `main`. CI then builds the release from the merge commit, tags it and publishes six binaries, the sums file and its signature (the `MINISIGN_SECRET_KEY` secret holds the key). If the merge is refused, the pull request stays open, and merging it by hand releases the same way.

Details: [docs/12-operations.md](12-operations.md) §4.
