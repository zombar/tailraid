# 21 — Users, groups and permissions

A person is not a device. Until this document a device was the only identity tailraid
knew (its Tailscale `StableID`, docs/08): every mount on a device was "that device's user",
every process on a member device could run every admin operation, POSIX modes were
stored and never enforced, and audit logging was a non-goal. This document adds the
second layer: who a device's key belongs to. A user is a name with
public keys, one per device they use; groups are the roles; grants map a user or a group
to a folder with a role; every mutating operation is signed by a user's key and recorded;
the browser signs a person in from a link their own CLI mints. Nothing here has a
password.

*Status (2026-10-15): built and shipped in 0.10.0, on Tailscale. It was written on top of
0.9.x's own WireGuard mesh, which 0.10.0 withdrew (docs/00 §Transport); nothing here
depended on it. The plans were `~/.claude/plans/let-s-get-a-plan-concurrent-flame.md`
and `i-think-0-9-x-has-transient-nest.md`. Codec 14 wraps any lower codec, so a
cluster from before users upgrades in place: the leader refuses the users ops while a
live member reads less (I-349), and enforcement waits for the first user. The users
state is snapshot frame 15 (0.9.x's frames 13 and 14 are refused, docs/12 §6).*

**What the standards ask for, and what this is.** ISO/IEC 27001 certifies an
organisation's information security management system, not a program; Common Criteria
and FIPS 140-3 are the product-level schemes and are out of scope for a home and
small-team volume. What a program can do is implement the technical controls an operator
needs to meet the standards their organisation is assessed against, and say plainly which
it does not reach. §11 maps each control to its mechanism here.

## 1. Identities (I-342, I-343)

- **A user** is a row in the replicated state: `User{Idx, Name, State, Keys, CreatedAt,
  By}`. `Name` matches `^[a-z][a-z0-9._-]{0,31}$`; `State` is `Active`, `Disabled` or
  `Removed`; `Keys` are at most 16 `UserKey{Pub, Label, AddedAt, By}`, each an Ed25519
  public key (FIPS 186-5 approved; 128-bit strength, above the 112-bit floor of NIST SP
  800-63B) with a label (the device's host name by default). `By` is the user who made
  the row, 0 for the bootstrap. A removed row stays, so the audit log can name it; its
  name may be invited again and gets a new index. Bounds: 1024 users, 256 groups, 4096
  grants, 256 open invites, 16 keys per user.
- **The private key** lives only in the person's own OS keychain (`internal/keychain`
  with the account `user-<hex cluster ID>` and, where the OS has no keyring, a 0600 file
  under `$XDG_CONFIG_HOME/tailraid/keys/`), never in the daemon's keychain, the FSM, a
  snapshot, a log line, a report or a request. **One user, many devices**: the row is
  cluster-wide and each key is one device, so a person is the same user on every node
  they sit at. From a device that already holds a key, `tailraid user key invite` prints
  a one-time code; `tailraid user key add <code>` on the new device generates a fresh
  pair there and registers its public key (an admin may mint the code instead). Grants,
  groups and personal secrets attach to the user and follow to every device; the key, the
  uid binding (§5) and a browser session (§6) are per device. `user key rm` revokes one
  device and leaves the others working.
- **Invites.** An invite is a row `Invite{Hash, Name, Kind, ForUser, Admin, ExpiresAt,
  By}`: the SHA-256 of a 10-character base32 code (50 bits), the name it is for, whether
  it creates a user or adds a key to one, whether the new user joins `admins`, and a
  24-hour expiry the leader's tick enforces. `tailraid user invite <name> [--admin]`
  (admins) prints the code; `tailraid user create <name> <code>` on the person's device
  is the one unauthenticated mutating request, since the person has no key yet: the
  receiving node checks the code's hash against its replica first, rate-limited to one
  attempt per second per source address after a refused code, as the join is (I-48; an
  accepted code throttles nothing, so a scripted setup creates its people in a row), and
  proposes only a match;
  the FSM consumes the invite in the same apply as the user's creation, so two devices
  presenting one code are serialised by the log and exactly one succeeds (as `O_EXCL` is,
  I-222). A guessed code never reaches the log.
- **Bootstrap.** `tailraid init` mints an admin invite and prints its code beside the
  join token; it never generates a user key: the key is the person's, made by `tailraid
  user create <name> <code>`, run by that person on the same device. `tailraid user bootstrap`, run on
  a voter as the OS root and unsigned like `recover` (docs/12 §9), mints the same invite
  for a cluster upgraded from a release before users and for lockout recovery (every admin disabled or
  every admin key lost): local root on a member is already full trust (docs/08 non-goals).
- **Groups** are rows `Group{Idx, Name, Members}` with the user name pattern, created and
  changed by admins only. Two are built in: `admins`, whose members hold every right
  (§3) and alone manage users, groups, system secrets and the cluster (§2), and
  `everyone`, every Active user with no membership rows. The last Active member of
  `admins` can be neither disabled, removed nor dropped from the group (`ErrLockout`).
  In a grant command a group is written `@<name>`.

## 2. Signed requests (I-344, I-345)

- **Every mutating admin operation** (`POST /v1/admin` and `POST /v1/members` ops that
  change anything; `user create`, `user key add` and `user bootstrap` are the admin ops
  that need none, the code or the bootstrap token being their credential) carries a
  `Tailraid-Sig` header in the shape of RFC 9421 HTTP
  Message Signatures: `user=<name> key=<8-byte key ID> ts=<unix seconds> nonce=<16
  random bytes, hex> sig=<Ed25519 over cluster ID ‖ target node ID ‖ method ‖ path ‖
  SHA-256(body) ‖ ts ‖ nonce>`. The target node ID is in the signed bytes because the
  nonce cache is per node and a call goes to this node (`localCall`) or to the leader
  (`adminCall`): without it a captured signature would replay against every other member
  inside the skew window.
- **The node verifies**, before the body is applied: it is the named target; the user is
  `Active`; the key is one of the user's; `|ts − now| ≤ users.clock_skew` (5 min); the
  nonce has not been seen on this node within twice the skew (a bounded per-node LRU);
  the signature is valid. Anything else is `401` with no hint which part failed; an
  identity without the right is `403`. A hundred consecutive failures on one key from one
  source address disable that source for a minute (the rate limit NIST SP 800-63B asks of
  a verifier), counted in `status.users.refused`. Replay resistance comes from the window
  and the nonce cache, not from a verifier-issued challenge; a one-round challenge is the
  upgrade an assessor would ask for and is listed in §12. Read-only operations (`status`,
  `check` without a scrub, every `ls`) stay member-only as today (I-18); the dashboard's
  status is unchanged.
- **What each operation needs** is one table, here and in `rpc.AdminOp.Needs()`, tested
  exhaustively against the dispatcher's switch so an op cannot be added unclassified:

  | Needs | Operations |
  |---|---|
  | nothing beyond membership | `status`, `check` (no scrub), `node ls`, `volume describe`, `chunk describe`, `trash ls`, `cache ls`, `mirror ls`, `snapshot ls`, `versions <path>` (listing), `search`, `audit ls` (own records), `access ls` |
  | a user | `secret set\|get\|ls\|rm` of one's own, `web login`, `web logout`, `user key invite` for oneself, `user key rm` of one's own key, `user whoami`, `user bind\|unbind` of one's own credential on this device |
  | `Admin` on the inode | `grant`, `revoke`, `access ls <path>` under a subtree; `snapshot restore` and `versions --revert\|--copy` into it; `cache pin\|unpin` under it; `trash restore\|purge` of entries under it |
  | `admins` | `user invite\|disable\|enable\|rm`, `user key invite\|rm` for another, `group *`, `secret * --system`, `audit ls` of everyone, `node promote\|demote\|drain\|remove\|add-disk\|add-s3`, `volume grow\|set-redundancy`, `token rotate`, `repair`, `gc`, `check --scrub`, `rebalance`, `mirror add\|remove`, `snapshot create`, `trash purge --all`, `media scrape`, `stats seal`, `search index` |

  A path-scoped operation resolves its path on the node's replica *before* the check
  (resolve, check, propose); a path that does not exist answers `403` exactly as a
  missing right does, so the namespace cannot be probed by a user who holds nothing
  there.
- **The CLI signs** with the key of the OS user running it (`--as <name>` picks one when
  the keychain holds several identities of this cluster). The daemon's own callers
  (`update --restart`'s idle wait, the service) use read-only operations only; nothing in
  `serve` signs anything.

## 3. Grants and the access rule (I-346, I-347, I-348)

- **A grant** is `Grant{Ino, Subject{Kind, Idx}, Role, At, By}`, keyed by `(Ino,
  Subject)`: a user or a group, a role, on an inode. `tailraid grant <path> <user|@group>
  read|write|admin` sets one, `tailraid revoke <path> <user|@group>` clears it. Grants
  are allow-only, inherited down the tree and monotonic: the **effective role** of user
  *u* at inode *i* is the highest role among the grants on *i* and its ancestors whose
  subject is *u*, one of *u*'s groups, or `everyone`; a member of `admins` holds `Admin`
  everywhere. There are no deny entries and no entry order: that is where the NFSv4 ACL
  model's complexity lives, and the POSIX-draft ACLs stayed allow-only for the same
  reason (§12).
- **Roles.** `Read`: look up, stat, read, list, search hits, thumbnails, the media
  catalogue's entries, the versions listing. `Write`: Read plus create, write, rename
  within the subtree, trash and restore of entries under it, the folder flags
  (`protected`, `nsfw`, the library kind). `Admin`: Write plus grant and revoke under the
  subtree (delegated administration), revert and restore from snapshots into it, pins. A
  rename across subtrees needs Write on both sides. Traversal is implicit: a user may look
  up and stat the ancestors of any inode they hold a role on, and a listing of a
  directory where they hold no role shows exactly the children under which they hold one.
  A trash entry evaluates on its original parent; `/.snapshots/<mirror>/<name>/…` evaluates
  the live grants of the same inode (an inode no longer in the volume: admins only), a
  snapshot directory lists what the user may reach as a live one does, a revert needs
  Read on the snapshot entry and `Admin` on the live file, and a restore from a snapshot
  needs `Admin` on the folder it lands in and copies the entries the user may read,
  reporting the rest as refused.
- **`/.tailraid/`** (thumbnails, stats, the search index, the audit log) is outside the
  grant walk for every user, admins included: only `System` commands write under it (§4)
  and only the node's own code reads it; the API reaches a thumbnail, a chart, a hit or a
  catalogue entry on a user's behalf after checking the source file's role.
- **`meta.Access(st, user, ino) Role`** is a pure function of the state: the same answer
  on every node at the same index, with no clock and no liveness in it. It walks the
  lineage once (as `lineage(ino)` does for the trash and the protected flag) with the
  user's group set computed once per request. The listing filter is not a lineage walk
  per child: a request builds once the set of ancestors of every inode the user holds a
  grant on (at most 4096 walks, cached on the gateway per FSM index) and keeps the
  children in that set.

## 4. Enforcement in the FSM (I-349)

- **Every command carries its actor.** `Command` gains `Actor UserIdx` and `System bool`
  at codec 14 (`meta.CodecUsers`, snapshot frame 15, ops 28–32: `Invite`, `User`,
  `Group`, `Grant`, `Session`; docs/02 §Codec versions). `CommandCodec` answers 14 when either is
  set. The FSM refuses the user-facing operations (`Create`, `Mkdir`, `Unlink`, `Rmdir`,
  `Rename`, `SetAttr`, `Trash`, `Restore`, `Purge`, and `Commit`)
  with `ErrAccess` (a new wire code, the list is append-only) unless `Access(actor,
  target) ≥` the role the operation needs on every inode it touches: a `Rename` and a
  `Restore` name two parents (the one an entry leaves, the one it lands in), both
  `Write`; a `Purge` of one entry needs `Admin` on its parent and the whole-trash sweep
  `Admin` at the root, which the retention sweep holds as `System` and an admin as an
  admin; a target the lookup did not find holds no role for anyone, so the refusal says
  nothing about what is missing. This is where the `protected` flag is enforced
  today (docs/13, I-154): the check runs once, in apply, so every mount and the browser
  agree.
- **System commands.** `System` is set by the node's own writers and is allowed only for
  the operations in this table; a `System` command on any other target is refused.

  | Writer | Operation |
  |---|---|
  | the trash sweep | `Purge` of an entry past `trash.retention` |
  | the flusher, for data written before the first user existed | `Commit` of any inode (its bytes alone change) |
  | the thumbnail writer, and the browser's `PUT /api/thumb` after the handler has checked `Read` on the source file | `Create`, `Commit` under `/.tailraid/thumbs` |
  | the stats sampler, the search indexer and catalogue, the audit writer | `Create`, `Commit`, `Unlink` under `/.tailraid/stats`, `/.tailraid/index`, `/.tailraid/audit` |
  | the scrape | nothing: it runs as the admin who started it and writes sidecars under their actor |

  A web upload's temp file is created under the user's actor beside its target (docs/13
  §4) and published under it; nothing of a user's lands as `System`.
- **The flusher's `Commit`** carries the actor who wrote: the WAL record gains the actor
  per inode, so a flush after a gateway restart still names them, and the FSM checks
  every `Commit` for `Write` on its inode. A grant revoked, or a user disabled, between
  the write and the flush ends that data: the FSM refuses the `Commit`, the gateway drops
  the inode's dirty bytes, retries nothing and answers `EACCES` to the flush that asked,
  and `status` counts it in `gateway.commits_refused`. Data written before the first user
  existed carries no actor and commits as `System`, which a `Commit` may be anywhere: it
  changes only its inode's bytes, and nobody held a role when it was written.
- **When it is on.** Enforcement is active only while the user table is non-empty and the
  prober's `MinCodec` is 14 or more; before either, a cluster behaves exactly as it did
  before this document, which is how every codec feature gates (docs/02). The moment the
  first user is created, every unbound credential on every device becomes nobody (§5), so
  docs/12 §12 states the order: create the first admin, then bind a user on each device,
  and `check` warns "device has no bindings" on every device until then. A 13-reader
  refuses a codec-14 entry as it refuses any newer one (I-105).
- **Trust boundary.** The FSM trusts the actor a member asserts: a compromised member
  can impersonate any user. That is the existing non-goal (docs/08: an attacker on a
  member device reads every byte already). Signing each command with the user's key would
  close it and would need the key on the daemon's side of every gateway write, which the
  design keeps in the person's keychain; it is listed in §12.

## 5. The gateways: who is the actor of a mount operation (I-350, I-351, I-352, I-353)

- **Bindings.** A device keeps a map of local credentials to users in
  `<data_dir>/bindings.json` (0600): `uid:<n>` for NFS (the AUTH_SYS uid of the calling
  process) and `smb:<account>` for SMB (the session's account). `tailraid user create`
  and `user key add` bind the unprivileged caller's own uid (never `SUDO_UID`) and, on
  Windows, the `tailraid` SMB account, to the user; `tailraid user bind [--uid n | --smb
  account] <name>` and `unbind` edit the map through signed operations on that node (a
  user binds their own credential, an admin anyone's). An unbound credential is nobody:
  every operation is refused (`NFS3ERR_ACCES`, `STATUS_ACCESS_DENIED`) and the root
  listing is empty. This replaces docs/08's "protecting the local mount from other users
  of the same device" non-goal with a rule.
- **NFS.** `Mount` advertises `AuthFlavorUnix`. The in-tree go-nfs (`third_party/nfs`)
  changes in three places: the AUTH_SYS credential of every call (`rpc.Header.Cred`, a
  raw `{Flavor, Body}`; the body is XDR `{stamp, machine, uid, gid, gids}`) is decoded
  once per call; `Handler.FromHandle` gains a context carrying it; and the duplicate
  request cache is keyed on the credential and the xid, not the xid alone, so two uids
  replaying one xid cannot share a reply. tailraid's handler maps the uid through the
  bindings and answers `vfs.FS.As(actor)`. The macOS and Linux mounts change nothing
  (AUTH_SYS is the client default). The mode bits a client sees derive from the
  effective role (the stored mode masked to 0500 for Read, 0700 for Write and Admin,
  owner `mount.owner`), so Finder and `ls -l` show a read-only tree as read-only; a
  SETATTR of mode or owner is stored as today and never consulted (docs/07 §Known POSIX
  gaps).
- **SMB.** `third_party/smb` hands one filesystem to every connection, so it gains a hook
  at tree connect (where the session's account already decides read-only): the share
  asks the adapter for the filesystem of that account and `smbgw` answers `vfs.FS.As`
  of the bound user. `tailraid` stays the mount account (docs/15, I-110); a second Windows
  user binds another account with `user bind --smb <account>`, which also adds it to the
  server with the share password.
- **`vfs.FS.As(actor)`** is a view (as `Chroot` is) that stamps the actor on every
  proposal, checks `Read` on open, stat and list against the local replica, filters
  listings (§3), and keeps the actor on each open `File` for its `Commit`. The read-side
  check is a convenience and a fast refusal; the authoritative check for every mutation
  stays in the FSM (§4). A `vfs.FS` with no actor (the node's own services) is the
  `System` writer and reaches `/.tailraid` only.
- **The browser** is a gateway too: the session's user (§6) is the actor of every `/api`
  and `/files` request, every listing route filters by role and every mutation carries
  the actor.

## 6. Signing in to the browser (I-354, I-355)

- **A person signs in to the volume, not to a node.** `tailraid web login`, a signed user
  operation, proposes a session row `Session{Hash, User, Key, ExpiresAt, By}` (op 32:
  the SHA-256 of 32 random bytes, the user, the key ID that minted it, `users.session`
  (168 h) from now; at most 16 open sessions per user, the oldest replaced) and prints
  `http://<this device's Tailscale IP>:20482/login?code=…`, a one-time code good for five
  minutes that carries the session token to the browser sealed with AES-256-GCM under a
  key derived from the cluster key (a code copied from a browser's history or a proxy's
  log yields no token to anyone without it); `--node <name>` only chooses
  which member's address the link points at (a phone that reaches one address and not
  another). Opening the link sets a cookie (`HttpOnly`, `SameSite=Lax`: a hand-off is a
  cross-origin navigation, for which a Strict cookie is withheld) holding the
  token on that origin, and every node validates a token against its replica: the
  session is cluster-wide, survives any node's restart, and `tailraid web logout
  [--all]` removes the row, which ends it everywhere. Revocation needs no hook: a
  request re-reads the session's user row, so a disabled user or a removed key is refused
  at its next request on every node.
- **Across nodes.** A browser cookie belongs to one origin and each node's page is its
  own origin (`<its address>:20482`), so a link from one node's page to another's (the
  member menu of the Stats page, docs/17)
  carries a hand-off: the page asks its node for a one-time code minted from the
  session (`GET /api/handoff?to=…`, five minutes, single use) and the target's `/login`
  sets its own cookie for the same session and continues to the page asked for. A person
  signs in once, on any node, and follows links between nodes without signing in again.
- **The sign-in page** (`/login` and `/assets/*` are the only routes without a session;
  a member device's address alone no longer admits a browser, docs/13 §2 and I-68) shows
  the one command to run and nothing else; the header gains the account chip (the name,
  a sign-out button) on every page. Management stays in the CLI: the browser lists what
  the session's user may see and changes what they may change, and no page manages
  users, groups or grants (docs/13 §9). The lookdev is `assets/lookdev/signin.html` and
  the chip in `components.html` and `dashboard.html`.
- **Phishing resistance** (NIST SP 800-63B §3.2.5) is not claimed: the code travels in a
  URL the person pastes; the tailnet is the transport boundary.

## 7. Secrets (I-356; docs/19 amended)

- **Two name spaces.** `tailraid secret set <name>` run by user *u* writes
  `user/<u>/<name>`; `secret ls` lists the caller's own; `secret rm <name>` removes one of
  their own. `--system` (admins) addresses the unprefixed space that holds
  `s3/<node>/access_key`, `s3/<node>/secret_key` and `tmdb` today (docs/19 §6). The FSM
  refuses a `user/<x>/…` put whose actor is not *x* and a delete whose actor is neither
  *x* nor an admin.
- **Reading.** `tailraid secret get <name>` prints the caller's own personal secret: a
  signed user operation the node answers for the owner only, admins included (an admin
  reads their own, never another user's), and every read is an audit record (§8). A
  system value is never printed: there is no `secret get --system`, and admins `ls
  --system` (name, set at, by which user). The two consumers of a system value, `node
  add-s3` and `media scrape`, run as an admin: their signed `secret_get` is answered for
  an admin only, the value is used in the CLI's process (the bucket check, the TMDB call)
  and never written out, and the read is an audit record. The admin route's `secret_get`
  answers a personal name to its owner and a system name to an admin, and nothing else.
- **Rows.** `Secret.By NodeIdx` keeps its meaning for rows written before codec 14;
  `ByUser UserIdx` is added at frame 15. Bounds: 256 system secrets and 64 per user. The
  cluster key, the per-name HKDF and AES-256-GCM, the key fetch and soak phase 24 are
  untouched (`Store.Opened(prefix)` already filters by prefix).

## 8. The audit log (I-357, I-358, I-333)

- **Records.** Every applied op 28–32, every secret put, delete and personal read, every
  operation that needed `admins`, every session mint and sign-in, every key added or
  removed, and every refusal (`401`, `403`, `ErrAccess`, `NFS3ERR_ACCES`,
  `STATUS_ACCESS_DENIED`; refusals rate-limited to one record per actor per minute with a
  count) becomes `{at, index, node, actor, key, kind, target, result, detail}`. Applied
  operations are recorded by the leader only, from its apply observer, so there is one
  copy; refusals by the node that refused.
- **Storage.** A small writer in `internal/audit` borrows the Stats page's segment
  format (docs/17: gzip JSON lines, one segment per node per hour, retention by the
  leader) under `/.tailraid/audit/<node>/<hour>.jsonl.gz`. Each record carries `prev`,
  the SHA-256 of the previous record in its segment: tamper evidence within a segment,
  which with the tree outside every user's grant walk (§3) is what the log offers. A
  segment is dropped after `audit.retention` (365 d).
- **Never in the leader's way.** The leader records applied operations from the FSM's
  apply observer, which runs on Raft's apply goroutine; a write into the volume is
  proposals that goroutine must apply. So `Record` only appends to an in-memory inbox
  under a lock held for no I/O, and the flush (every 30 s, and the seal at a stop)
  places the inbox, reads the open hour's segment and writes it with that lock released.
  A record made while a segment is being written waits for nothing and lands in the next
  flush; a record whose hour differs from the open segment's closes that hour in the
  flush, never in `Record` (I-333; the 2026-10-14 soak's leader stopped applying when a
  `secret set` was applied during the flush's write).
- **Reading.** `tailraid audit ls [--since 7d] [--user u] [--kind k] [--json]` merges
  the segments newest first; a user sees their own records, admins everyone's.
  `status.users` carries the counts (users, active sessions on this node, refusals in the
  last hour) and `check` warns for every user with no sign-in and no signed operation in
  `users.dormant_after` (90 d), which it never disables. `tailraid access ls [path]` is
  the access review: every grant with its subject, role, who granted it and when.

## 9. Commands (docs/12 §12)

```
tailraid user invite <name> [--admin]            # admins: a one-time code, 24 h
tailraid user create <name> <code>               # on the person's own device: key → their keychain, row registered, uid bound
tailraid user ls [<name>] | whoami               # ls <name>: keys, groups, grants
tailraid user key invite [<name>]                # oneself (or admins): a code for another device
tailraid user key add <code>                     # on the new device
tailraid user key rm <name> <key-id>
tailraid user disable|enable|rm <name>           # rm keeps the row as Removed
tailraid user bind [--uid n | --smb account] <name> | unbind …   # this device's bindings
tailraid user bootstrap                          # root on a voter, unsigned: an admin invite (first admin, lockout)
tailraid group create|rm <group> | add|remove <group> <user> | ls
tailraid grant <path> <user|@group> read|write|admin
tailraid revoke <path> <user|@group>
tailraid access ls [path]                        # the access review
tailraid secret set <name> [--system] | get <name> | ls [--system] | rm <name> [--system] | rm --all
tailraid audit ls [--since d] [--user u] [--kind k]
tailraid web login [--node n] | logout [--all]   # a session of the volume; --node picks the address the link points at
```

Config (I-359): `users.clock_skew` (5m; 1m..1h), `users.session` (168h; 1h..720h),
`users.dormant_after` (2160h; 24h..8760h), `audit.retention` (8760h; 168h..87600h), each
refused outside its bounds by `Validate` (I-126).

## 10. Soak phases 28–31 (docs/09 §6)

- **28 users and keys.** An admin invite minted on one node creates a user on another;
  the user adds a key on a third node and runs a signed operation from both; a mount on
  each bound device acts as the user; one key removed, the other keeps working; a
  disabled user is refused at once on every node; dropping the last admin is refused
  (`ErrLockout`); a removed name is invited again and gets a new index; an expired and a
  used code are refused and leave no row.
- **29 grants on the gateways.** Read, Write, Admin and nobody on NFS, on SMB where the
  rig has it, and through the web: a listing filtered to what the user holds, a write
  refused under a read grant with the OS error, a revoke taking effect on the next
  operation mid-copy, two gateways answering the same role for the same user (the N-node
  determinism of `Access`), `/.tailraid` unreachable from every mount.
- **30 secrets per user.** `set`, `get`, `ls` and `rm` of one's own through two nodes;
  another user's name refused; an admin's `get` of another user's name refused;
  `--system` refused for a non-admin and listed for an admin; `node add-s3` and the scrape
  still reach their system values in process; the personal secret removed at the end,
  whatever happened.
- **31 audit and sessions.** Every operation of phases 28–30 has one record with its
  actor; refusals are counted and rate-limited; `web login` on one node signs the
  harness's browser in, the same session is honoured by every other node's page through
  the hand-off, a page lists only the user's folders, `logout` on any node ends the
  session everywhere, a removed key ends it at the next request; the chain hash of a segment verifies; retention drops a
  segment older than `audit.retention` on a fake clock.

The report gains a Users section: the round-trip table of the invite, the grant matrix
per gateway, and the audit counts.

## 11. Standards mapping

| Control | Mechanism here |
|---|---|
| ISO/IEC 27002:2022 5.15 access control, 8.3 information access restriction | grants on folders with three roles, deny by default, enforced once in the FSM for every gateway (§3, §4) |
| 5.16 identity management | one row per person with a lifecycle (invite, create, disable, remove), keys per device, no shared accounts (§1) |
| 5.17 authentication information | no passwords; a private key in the person's own keychain, never transmitted; a one-time code for enrolment; system secrets never printed (§1, §7) |
| 5.18 access rights, SOC 2 CC6.2 and CC6.3, CIS 6.1 and 6.2 | an admin authorises before credentials exist (the invite); `grant`/`revoke` with the grantor recorded; `access ls` as the periodic review (§1, §3, §8) |
| 5.3 segregation of duties, 8.2 privileged access rights, CIS 5.4 | `admins` is the one privileged group, its membership is admins-only, delegated `Admin` is per subtree, and every privileged operation names its actor (§2) |
| 8.5 secure authentication, NIST SP 800-63B, IA-2, IA-5 | Ed25519 single-factor cryptographic software authenticator, ≥ 112-bit strength, signed requests with a timestamp and nonce, 100-failure lockout per key and source, immediate revocation (`user key rm`, `user disable`) (§2) |
| 8.15 logging, 8.16 monitoring, NIST AU-2, AU-3, AU-9, AU-11 | the audit log with actor, time, target and result, refusals included, hash-chained segments under a tree no user can write, retention, `check` warnings (§8) |
| NIST AC-2 account management, CIS 5.1, 5.3 | `user ls` as the inventory, dormant users warned about and never silently disabled (§8) |
| NIST AC-3, AC-6 least privilege | nothing is granted by default; a user who holds nothing sees an empty root (§3, §5) |
| INCITS 359 (NIST RBAC) core | users, roles as groups, permissions as `(inode, role)`; no role hierarchy, no sessions with role activation (§12) |
| 8.1 user endpoint devices, 8.24 cryptography, NIST SC-28 | docs/08 §Files on disk: 0700 directories, ownership and full-disk encryption checked and warned about (I-340, I-341) |

What it does not reach, said plainly: AAL2 and above need a second factor; Go's
cryptography is not a FIPS 140-3 validated module; the FSM trusts a member's assertion
of the actor (§4); the browser's code-in-a-link is not phishing resistant (§6); and an
organisation's ISMS, its policies and its reviews are the operator's, not the program's.

## 12. Non-goals

Deny entries and ACL ordering; role hierarchies and separation-of-duty constraints beyond
the `admins`/everyone split; quotas; user-signed Raft commands (§4); one cookie for every
node (each node's page is its own origin, §6); passkeys and WebAuthn (a later sign-in method the session layer would carry);
a second factor; LDAP or OIDC federation; enforcing POSIX modes (they stay stored, docs/07);
per-user S3 gateway keys (the gateway is deferred, docs/10); a verifier-issued challenge in place of the timestamp and nonce
(§2); a page that manages users, groups or grants (docs/13 §9).

## Invariants

- **I-342** — A user is a row with a name matching `^[a-z][a-z0-9._-]{0,31}$`, a state
  of Active, Disabled or Removed, at most 16 Ed25519 public keys each with a label and
  its adder, and the user who created it; a removed row stays and its name may be
  invited again under a new index; the cluster holds at most 1024 users, 256 groups,
  4096 grants and 256 open invites; a private key is never in the FSM, a snapshot, a log
  line, a report or any request.
- **I-343** — An invite is the SHA-256 of a 10-character base32 code with a name, a kind
  (user or key), an admin flag, its minter and a 24-hour expiry the leader's tick
  enforces; `user create` and `user key add` are accepted only with a code whose hash the
  receiving node's replica holds, one attempt per second per source address after a
  refused code, and the FSM
  consumes the invite in the same apply as the row it creates, so of two devices
  presenting one code exactly one succeeds and an expired or used code leaves no row;
  `init` and `user bootstrap` mint an admin invite and never a key.
- **I-344** — Every mutating admin and member operation carries a `Tailraid-Sig`
  header signed over the cluster ID, the target node ID, the method, the
  path, the body's SHA-256, a timestamp and a 16-byte nonce; the node applies nothing
  unless it is the named target, the user is Active, the key is the user's, the
  timestamp is within `users.clock_skew` of its clock, the nonce is unseen on this node
  within twice the skew and the signature verifies, answering `401` otherwise with no
  hint which part failed; a hundred consecutive failures on one key from one source
  disable that source for a minute; read-only operations stay member-only (I-18).
- **I-345** — Every admin operation is classified in one table as needing nothing,
  a user, `Admin` on the inode its path names, or `admins`; an operation the table does
  not name is refused; a path-scoped operation resolves its path before the check and a
  missing path answers `403` exactly as a missing right does.
- **I-346** — `admins` and `everyone` are built in; group creation, removal and
  membership changes need `admins`; the last Active member of `admins` can be neither
  disabled, removed nor dropped from the group (`ErrLockout`).
- **I-347** — `meta.Access(st, user, ino)` is a pure function of the state: the highest
  role among the grants on the inode and its ancestors whose subject is the user, one of
  the user's groups or `everyone`, and `Admin` for a member of `admins`; it reads no
  clock and no liveness, so every node answers the same at the same index.
- **I-348** — `Read` allows look-up, stat, read, list, search hits, thumbnails, catalogue
  entries and the versions listing; `Write` adds create, write, rename within the
  subtree, trash, restore and the folder flags; `Admin` adds grant and revoke under the
  subtree, revert and restore into it, and pins; a rename across subtrees needs `Write`
  on both; a user may look up and stat the ancestors of any inode they hold a role on,
  and a listing of a directory where they hold none shows exactly the children under
  which they hold one, a snapshot directory likewise; a revert needs Read on the snapshot entry and `Admin` on the live file, a restore from a snapshot `Admin` where it lands and copies only what the user may read; a trash entry evaluates on its original parent, a snapshot path on
  the live inode (admins only once it is gone); `/.tailraid/` is outside the grant walk
  for every user.
- **I-349** — `Command` carries `Actor` and `System` at codec 14; while the user table is
  non-empty and `MinCodec ≥ 14` the FSM refuses `Create`, `Mkdir`, `Unlink`, `Rmdir`,
  `Rename`, `SetAttr`, `Trash`, `Restore`, `Purge` and every `Commit` with
  `ErrAccess` unless the actor's effective role covers the operation on every inode it
  touches (both parents of a rename or a restore; the root, for the whole-trash sweep,
  which only `System` and admins hold), and refuses a
  `System` command outside the System table (the retention sweep's `Purge`, a `Commit`
  anywhere, the writers under `/.tailraid`); the flusher's `Commit` carries the actor
  the WAL recorded for the write, and a `Commit` the FSM refuses drops that inode's
  dirty data at the gateway, which answers `EACCES` and retries nothing; before the
  first user, or while a member reads a codec older than 14, a cluster behaves as before, and a
  reader of an older codec refuses a codec-14 entry.
- **I-350** — A device maps `uid:<n>` and `smb:<account>` to users in
  `<data_dir>/bindings.json` (0600), changed only by signed operations on that node (a
  user for their own credential, an admin for any); `user create` and `user key add`
  bind the unprivileged caller's own uid, never `SUDO_UID`; an unbound credential is
  nobody: every operation is refused with `NFS3ERR_ACCES` or `STATUS_ACCESS_DENIED` and
  the root listing is empty.
- **I-351** — The NFS gateway advertises `AuthFlavorUnix`, decodes the AUTH_SYS
  credential of every call, resolves the actor in `FromHandle` through the bindings and
  keys its duplicate request cache on the credential and the xid; the mode bits it
  reports are the stored mode masked to 0500 for `Read` and 0700 above, its ACCESS answer
  is the mask the role allows (every bit under `Write`, READ, LOOKUP and EXECUTE under
  `Read` and on a directory on the way to a role, whose listing is the filtered one of
  I-348 and which a client never lists once ACCESS denied READ, `NFS3ERR_ACCES`
  elsewhere), and a
  SETATTR of mode or owner is stored and never consulted.
- **I-352** — The SMB gateway binds each tree connect to the filesystem of the user its
  session account maps to; `tailraid` stays the mount account; an unbound account's
  operations are `STATUS_ACCESS_DENIED` and its listing of the share is empty; an account
  holding `Read` alone opens a file for reading and its WRITE and size changes are
  `STATUS_ACCESS_DENIED`, with no refusal recorded for the open.
- **I-353** — `vfs.FS.As(actor)` stamps the actor on every proposal, refuses open, stat,
  readlink and list without `Read` on the local replica before it tells anything else of
  the entry (`EEXIST`, `EISDIR`, `ELOOP`, a folded name), filters listings as I-348 says,
  and keeps the actor on each open file for its `Commit`; a `vfs.FS` with no actor is the
  `System` writer and reaches `/.tailraid` only.
- **I-354** — `web login` proposes a session row (the token's SHA-256, the user, the
  minting key, an expiry `users.session` away; at most 16 per user) and prints a
  one-time five-minute code that carries the token sealed under the cluster key, so the
  code alone yields no token; opening the link on any node's
  `/login` sets an `HttpOnly`, `SameSite=Lax` cookie for that origin, every node
  validates a token against its replica, and a link between two nodes' pages carries a
  one-time hand-off code that signs the second origin into the same session; `/login` and
  `/assets/*` are the only routes without a session and a member's address alone admits
  nothing; every request re-reads the session's user row, so a disabled user, a removed
  key or a removed session is refused at its next request on every node; `web logout`
  removes the row, `--all` every row of the user.
- **I-355** — Every web route answers only what the session's user holds `Read` on and
  every mutation carries the user as its actor; `PUT /api/thumb` is proposed as `System`
  only after `Read` on the source file.
- **I-356** — `secret set <name>` writes `user/<caller>/<name>` and `--system` the
  unprefixed space for admins only; the FSM refuses a personal put by another actor and a
  personal delete by anyone but the owner or an admin; `secret_get` answers a personal
  name to its owner only and a system name to an admin only, each read recorded in the
  audit log; no command prints a system value (`node add-s3` and the scrape use theirs
  in process); `secret ls` lists the caller's own names and `--system` the system names
  to an admin; a secret records the user who set it; at most 256 system secrets and 64
  per user.
- **I-357** — Every applied op 28–32 (a session's end included), secret put, delete and personal read, operation
  that needed `admins`, session mint and sign-in, key addition and removal, and every
  refusal (one record per actor per minute with a count) is an audit record with its
  time, log index, node, actor, key, kind, target, result and detail; applied operations
  are recorded by the leader only.
- **I-358** — Audit records are gzip JSON-line segments under
  `/.tailraid/audit/<node>/<hour>.jsonl.gz`, each record carrying the SHA-256 of the
  previous one in its segment; the leader drops a segment older than `audit.retention`;
  `audit ls` merges them newest first with its filters, a user seeing their own and an
  admin everyone's; `status.users` counts users, sessions and refusals; `check` warns for
  a user dormant longer than `users.dormant_after` and disables no one; `access ls` lists
  every grant with subject, role, grantor and time.
- **I-359** — `users.clock_skew`, `users.session`, `users.dormant_after` and
  `audit.retention` are validated within their bounds (1m..1h, 1h..720h, 24h..8760h,
  168h..87600h) and default to 5m, 168h, 2160h and 8760h.
- **I-333** — `audit.Writer.Record` and `Refuse` touch no volume and wait for no flush:
  the record goes into an in-memory inbox under a lock held for no I/O, and `Flush`
  places the inbox, reads and writes the segments and the seen file with that lock
  released, so the leader's apply observer never blocks Raft's apply goroutine on a
  proposal of the writer's own; a record made during a segment write lands in the next
  flush, and a closed hour is written by the flush alone.

## Tests

Planned names are written without backticks until the tests exist; each ID is cited by a
stub in `test/invariants/` meanwhile.

| Invariant | Tests |
|---|---|
| I-342 | `TestUsers_Apply_Table`, `TestUsers_Bounds_Table`, `TestUsers_RemovedNameReinvited`, `TestUsers_NoPrivateKeyInState` |
| I-343 | `TestInvite_ConsumedOnce`, `TestInvite_ExpiredAndUsedRefused`, `TestUsers_Apply_Table`, `TestInvite_RateLimitedPerSource`, `TestCLI_Init_PrintsAdminInvite`, `TestCLI_UserBootstrap`, `TestCLI_UsersFlow`, `TestIdentity_Keychain` |
| I-344 | `TestSig_Vectors`, `TestSig_SkewAndReplay_Table`, `TestSig_TargetNodeBound`, `TestSig_FailureLockout`, `TestRPC_MutatingOpsNeedSignature`, `TestCLI_UsersFlow` |
| I-345 | `TestAdminOp_Needs_Exhaustive`, `TestAdminOp_Needs_Refinements`, `TestAdminOp_PathResolvesBeforeCheck`, `TestRPC_MutatingOpsNeedSignature` |
| I-346 | `TestGroups_Builtin`, `TestGroups_LastAdminLockout` |
| I-347 | `TestAccess_Table`, `TestAccess_NNodeDeterminism`, `TestRole_Words` |
| I-348 | `TestAccess_Roles_Table`, `TestAccess_TrashEvaluatesOnOriginalParent`, `TestAccess_TailraidTreeExcluded`, `TestAccess_ListingFilter`, `TestAccess_SnapshotPaths`, `TestAccess_SnapshotRestoreByRole` |
| I-349 | `TestCluster_Codec14RefusedWhileMemberOld`, `TestDecodeState_MeshFramesRefused`, `TestFSM_ActorEnforced_Table`, `TestFlusher_RefusedCommitDropsDirtyData`, `TestFlusher_PreUsersDirtyDataCommitsAsSystem`, `TestAccess_Roles_Table`, `TestAccess_TailraidTreeExcluded`, `TestCodec14_OlderReaderRefuses`, `TestUsersState_Snapshot`, `TestNFS_UnboundIsNobody`, the users steps of `TestModelCheck_Long` |
| I-350 | `TestBindings_File`, `TestBindings_OwnUidNeverSudo`, `TestCLI_UsersFlow`, `TestNFS_AuthSysActor`, `TestNFS_UnboundIsNobody`, `TestSMB_UnboundDenied` |
| I-351 | `TestNFS_AuthSysActor`, `TestNFS_UnboundIsNobody`, `TestNFS_ModeFromRole`, `TestNFS_AccessFollowsRole`, `TestDRC_KeyedOnCredential` (in `third_party/nfs`) |
| I-352 | `TestSMB_TreeConnectActor`, `TestSMB_UnboundDenied`, `TestSMB_ReadOnlyUserReads` |
| I-353 | `TestVFS_As_StampsActor`, `TestVFS_As_ReadCheck`, `TestVFS_As_RoleBeforeExistence`, `TestWAL_RecordsActor`, `FuzzWALReplay` |
| I-354 | `TestSessions_Apply` (the FSM's rows), `TestCodes_OnceWithinLife`, `TestCodes_TokenNotRecoverableFromCode`, `TestWebUI_Login_CodeOnce`, `TestWebUI_Session_Lifecycle_Table`, `TestWebUI_SessionOnEveryNode` (the hand-off between two nodes' pages), `TestWebUI_MemberAddressAlone_Refused`, `TestWebUI_RevokedAtNextRequest`, `TestCLI_WebLogin` |
| I-355 | `TestWebUI_RoutesFilterByRole`, `TestWebUI_AdminRoutes_ByRole`, `TestWebUI_Upload_RoleBeforeExistence`, `TestWebUI_ThumbPut_SystemAfterRead`, `TestWebUI_RevokedAtNextRequest` |
| I-356 | `TestSecrets_PersonalNamespace`, `TestSecrets_GetOwnerOnly`, `TestSecrets_SystemNeverPrinted`, `TestCLI_UsersFlow` |
| I-357 | `TestAudit_OneRecordPerOp`, `TestAudit_RefusalRateLimit`, `TestAudit_LeaderOnly`, `TestStatus_Users` |
| I-358 | `TestAudit_ChainHash`, `TestAudit_Retention_Table`, `TestAudit_Ls_Filters`, `TestStatus_Users`, `TestCheck_DormantWarning` |
| I-359 | `TestConfig_Validate_Table` |
| I-333 | `TestAudit_RecordNeverWaitsForTheVolume` |
