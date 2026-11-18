# Host setup

This guide is the same as `README-worker.md` in every release tarball, plus
how to check a release before you install it. The repository README lists what
the worker sends and never sends.

`dd-worker` runs Defect Drainer coding-agent jobs on a Linux host you own. It
connects out to your Defect Drainer server over a WebSocket, takes the jobs its
policy allows, runs the coding agent (Claude Code) in a sandbox, and pushes the
job branch when an operator ships. It never downloads code or updates itself.

Everything below runs as root (`sudo`). `install.sh` is POSIX sh, idempotent,
and prints each step.

## Requirements

- Linux on arm64 or amd64 (a Raspberry Pi 4/5 with 64-bit OS is fine).
- `git` 2.32 or newer, `bubblewrap` and `socat` (Claude's sandbox), `curl`:

  ```sh
  sudo apt-get install -y git bubblewrap socat curl
  ```

  `install.sh` checks for them and prints this line if any is missing; it
  installs nothing on its own.
- Claude Code installed system-wide at `/usr/local/bin/claude` (the unit runs
  `dd-worker run --claude-bin /usr/local/bin/claude`).
- An Anthropic API key for this host.
- An admin of your Defect Drainer org to create a join token.

## Install

Pick the release `X.Y.Z` (Settings -> Workers -> Add worker shows it). Releases
are published at
<https://github.com/more-than-code/defect-drainer-worker/releases>; the tag for
version `X.Y.Z` is `vX.Y.Z`.

```sh
curl -fsSLO https://github.com/more-than-code/defect-drainer-worker/releases/download/vX.Y.Z/install.sh
sudo sh install.sh install X.Y.Z
```

The `install.sh` attached to a release knows its release base. To install from
another place (a mirror, or a local copy for testing), set
`DD_WORKER_RELEASE_BASE` (`https://` or `file://`), e.g.
`sudo DD_WORKER_RELEASE_BASE=file:///srv/dd-worker sh install.sh install X.Y.Z`.

This downloads `dd-worker_X.Y.Z_linux_<arch>.tar.gz` and `SHA256SUMS` from the
same release, refuses the tarball if its SHA-256 does not match, and sets up:

| Path | Owner : mode | Holds |
|---|---|---|
| `/usr/local/lib/dd-worker/X.Y.Z/` | root : 0755 | this version (current and previous are kept) |
| `/usr/local/bin/dd-worker` | root, symlink | the current version |
| `/etc/dd-worker/` | root : 0755 | your config; the worker never writes here |
| `/etc/dd-worker/policy.toml` | root : 0644 | your host policy (you write it) |
| `/etc/dd-worker/secrets.env` | dd-worker : 0600 | `ANTHROPIC_API_KEY` |
| `/var/lib/dd-worker/` | dd-worker : 0700 | worker state: clones, jobs, spool |
| `/var/lib/dd-worker/credential/` | dd-worker : 0700 | the worker credential (rotated by the server) |
| `/home/dd-worker` | dd-worker : 0700 | an empty HOME |
| `/etc/systemd/system/dd-worker.service` | root : 0644 | the hardened unit |

It creates the `dd-worker` system user, and never overwrites an existing
`policy.toml`.

## Check a release

Each release has three tarballs, `install.sh` and `SHA256SUMS`:

- `dd-worker_X.Y.Z_linux_arm64.tar.gz`, `dd-worker_X.Y.Z_linux_amd64.tar.gz`,
  `dd-worker_X.Y.Z_darwin_arm64.tar.gz`;
- each holds `dd-worker`, `install.sh`, `dd-worker.service`,
  `policy.example.toml`, `README-worker.md`, `LICENSE` and `NOTICE`.

`install.sh` already checks the tarball against `SHA256SUMS`. To check by hand,
download the release's files into one directory and run:

```sh
sha256sum -c --ignore-missing SHA256SUMS
```

The release notes name the commit the release was built from. To match a binary
to its source, check out that commit and run
`make worker-dist WORKER_VERSION=X.Y.Z`. `dd-worker version` prints `X.Y.Z`.

## Policy

```sh
sudo cp /etc/dd-worker/policy.example.toml /etc/dd-worker/policy.toml
sudo $EDITOR /etc/dd-worker/policy.toml
```

Set `orgs` to your org id, and `repos` to limit which repos this host works on.
Keep `allow_verify_commands = false`: verify commands are not sandboxed.
`allow_ship = true` lets the worker push the job branch (the server opens the
PR). The file must stay root-owned and not group- or world-writable, or the
worker declines every job. `sudo sh install.sh status` shows how it reads it.

## Secrets

```sh
sudo sh install.sh secrets
```

Paste the API key at the hidden prompt (or pipe it on stdin). It is never an
argument and never printed. Run it again to replace the key; a running worker
is restarted.

## Enrol

In the console, Settings -> Workers -> Add worker creates a one-time join token
(valid 1 hour). Then:

```sh
sudo sh install.sh enroll https://<your-server>
```

Paste the token at the hidden prompt (or pipe it on stdin). This runs
`dd-worker init` as the `dd-worker` user, which writes the credential to
`/var/lib/dd-worker/credential/`. Re-enrolling the same host keeps its worker id.

## Start, stop, status

```sh
sudo sh install.sh start     # enable and start the unit
sudo sh install.sh status    # unit state, version, enrolment, policy, connection
sudo sh install.sh stop
journalctl -u dd-worker -f   # the worker's log
```

The unit restarts on failure, except after exit 3 (revoked) or 4 (replaced by
another connection, or a protocol or org mismatch): those need you to act, so
the worker stays down.

## Upgrade and roll back

```sh
sudo sh install.sh upgrade X.Y.Z
```

installs the new version alongside the current one, switches to it and
restarts. If the worker is not active with a `connected` log line within 60
seconds, it switches back and restarts the old version on its own. Only the
current and previous versions are kept.

If you also run the server, upgrade the server first, then its workers.

```sh
sudo sh install.sh rollback
```

switches to the previous version and restarts. Rolling back to a version older
than 0.2.0 needs the credential copied back to `/etc/dd-worker/env` by hand
(0.2.0 moved it to `/var/lib/dd-worker/credential/`).

## Remove

```sh
sudo sh install.sh remove           # stop, disable, delete unit and binaries
sudo sh install.sh remove --purge   # also state, config, the user and HOME
```

Removing never revokes the worker. Revoke it in the console (Settings ->
Workers -> Revoke); that cannot be undone, and the host must enrol again.

## Server requirements

The server origin must be `https://`. If a reverse proxy sits in front of it,
`/worker/v1/ws` needs WebSocket Upgrade, the `Authorization` header passed
through, and an idle timeout of at least 60 seconds (the worker sends a
heartbeat every 20 s).
