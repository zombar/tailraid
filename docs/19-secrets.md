# 19 — Secrets: one encrypted store for the whole cluster

The volume needs a few credentials: an S3 bucket's access key (10 §4.1), the TMDB key the
scrape asks with (18 §11), and more to come. Before this doc each lived in a file on the
device that was given it (`s3nodes.yaml`, `tmdb.key`), so a voter without the bucket's key
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

*Status (2026-10-14): built in three PRs: `internal/keychain` (I-309); the store in
`internal/secrets`, ops 23–25 at codec 11, the member key fetch, `secret set|ls|rm` and
soak phase 24 (I-303..I-307); and the S3 and TMDB keys moved in, with the import of the
per-device files gone (I-308, I-139 and I-302 reworded). `secret set` is the one way in:
`node keys` and `media key` are gone, each consumer checks its key when it uses it, and
nothing carries the old files over: the volume is re-created with the store.*

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
- **Bounded.** Each keyring call gets `keychain.Timeout` (5 s): a locked Keychain can wait
  on a prompt nobody sees and a missing Secret Service can stall on D-Bus; past it the call
  counts as a failure and the file is used.
- **Account names are checked** (`^[a-z0-9][a-z0-9._-]{0,127}$`), so an account is never
  a path.
- macOS reaches the Keychain through `/usr/bin/security` (go-keyring's own shell-out), so
  the item's access list names that tool and a new tailraid binary reads it without a
  prompt; Windows uses the Credential Manager of the account the service runs as; Linux
  speaks D-Bus to the Secret Service of the user's session. All three are pure Go.
- Tests use go-keyring's in-memory mock; no test touches the real keychain.

## 3. The cluster key (I-303, I-304, I-305)

- **Made once.** The leader makes the key on its first maintenance tick when the cluster
  has none (the node that ran `tailraid init` leads first, so a cluster has its key from
  its first minute): it keeps the key in its keychain and proposes its ID (op 23,
  `SecretKeyInit`, set only while `Meta.SecretKeyID` is empty). A `secret set` on a
  cluster still without one makes it the same way. A node makes at most one key at a
  time (its tick and a `secret set` at once propose one, the second taking the first's),
  and a fetch runs outside the store's lock, since the member asked may be the asker
  itself. Of two nodes racing, one proposal wins; the loser drops its key and fetches the
  winner's.
- **Fetched on demand.** A node that needs the key and has none (a fresh `join`, a device
  after the upgrade, a reinstall) asks the members in turn, the leader first, with
  `GET /v1/secrets/key`: a member route (the WhoIs member check, I-18) that answers only
  with the cluster header, only when the asker is a current member, and only with a key
  whose ID matches `Meta.SecretKeyID`. The asker checks the ID too, keeps the key in its
  keychain, and asks no further. The key crosses the tailnet's WireGuard tunnel between
  two members and nowhere else. A running node that holds no copy while the store has a
  key fetches it on its maintenance tick (once a minute, and once at start), so a device
  opens its buckets' credentials without a `secret` command run on it; the hot path
  itself never fetches (§6).
- **Never elsewhere.** The key is never in the FSM, a snapshot, `tailraid export`, a
  report, a log line, an error or any request but that one reply.
- **Lost everywhere.** If no device holds the key, the store cannot be read: `secret ls`
  lists every name as `unreadable`, and `secret rm --all` removes every secret and clears
  the key ID, so the next `secret set` makes a new key. Each secret is then entered again
  with `secret set`. Rotating the key is a non-goal of this version.

## 4. The store (I-306, I-307)

- **Encryption.** Each value is sealed with AES-256-GCM under a key derived for its name,
  HKDF-SHA256(cluster key, salt = the cluster ID, info = `"tailraid secret v1 " + name`),
  with a random 12-byte nonce and the name as additional data. A ciphertext copied to
  another name, under another key or altered by one bit does not open.
- **Commands.** Op 24 `SecretPut{Name, Nonce, Cipher, KeyID, At, By}` and op 25
  `SecretDelete{Name}` (or every name, which also clears the key ID). All three are
  log-entry codec 11 and snapshot frame 11 (codec 12 is sealed S3's, 10 §4.7: a codec
  promises every lower one, and the store landed first); the leader refuses them while a
  live member reads an older codec (`ErrCodec`, 02 §Codec versions). A put whose key ID is not the FSM's is
  refused.
- **Bounds.** A name matches `^[a-z0-9][a-z0-9._/-]{0,127}$` (`tmdb`, `s3/b2`); a value
  is 1 to 16 KiB and holds no newline. At most 256 secrets.
- **Who encrypts.** The node the CLI talks to: `tailraid secret set <name>` reads the
  value from stdin (piped, or typed at a prompt that does not echo), never from argv or
  a flag, and sends it to its local node in an admin op over the tailnet; that node
  encrypts it and proposes the put. The leader and the log only ever see ciphertext.
- **Reading.** A node decrypts a secret only for its own use (an S3 bucket's
  credentials) or for the admin op `secret_get` that a consumer in the CLI calls (`node
  add-s3` for the bucket's credentials, the scrape for its TMDB key). There is no
  `tailraid secret get`: no command prints a value. With users (docs/21 §7)
  the store gains a personal name space, `user/<name>/…`, whose values `secret get`
  prints to their owner only; a system value still crosses to no CLI, and the two
  consumers open theirs inside the node (I-356).
- **One way in.** `secret set` is the only command that stores a secret. A consumer
  checks its key when it uses it, as a service normally does: `node add-s3` refuses a
  bucket its credentials do not open, the scrape asks TMDB once before a keyed run and a
  node's probe reports a bucket its credentials do not open as dead.

## 5. Commands

| Command | Does |
|---|---|
| `tailraid secret set <name>` | reads the value from stdin and stores it, replacing any value of that name; makes the cluster key first if the cluster has none; the one way in, for `s3/<node>/access_key`, `s3/<node>/secret_key`, `tmdb` and any other |
| `tailraid secret ls` | each name, when it was set and by which node, and whether this node can open it; never a value |
| `tailraid secret rm <name>` | removes one; `--all` removes every secret and the key ID |

With users (docs/21 §7): `secret set <name>` writes the caller's own `user/<name>/…`,
`--system` (admins) the names above, `secret get <name>` prints one's own, and `secret
ls --system` lists the system names with the user who set each.

## 6. What moves in (I-308)

- **S3 node credentials** are the secrets `s3/<node name>/access_key` and
  `s3/<node name>/secret_key`, set with `secret set` before `node add-s3 <name>`, which
  reads them through its node, checks the bucket with them as before and only then
  registers it; a rotated key is a new `secret set` of the two. A node name is one the
  secret pattern takes (lower-case letters, digits and `. _ -`). Every member holds the
  credentials at once, so every voter can judge the bucket when it leads (I-148). Each
  node serves a bucket with the store's pair; one that cannot open it (no copy of the
  cluster key yet) sees the bucket dead until its fetch succeeds. I-139 is reworded: the credentials are in the
  store, sealed, and never in plaintext in the FSM, a report, a log line or a request but
  the CLI's admin op to its own node.
- **The TMDB key** is the secret `tmdb`, set with `secret set tmdb`. The scrape asks its
  node for it (`secret_get tmdb`), checks it with TMDB once before a run that needs it
  (movies, tv or episodes), and without one runs the keyless groups. There is no
  `media key` and no environment override. I-302's "on the device that saved it"
  becomes "in the store".
- **No carry-over.** Nothing reads or writes the old `s3nodes.yaml` and `tmdb.key`: a
  volume made before the store is re-created with it, and its secrets entered again.
  While a member reads a codec older than 11 the leader refuses every put and
  `secret set` says so.

## 7. Soak phase 24

A secret made for the run is set through one node and read back through every node (the
sealed value replicates, each node opens it with the cluster key and fetches the key
when its keychain lacks it); every node lists it readable with no value in the reply; a
replace through another node reads back through the first; a node whose keychain copy
is wiped and which is restarted fetches the key and reads it (`SecretEnv.ForgetSecretKey`:
the compose rig removes the fallback file and restarts the container, the harness
empties the device's in-memory keychain; `tailraid verify` cannot on a live cluster's
devices and records the check skipped); with the node that set it killed, the others
read it; a removal through one node reaches every node. The phase removes its secret
whatever happens and touches no other, so a live cluster's own secrets stay.

## 8. Non-goals

Rotating the cluster key, a `secret get` of a system value (a personal one is docs/21
§7), secret files (certificates, kubeconfigs), scoping a secret to some members, a page for secrets in the web UI,
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
- **I-307** — Ops 23–25 are codec 11, refused with `ErrCodec` while a live member reads
  an older codec; a put under a key ID that is not the FSM's, a name off its pattern, an
  empty, oversized or multi-line value, or a 257th secret is refused; `secret ls` lists
  names, times and setters, never values.
- **I-308** — `secret set` is the only command that stores a secret; S3 credentials
  are the secrets `s3/<node>/access_key` and `s3/<node>/secret_key` and the TMDB key is
  `tmdb`, read from the store and nowhere else by `node add-s3`, every node's bucket
  client and the scrape, each of which checks its key when it uses it; no file in a
  data directory holds a secret.
- **I-309** — The keychain layer stores, reads and removes an account through the OS
  keyring, and through `<data_dir>/keys/<account>.key` (0600, dir 0700) when the keyring
  fails; a keyring failure never fails the caller; an account off its pattern is refused;
  `Source` names where a value is.

## Tests

| Invariant | Tests |
|---|---|
| I-303 | `TestStore_ReplicatedAcrossNodes`, `TestSoak_SecretsAgainstHarness` |
| I-304 | `TestStore_ReplicatedAcrossNodes`, `TestStore_FetchChecksTheKeyID`, `TestSecretKey_MembersOnly`, `TestFetchKey_OnTick`, `TestSoak_SecretsAgainstHarness` |
| I-305 | `TestStore_KeyRace`, `TestSecretApply_Table` |
| I-306 | `TestSeal_Open`, `TestStore_ReplicatedAcrossNodes`, `TestSecretState_Snapshot`, `TestSoak_SecretsAgainstHarness` |
| I-307 | `TestSecretOps_Codec11`, `TestSecretApply_Table`, `TestSecretState_Snapshot`, `TestStore_Bounds_RemoveAll`, `TestSoak_SecretsAgainstHarness` |
| I-308 | `TestStore_Opened`, `TestFetchKey_OnTick`, `TestS3SecretNames`, `TestCLI_AddS3`, `TestNeedsKey_Table` |
| I-309 | `TestKeychain_KeyringFirst`, `TestKeychain_FallbackFile_Table`, `TestKeychain_KeyringRecovers`, `TestKeychain_Names_Table` |
