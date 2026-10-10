# Windows validation checklist (SMB mount)

The Windows mount path is **experimental**: the SMB gateway (docs/15) is tested in CI on
the Windows runner through a Go SMB client, but the Windows redirector has never mounted
it on real hardware. This is the manual pass that promotes it. Target: a Windows 11
**24H2 or newer** machine (build 26100+) on the tailnet; older builds cannot be told the
gateway's port and are refused by `tailraid mount`.

Every item is *to verify*. Record the Windows build, tailraid version and date when
ticking.

## 0. Build & stage

- [ ] `GOOS=windows GOARCH=amd64 make build` on the Mac (or `make release`), copy
      `tailraid.exe` to a stable path (e.g. `C:\tailraid\tailraid.exe`); the service
      registers this exact path
- [ ] Tailscale for Windows installed and connected; `tailscale status --json` works from
      PowerShell (tailraid shells out to `C:\Program Files\Tailscale\tailscale.exe`)

## 1. Prerequisites tailraid checks

- [ ] `winver` shows 24H2 or newer (build ≥ 26100); on an older build `tailraid.exe mount`
      refuses with the build number and the reason, and runs nothing
- [ ] `Get-SmbClientConfiguration | Select RequireSecuritySignature, EnableInsecureGuestLogons`
      shows signing required and guest logons off (the 24H2 defaults the gateway is built
      for: it signs every message and refuses guests)
- [ ] `netstat -ano | findstr ":20445 "` shows nothing before `serve`; `:445` is the
      Server service, which tailraid never touches

## 2. Join and serve

- [ ] `tailraid.exe join <host> <token>` from PowerShell writes `listen.smb: 127.0.0.1:20445`
      and a 24-character `smb.password` into `%ProgramData%\tailraid\config.yaml` and
      prints neither; `tailraid.exe status` on the Mac shows the Windows node as a live
      non-voter
- [ ] The config file's ACL: `icacls %ProgramData%\tailraid\config.yaml` — record who can
      read it; if Users can, tighten with
      `icacls … /inheritance:r /grant:r "SYSTEM:F" "Administrators:F" "%USERNAME%:F"` and
      note whether `join` should do that itself
- [ ] `tailraid.exe serve` in a console: RPC on `<tsIP>:20481`, Raft on `:20480`, the log
      line `smb=127.0.0.1:20445 nfs=off`, and a `mount with` line that holds no password
- [ ] Windows Firewall prompt on first Tailscale-IP bind → allow; from the Mac
      `curl http://<winIP>:20481/health` answers

## 3. Mount

- [ ] `tailraid.exe mount T:` runs `New-SmbMapping … -TcpPort 20445 -UserName tailraid`
      through PowerShell with the password in the child's environment, and `T:` appears
      in Explorer. (The `net use … *` form reads a password from the console only; the
      three-VM rig showed it failing with "System error 86" from any other session.) If
      PowerShell is unavailable, run the console form the command prints and record that
- [ ] If `net use` fails with "System error 53" or "67" (path not found): try
      `\\localhost\tailraid` and `\\<machine name>\tailraid` with the same `/TCPPORT`, and
      record which name the redirector accepts for a non-445 loopback target. The config
      and `mount.SMBCommand` then follow the answer
- [ ] Explorer shows each file's **Date modified** as the FSM holds it: write a file from
      the Mac, check the date on `T:`; copy a file with `robocopy /COPY:DAT` and check the
      timestamp survived (the in-tree server copy's timestamp change, docs/15 §2)
- [ ] Copy a folder (incl. files > 1 GiB and 10k small files) from the Mac via Finder and
      from Windows via Explorer; `fc /b` or `certutil -hashfile` match on both sides
- [ ] Write at offsets from PowerShell (`$f=[IO.File]::Open(...); $f.Seek(...)`) and verify
      the Mac reads the same bytes; the writes are 64 KiB multiples, which the upstream
      library refused before the credit fix (docs/15 §2)
- [ ] Rename, delete, `mkdir`, attribute change (read-only flag, timestamps) from Explorer
      behave; note which SET_INFO classes Windows sends and whether any return
      `STATUS_NOT_SUPPORTED`
- [ ] Case: create `A.txt` and `a.txt` from the Mac; record what Explorer shows and which
      file opens by each name (docs/15 §5). Open a folder of 10k entries and time it
      (Explorer probes `desktop.ini`, one listing per miss)
- [ ] Symlinks created from the Mac: record what Explorer shows (the server has no reparse
      ioctl; docs/15 §3)
- [ ] Stale mode (`tailscale down` on the leader for > 30 s): a new file from Explorer is
      "access denied"; saving an existing file succeeds; deleting a file reports success
      and the file is still there on the next listing (docs/15 §8)
- [ ] Full cluster (pin a tiny budget on every disk): a copy stops with "There is not
      enough space on T:", not an I/O error
- [ ] Trash (docs/15 §8): delete a file in Explorer; it is gone from `T:` at once and a
      new file of that name saves; the browser UI lists it dimmed in its folder and
      "Restore" brings it back on the next F5 with the same bytes (`certutil -hashfile`)
- [ ] Protected (docs/15 §8): mark a folder protected in the browser UI; deleting or
      moving a file out of it in Explorer says "Access denied" and the file stays;
      saving over an existing file in it says "Access denied"; a new file saves

## 4. Service mode

- [ ] `tailraid.exe install --print` — eyeball the `sc.exe` lines (`binPath … serve --config …
      --service`, `depend= Tailscale`, `start= delayed-auto`, failure actions); `install`
      also writes `log.file: %ProgramData%\tailraid\serve.log` into the config
- [ ] `tailraid.exe install` (elevated) → `sc.exe query tailraid` RUNNING
- [ ] **Service account**: the service runs as LocalSystem with home `%ProgramData%\tailraid`;
      confirm the config/keyring paths it reads are the ones `join` wrote, or document the
      copy step
- [ ] Reboot → service auto-starts after Tailscale; the mapping is `/PERSISTENT:NO`, so
      document a logon task that runs `tailraid mount T:` (or that the user runs it)
- [ ] Kill `tailraid.exe` in Task Manager → SCM restarts it within ~5 s; record whether
      Explorer reconnects on its own (the redirector re-establishes the tree connect and
      reopens by path) or needs F5
- [ ] `sc.exe stop tailraid` → clean stop; `T:` reports disconnected, no hang in Explorer
- [ ] Service logs: `tailraid.exe logs -f` follows `%ProgramData%\tailraid\serve.log` and
      shows the JSON/text lines of the running service (Windows services drop stderr)

## 5. Endurance

- [ ] Leave serving overnight as a diskless or storage node; `tailraid status` on the Mac
      shows it live the whole time (sleep settings!) and `check` reports no
      under-replicated chunks attributed to it
- [ ] Put the laptop to sleep for 15 min and wake: it rejoins, no repair was triggered
      before `fill_after`
- [ ] Copy a file larger than 512 MB onto `T:` while the uplink is throttled to ~5 MB/s:
      Explorer's copy slows down but completes without an error (the gateway holds a WRITE
      for up to 20 s at a cap, docs/15 §7; the SMB client's `SessTimeout`, 60 s by default,
      must exceed that, else `Set-SmbClientConfiguration -SessionTimeout` and note the
      value here) and the file's SHA-256 matches

When every box is ticked, remove the EXPERIMENTAL marker from `internal/mount` and
`cmd/tailraid/install.go`, and record the validation date and Windows build here.
