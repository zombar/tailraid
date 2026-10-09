# Windows validation checklist (M4)

The Windows mount path is **experimental**: the portmap stub, mount helper and service
installer are cross-compiled and unit-tested, but have never run on real Windows hardware.
This is the manual pass that promotes it. Target: a Windows 11 **Pro/Enterprise/Education**
machine on the tailnet (Home edition has no NFS client; use the S3 gateway + rclone there).

Every item is *to verify at M4*. Record the Windows build, tailraid version and date when
ticking.

## 0. Build & stage

- [ ] `GOOS=windows GOARCH=amd64 make build` on the Mac (or `make release`), copy
      `tailraid.exe` to a stable path (e.g. `C:\tailraid\tailraid.exe`); the service
      registers this exact path
- [ ] Tailscale for Windows installed and connected; `tailscale status --json` works from
      PowerShell (tailraid shells out to `C:\Program Files\Tailscale\tailscale.exe`)

## 1. Prerequisites tailraid prints

- [ ] `tailraid.exe mount` on a machine without the NFS client prints the three
      prerequisites and exits non-zero
- [ ] `Enable-WindowsOptionalFeature -Online -FeatureName ServicesForNFS-ClientOnly`
      (elevated) → reboot if asked
- [ ] `nfsadmin client localhost config protocol=tcp` (go-nfs is TCP-only)
- [ ] Registry `HKLM\SOFTWARE\Microsoft\ClientForNFS\CurrentVersion\Default`:
      `AnonymousUid` / `AnonymousGid` DWORDs set to the gateway's reported uid/gid; restart
      the "Client for NFS" service

## 2. Join and serve

- [ ] `tailraid.exe join <host> <token>` from PowerShell; `tailraid.exe status` on the Mac
      shows the Windows node as a live non-voter
- [ ] `tailraid.exe serve` in a console: RPC on `<tsIP>:20481`, Raft on `:20480`, NFS on
      `127.0.0.1:2049`, PMAP stub on `127.0.0.1:111` (TCP and UDP); no "port in use" (check
      nothing else owns 111/2049: `netstat -ano | findstr ":111 "`)
- [ ] Windows Firewall prompt on first Tailscale-IP bind → allow; from the Mac
      `curl http://<winIP>:20481/health` answers

## 3. Mount

- [ ] `tailraid.exe mount T:` runs `mount -o anon,nolock,mtype=soft \\127.0.0.1\tailraid T:`
      and `T:` appears in Explorer
- [ ] If `mount` fails with "network path not found": confirm the PMAP stub answers
      `GETPORT` for 100005/100003 (Wireshark on loopback or the stub's debug log); record
      whether Windows used TCP or UDP to query it
- [ ] Copy a folder (incl. files > 1 GiB and 10k small files) from the Mac via Finder and
      from Windows via Explorer; `diff -r` equivalents (`fc /b` or `certutil -hashfile`)
      match on both sides
- [ ] Write at offsets from PowerShell (`$f=[IO.File]::Open(...); $f.Seek(...)`) and verify
      the Mac reads the same bytes
- [ ] Rename, delete, `mkdir`, attribute change (read-only flag, timestamps) from Explorer
      behave; note which SETATTR fields Windows sends and whether any return NOTSUPP
- [ ] Windows' 4 GiB / 8.3-name / case-insensitivity quirks: create `A.txt` and `a.txt` from
      the Mac, observe what Explorer shows; document

## 4. Service mode

- [ ] `tailraid.exe install --print` — eyeball the `sc.exe` lines (`binPath … serve --config …
      --service`, `depend= Tailscale`, `start= delayed-auto`, failure actions); `install`
      also writes `log.file: %ProgramData%\tailraid\serve.log` into the config
- [ ] `tailraid.exe install` (elevated) → `sc.exe query tailraid` RUNNING
- [ ] **Service account**: the service runs as LocalSystem with home `%ProgramData%\tailraid`;
      confirm the config/keyring paths it reads are the ones `join` wrote, or document the
      copy step (errol's Windows pitfall)
- [ ] Reboot → service auto-starts after Tailscale; `T:` is mounted by the logon-time
      `mount` task (or document that the user runs `tailraid mount` after logon)
- [ ] Kill `tailraid.exe` in Task Manager → SCM restarts it within ~5 s; the mount survives
      (handles are stable, `mtype=soft`)
- [ ] `sc.exe stop tailraid` → clean stop; `T:` reports disconnected, no hang in Explorer
- [ ] Service logs: `tailraid.exe logs -f` follows `%ProgramData%\tailraid\serve.log` and
      shows the JSON/text lines of the running service (Windows services drop stderr;
      errol's pitfall)

## 5. Endurance

- [ ] Leave serving overnight as a storage node; `tailraid status` on the Mac shows it live
      the whole time (sleep settings!) and `check` reports no under-replicated chunks
      attributed to it
- [ ] Put the laptop to sleep for 15 min and wake: it rejoins, no repair was triggered
      before `fill_after`
- [ ] Copy a file larger than 512 MB onto `T:` while the uplink is throttled to ~5 MB/s:
      Explorer's copy slows down but completes without an I/O error (the gateway holds a
      WRITE for up to 20 s at a cap, docs/07; the Windows client's soft `Timeout` ×
      `Retransmissions` must exceed that, else raise them in the registry and note the
      values here) and the file's SHA-256 matches

When every box is ticked, remove the EXPERIMENTAL marker from `internal/mount` and
`cmd/tailraid/install.go`, and record the validation date and Windows build here.
