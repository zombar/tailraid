# Users and permissions

Until the volume has its first user, every mount and every browser on your tailnet sees
the whole volume, as it always did. The moment the first user exists, the rules switch on
everywhere at once: each request acts as a user, a folder shows only to those it was
granted to, and a mount on a device with no binding (below) acts as nobody and sees
nothing. So set up the first admin and bind each device in one sitting. Users have no
passwords: each person holds an Ed25519 key per device, kept in their own keychain. The
design is [docs/21](21-users.md).

## The first admin

`tailraid init` prints an admin invite beside the join token. The code is valid for 24
hours and **for the name it prints**: by default the name of the OS account that ran
`init` (`init --admin <name>` picks another). On the device you will administer from, run
it as yourself, never with `sudo`, since the key goes into your own keychain:

```sh
tailraid user create gurgeh <code>     # the name and code init printed
tailraid user whoami                   # gurgeh (admin)
```

A code presented under another name is refused as "no such invite, or it has expired". If
the code has expired or is lost, mint another on any **voter** (`tailraid node ls`, the
`VOTER` column), as the user the daemon runs as, and use it on any device:

```sh
tailraid user bootstrap --name gurgeh  # on a voter; sudo where the service runs as root
```

## Every other device, straight away

A mount presents the local uid (macOS, Linux) or SMB account (Windows) to its node, and
each device maps those to users in its own bindings. An unbound credential is nobody: its
mount lists an empty root and refuses everything. `tailraid check` warns "device has no
bindings" on every device until it has one. On each of your other devices, either give
yourself a key there, which also binds your uid:

```sh
tailraid user key invite               # on a device that already has your key: prints a code
tailraid user key add <code>           # on the new device, as yourself
```

or, as an admin, bind a local account to an existing user without a key:

```sh
tailraid user bind --uid 501 kim                     # a uid on this device
tailraid user bind --uid 1000 gurgeh --node 100.64.0.12   # a uid on another device (its Tailscale IP)
tailraid user bind --smb tailraid kim                # Windows: the account the mount uses
```

A key is needed only for the CLI and the browser sign-in; a binding is all a mount needs.

## People, groups and grants

| Step | Command |
|---|---|
| Invite a person (admins) | `tailraid user invite kim [--admin]`, then Kim runs `tailraid user create kim <code>` on their device |
| Make a group | `tailraid group create family` · `group add family kim` · `group ls` |
| Share a folder | `tailraid grant /Photos @family read` · `grant /Photos/Kim kim write` |
| Take it back | `tailraid revoke /Photos @family` |
| Review who has what | `tailraid access ls [path]` |
| Suspend or remove a person | `tailraid user disable kim` · `enable` · `rm` (the row stays for the audit log) |
| Revoke a lost device | `tailraid user ls kim` for the key ID, then `user key rm kim <key-id>` |

Grants are allow-only and inherited down the tree: a person's role on a folder is the
highest one granted to them, a group of theirs or `everyone` on it or any folder above it.
There are no deny grants, so a folder is hidden by not granting it, which means it must
not sit under a folder you do grant. A person with no grants sees an empty root; a folder
on the way to one of their grants shows only that path.

| Role | Allows |
|---|---|
| `read` | open, list, stream, search hits, thumbnails, the Media tab, the versions list |
| `write` | read, plus create, write, rename within the folder, delete to the trash and restore, the folder flags |
| `admin` | write, plus grant and revoke under the folder, restore and revert from snapshots, pins |

## Admins

Members of the built-in `admins` group hold `admin` everywhere, see every folder and alone
manage users, groups, system secrets and the cluster. The last active admin cannot be
disabled, removed or dropped from `admins`; the cluster refuses it. If every admin's key is
lost anyway, `tailraid user bootstrap` on a voter mints a new admin invite. Local root on
a member device is full trust by design.

## One person, two users

An admin sees everything, so to keep a folder out of your own everyday view, make a second,
plain user for yourself in the same OS account and switch between them. `user use`
rebinds this device's mount to the user it switches to, so the mount follows:

```sh
tailraid user invite jim                     # as the admin
tailraid user create jim <code>              # same OS account; jim is now active
tailraid --identity gurgeh grant /Movies jim write   # one command as the admin
tailraid user use                            # lists the identities held here, * marks the active one
tailraid user use gurgeh                     # back to the admin, the mount with it
```

This is a convenience, not a protection from yourself: whoever uses the OS account can
switch. The audit log names the user who acted.

## The browser

The web UI signs a person in from a one-time link: run `tailraid web login` on your own
device (as the identity you want) and open the link it prints; one sign-in holds on every
node's page. `tailraid web logout` ends the sessions this device's key started, `--all`
every one of yours.

## Secrets and the audit log

`tailraid secret set <name>` stores a **personal** secret (`user/<you>/<name>`) that only
you can read back with `secret get`. The cluster's own secrets (the S3 keys and the TMDB
key) are system secrets an admin sets with `--system`, such as
`tailraid secret set tmdb --system`; no command prints one back. Every user, group, grant,
secret and sign-in change, and every refusal, is in a hash-chained audit log:
`tailraid audit ls [--since 7d] [--user kim] [--kind k]` shows an admin everyone's records
and a user their own.
