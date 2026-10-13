# 19 — Secrets: one encrypted store for the whole cluster

The volume needs a few credentials: an S3 bucket's access key (10 §4.1), the TMDB key the
scrape asks with (18 §11), and more to come. Until now each lived in a file on the device
that was given it (`s3nodes.yaml`, `tmdb.key`), so a voter without the bucket's key
re-placed its slots (I-148) and every device that scrapes needed the key typed in again.
This doc makes them **one store, replicated and encrypted**: the values live in the FSM as
ciphertext, so every member has them, and the one key that opens them, the **cluster
key**, lives in each device's OS keychain. A device that lacks the cluster key fetches it
from another member over the tailnet.

The keychain layer is errol's (`zombar/errol_v2`, `internal/keys`, `internal/secrets`):
`zalando/go-keyring` for the macOS Keychain, the Windows Credential Manager and the Linux
Secret Service, a 0600 file when the keyring is unavailable, and secret values read from
stdin only. errol keeps its secrets on one host; the replication and the cluster key are
this doc's.

*Status (2026-10-13): designed; docs and stubs first. I-303..I-309 are stubs in
`test/invariants/secrets_stubs_test.go`. PR 1 lands `internal/keychain` (I-309); PR 2 the
store, the key fetch and the CLI (I-303..I-307); PR 3 moves the S3 and TMDB keys in
(I-308) and rewords I-139 and I-302.*

## 1. The pieces

| Piece | Where | Holds |
|---|---|---|
| Cluster key | each device's OS keychain (service `tailraid`, account `cluster-<cluster ID>`), else `<data_dir>/cluster.key` (0600, dir 0700) | 32 random bytes |
| Key ID | FSM `Meta.SecretKeyID` | the first 8 bytes of HMAC-SHA256(key, `"tailraid key id"`): which key the store is under, never the key |
| Secrets | FSM `State.Secrets`, by name | name, nonce, ciphertext, the key ID, when, which node set it |
| Values in use | memory of the node that decrypts them | the plaintext, never written anywhere |

Nothing else changes hands: the join token stays where it is (08), the SMB share password
stays in the node's config (15).

## 2. The keychain layer (I-309)

`internal/keychain` wraps go-keyring behind four calls, `Get`, `Set`, `Delete` and
`Source`, as errol's `internal/keys` does:

- **Keyring first, never blocking.** A keyring that fails (a headless Linux host with no
  Secret Service, the confined systemd unit with no session bus, a locked Keychain) falls
  back to a file `<data_dir>/keys/<account>.key`, mode 0600 under a 0700 dir; a keyring
  error never fails `init`, `join` or `serve`. `Source` says where a value is: `keyring`
  or the file's path.
- **Account names are checked** (`^[a-z0-9][a-z0-9._-]{0,127}$`), so an account is never
  a path.
- macOS reaches the Keychain through `/usr/bin/security` (go-keyring's own shell-out), so
  the item's access list names that tool and a new tailraid binary reads it without a
  prompt; Windows uses the Credential Manager of the account the service runs as; Linux
  speaks D-Bus to the Secret Service of the user's session. All three are pure Go.
- Tests use go-keyring's in-memory mock; no test touches the real keychain.

## 3. The cluster key (I-303, I-304, I-305)

- **Made once.** `tailraid init` makes the key, keeps it in the keychain and proposes its
  ID (op 23, `SecretKeyInit`, set only while `Meta.SecretKeyID` is empty). An existing
  cluster has no key until the first `secret set`: that node makes one and proposes its
  ID the same way. Of two nodes racing, one proposal wins; the loser drops its key and
  fetches the winner's.
- **Fetched on demand.** A node that needs the key and has none (a fresh `join`, a device
  after the upgrade, a reinstall) asks the members in turn, the leader first, with
  `GET /v1/secrets/key`: a member route (the WhoIs member check, I-18) that answers only
  with the cluster header, only when the asker is a current member, and only with a key
  whose ID matches `Meta.SecretKeyID`. The asker checks the ID too, keeps the key in its
  keychain, and asks no further. The key crosses the tailnet's WireGuard tunnel between
  two members and nowhere else.
- **Never elsewhere.** The key is never in the FSM, a snapshot, `tailraid export`, a
  report, a log line, an error or any request but that one reply.
- **Lost everywhere.** If no device holds the key, the store cannot be read: `secret ls`
  lists every name as `unreadable`, and `secret rm --all` removes every secret and clears
  the key ID, so the next `secret set` makes a new key. Each secret is then entered again
  (`node add-s3`'s key, `media key`). Rotating the key is a non-goal of this version.

## 4. The store (I-306, I-307)

- **Encryption.** Each value is sealed with AES-256-GCM under a key derived for its name,
  HKDF-SHA256(cluster key, salt = the cluster ID, info = `"tailraid secret v1 " + name`),
  with a random 12-byte nonce and the name as additional data. A ciphertext copied to
  another name, under another key or altered by one bit does not open.
- **Commands.** Op 24 `SecretPut{Name, Nonce, Cipher, KeyID, At, By}` and op 25
  `SecretDelete{Name}` (or every name, which also clears the key ID). Both are log-entry
  codec 12 and snapshot frame 12; the leader refuses them while a live member reads an
  older codec (`ErrCodec`, 02 §Codec versions). A put whose key ID is not the FSM's is
  refused.
- **Bounds.** A name matches `^[a-z0-9][a-z0-9._/-]{0,127}$` (`tmdb`, `s3/b2`); a value
  is 1 to 16 KiB and holds no newline. At most 256 secrets.
- **Who encrypts.** The node the CLI talks to: `tailraid secret set <name>` reads the
  value from stdin (piped, or typed at a prompt that does not echo), never from argv or
  a flag, and sends it to its local node in an admin op over the tailnet; that node
  encrypts it and proposes the put. The leader and the log only ever see ciphertext.
- **Reading.** A node decrypts a secret only for its own use (an S3 bucket's
  credentials) or for the admin op `secret_get` that a consumer in the CLI calls (the
  scrape's TMDB key). There is no `tailraid secret get`: no command prints a value.

## 5. Commands

| Command | Does |
|---|---|
| `tailraid secret set <name>` | reads the value from stdin and stores it, replacing any value of that name; makes the cluster key first if the cluster has none |
| `tailraid secret ls` | each name, when it was set and by which node, and whether this node can open it; never a value |
| `tailraid secret rm <name>` | removes one; `--all` removes every secret and the key ID |

## 6. What moves in (I-308)

- **S3 node credentials** become the secret `s3/<node name>` (the access key and the
  secret key, as JSON). `node add-s3` and `node keys` write the secret; every node reads
  its buckets' credentials from the store, so every voter has them (I-148 holds on every
  leader). A node that still finds `<data_dir>/s3nodes.yaml` imports each entry the store
  lacks (a put that loses to an existing name is skipped) and keeps reading the file as a
  fallback while any member reads a codec older than 12. I-139 is reworded: the
  credentials are in the store, sealed, and never in plaintext in the FSM, a report, a
  log line or a request.
- **The TMDB key** becomes the secret `tmdb`. `media key` writes it; the scrape asks its
  node for it (`secret_get`), then `TAILRAID_TMDB_API_KEY`, then the old
  `<data_dir>/tmdb.key`, which `media key` imports and removes. I-302's "on the device
  that saved it" becomes "in the store".

## 7. Non-goals

Rotating the cluster key, a `secret get` command, secret files (certificates,
kubeconfigs), scoping a secret to some members, a page for secrets in the web UI,
redaction of logs beyond never logging a value (errol's redactor answers a different
threat: a model reading process output), and protecting a secret from a member: every
member holds the volume's data and can fetch the key, by design.

## Invariants

- **I-303** — The cluster key is 32 random bytes made once per cluster; a device keeps it
  only in its keychain or, when the keyring is unavailable, in `<data_dir>/keys/` (0600,
  dir 0700); it is never in the FSM, a snapshot, an export, a report, a log line, an
  error or any request or reply but the member key fetch.
- **I-304** — `GET /v1/secrets/key` answers only a current member with the cluster
  header, and only with a key whose ID equals `Meta.SecretKeyID`; a node accepts a
  fetched key only when its ID equals the FSM's, and keeps it in its keychain.
- **I-305** — `Meta.SecretKeyID` is set by op 23 only while it is empty, and cleared only
  by removing every secret; of nodes making a key at once, exactly one key becomes the
  store's and the others fetch it.
- **I-306** — A secret's value is in the FSM only as AES-256-GCM ciphertext under the
  HKDF key of its name, with a random nonce and its name as additional data; a
  ciphertext under another name, another key or with any bit changed does not decrypt;
  no value is ever logged, reported or placed on a command line.
- **I-307** — Ops 23–25 are codec 12, refused with `ErrCodec` while a live member reads
  an older codec; a put under a key ID that is not the FSM's, a name off its pattern, an
  empty, oversized or multi-line value, or a 257th secret is refused; `secret ls` lists
  names, times and setters, never values.
- **I-308** — S3 credentials and the TMDB key are read from the store first; a legacy
  `s3nodes.yaml` or `tmdb.key` is imported only into names the store lacks, and read as a
  fallback only while a member reads a codec older than 12 (the TMDB file is removed once
  imported).
- **I-309** — The keychain layer stores, reads and removes an account through the OS
  keyring, and through `<data_dir>/keys/<account>.key` (0600, dir 0700) when the keyring
  fails; a keyring failure never fails the caller; an account off its pattern is refused;
  `Source` names where a value is.

## Tests

| Invariant | Tests |
|---|---|
| I-303 | stub `TestStub_I_303` until PR 2 |
| I-304 | stub `TestStub_I_304` until PR 2 |
| I-305 | stub `TestStub_I_305` until PR 2 |
| I-306 | stub `TestStub_I_306` until PR 2 |
| I-307 | stub `TestStub_I_307` until PR 2 |
| I-308 | stub `TestStub_I_308` until PR 3 |
| I-309 | stub `TestStub_I_309` until PR 1 |
