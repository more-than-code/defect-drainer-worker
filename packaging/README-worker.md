# dd-worker host setup

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

Pick the release `X.Y.Z` (Settings -> Workers -> Add worker shows it).
`<release-base>` is the release download URL, for example
`https://github.com/<owner>/<repo>/releases/download`:

```sh
curl -fsSLO <release-base>/vX.Y.Z/install.sh
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

The worker reads `secrets.env` itself when it starts; the unit has no
`EnvironmentFile`, so the key never sits in the service's environment.
`install.sh secrets` restarts the service after writing it.

Claude Code is never updated by the worker: every agent run gets
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, which turns Claude Code's
auto-updater off, and `/usr/local/bin` is read-only to the service. Update
Claude Code yourself, as root.

It creates the `dd-worker` system user, and never overwrites an existing
`policy.toml`.

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

### What a job's agent can reach

- **Sandbox:** every agent except grok runs its CLI's own sandbox, `strict`,
  whatever the app asks for. The agent writes only its job's handoff and
  worktrees.
- **Git:** the agent can read its job's repo clone (`git status`, `diff`,
  `log` work) but cannot write it, and cannot read any other clone. It leaves
  its changes uncommitted; the worker commits them when the operator ships.
  One clone serves every job on the same repo URL, so its `.git` also holds
  the other jobs' branches on that repo: list a repo on hosts that only take
  jobs you'd let see each other's work.
- **Report evidence:** the agent and the reviewers can read this job's
  report screenshots, and no other job's.
- **Writes:** the agent writes its job's handoff and worktrees. It cannot
  write the worker's own files (`/var/lib/dd-worker/status.json` and
  `readiness/`) or the worker user's home. Directories it cannot read
  (other jobs, the clones, the credential, `~/.claude`) appear empty to it,
  and anything it writes there is thrown away with the sandbox; it never
  reaches the disk. One gap is accepted: the agent can create a *new* file
  directly in `/var/lib/dd-worker`. The worker never reads files there it
  doesn't know, and nothing runs from there.
- **Tool caches:** npm and other XDG caches go to `.cache/` in the job's
  handoff, not the home directory, and are removed with the job.
- **Network** (Claude only; other adapters ignore it): none by default. `agent_allowed_domains` lists the exact
  hosts the agent's sandboxed shell may reach, for example
  `["registry.npmjs.org"]` for npm installs. Lowercase hostnames only: no
  wildcards, IP addresses or ports, at most 16. A bad entry makes
  `dd-worker policy check` fail and the worker decline every job.
- **Cost** (Claude only: the other adapters report no cost): the server caps
  each job's agent cost (default $5). The loop
  stops with `budget` once the job's total, Resumes included, reaches it.
  `max_cost_usd` (whole dollars, 1 to 100) can lower that cap on this host,
  never raise it. With no cap from the server, the host's value is used, or $5.
- **Verify:** with `allow_verify_commands = false`, a job's verify commands
  are skipped, the job log says so, and no defect resolves from that job.

Every agent run logs `Permission mode forced to default —
CLAUDE_CODE_SUBPROCESS_ENV_SCRUB is set …` on stderr. That is expected: the
worker sets the scrub on purpose so the agent's shell can't print the API key,
and the allow and deny rules in the job's settings file apply either way.

The loop also stops when the agent writes a `BLOCKED: <reason>` line in its
`NOTES.md` (`blocked`), or when two iterations in a row change nothing
(`no_progress`).

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

For Defect Drainer's hosted service the origin is `https://api.defectdrainer.com`
(the API host; the console at `console.defectdrainer.com` does not accept workers).

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

```sh
sudo sh install.sh rollback
```

switches to the previous version and restarts. Rolling back to a version older
than 0.2.0 needs the credential copied back to `/etc/dd-worker/env` by hand
(0.2.0 moved it to `/var/lib/dd-worker/credential/`).

Versions before 0.2.0-rc2 refuse `agent_allowed_domains` and `max_cost_usd`
as unknown keys and then decline every job. Before rolling back past rc2,
remove both keys from `policy.toml`; after an upgrade to rc2, add them only
once the upgrade has succeeded.

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
