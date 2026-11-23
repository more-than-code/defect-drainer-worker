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
- `git` 2.32 or newer, `bubblewrap` and `socat` (Claude's sandbox and the
  verify sandbox), `curl`:

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
| `/etc/dd-worker/secrets.env` | root : 0600 (read by systemd LoadCredential) | `ANTHROPIC_API_KEY` |
| `/var/lib/dd-worker/` | dd-worker : 0700 | worker state: clones, jobs, spool |
| `/var/lib/dd-worker/credential/` | dd-worker : 0700 | the worker credential (rotated by the server) |
| `/home/dd-worker` | dd-worker : 0700 | an empty HOME |
| `/etc/systemd/system/dd-worker.service` | root : 0644 | the hardened unit |

From 0.2.0-rc8 the unit has `LoadCredential=secrets.env:/etc/dd-worker/secrets.env`:
systemd reads the root-owned file when the service starts and hands the worker
a private copy in `$CREDENTIALS_DIRECTORY`, which agents can't read. The
`dd-worker` user can't read `/etc/dd-worker/secrets.env` itself, and the unit
has no `EnvironmentFile`, so the key never sits in the service's environment.

- The unit doesn't start without the file, so `install.sh` creates an empty
  one (no keys) when there is none.
- An edit takes effect when the service restarts. `install.sh secrets`
  restarts it after writing the file.
- `dd-worker run` started by hand, with no `CREDENTIALS_DIRECTORY`, reads the
  file directly. It must then be readable by that user: root- or
  `dd-worker`-owned, 0600.

Claude Code is never updated by the worker: every agent run gets
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, which turns Claude Code's
auto-updater off, and `/usr/local/bin` is read-only to the service. Update
Claude Code yourself, as root.

Every Claude task also gets `BASH_DEFAULT_TIMEOUT_MS` and `BASH_MAX_TIMEOUT_MS`
set to 1500000 (25 min, instead of Claude Code's 10-min default), so a slow
test suite on a small host isn't stopped mid-run.

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
A project's verify commands run in the worker's verify sandbox (see
[Verify sandbox](#verify-sandbox)); `verify_commands = "off"` skips them.
`allow_ship = true` lets the worker push the job branch (the server opens the
PR). The file must stay root-owned and not group- or world-writable, or the
worker declines every job. `sudo sh install.sh status` shows how it reads it.

### Managing the policy from a host manager

From 0.2.0-rc6, a host manager such as Deploybot can own `policy.toml` and
`secrets.env`: it shows and versions them, and applies an edit only after the
worker accepts it. It writes both files root-owned (`policy.toml` 0644,
`secrets.env` 0600) and calls these commands as the `dd-worker` user:

| Command | Exit codes |
|---|---|
| `dd-worker policy check --file <path\|->` | 0 valid; 2 invalid, with one stderr line `bad_<reason>` (for example `bad_managed_url`, `bad_unknown_key`); 1 a usage error, or the file can't be read |
| `dd-worker secrets check --file <path\|->` | the same; reasons `bad_format`, `bad_name`, `bad_duplicate`, and for a path `bad_symlink`, `bad_not_regular`, `bad_mode`, `bad_owner` |
| `dd-worker health --wait <duration>` | 0 once the running worker has loaded the `policy.toml` now on disk, finds it valid and enabled, and is connected; 3 when the wait (at most 10m) ends, with one stderr line: `not_running`, `policy_invalid` or `not_connected` |
| `dd-worker health --idle` | 0, or 4 (`busy`) while a job runs |

- `--file -` reads the candidate from stdin and checks its content only:
  owner and mode are the manager's job. A path is checked with the same file
  rules as the installed file (`policy check --syntax-only` skips them).
- The checks never print the file's contents or a secret's value. A valid
  policy prints one line with its sha256.
- `health` only reads `/var/lib/dd-worker/status.json` and `policy.toml`: it
  needs no network and writes nothing.
- A typical apply: check the candidate, run `health --idle` (or apply anyway),
  swap the file, restart the worker, then `health --wait 30s`, and roll back
  on any non-zero exit.
- `secrets.env` needs 0.2.0-rc8 or later on the host, because older workers
  read the file themselves and can't read a root-owned one. Manage it as its
  own entry (for example `dd-secrets`, mode `0600`), check candidates with
  `secrets check --file -`, and restart after the swap: the worker gets new
  keys only on a restart. As `dd-worker`, a path check of the root-owned file
  can't open it (exit 1).

Set `managed_url` in `policy.toml` to the page where the file is edited
(https only, at most 512 characters, no user or password). The console then
shows "Managed in Deploybot → Edit" with that link; without it, "Edited on
the host". The worker reports the file's sha256 as the policy version, which
equals `sha256sum /etc/dd-worker/policy.toml`.

### What a job's agent can reach

- **Sandbox:** every agent except grok runs its CLI's own sandbox, `strict`,
  whatever the project asks for. The agent writes only its job's handoff and
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
- **Browser** (Claude fix jobs only): off unless `agent_browser` is set; see
  [After-fix screenshots](#after-fix-screenshots).
- **Job stack** (browser jobs only): a project's services and a throwaway
  Postgres (with `agent_postgres`), loopback only; see
  [Job stack](#job-stack).
- **Network** (Claude only; other adapters ignore it): none by default. `agent_allowed_domains` lists the exact
  hosts the agent's sandboxed shell may reach, for example
  `["registry.npmjs.org"]` for npm installs. Lowercase hostnames only: no
  wildcards, IP addresses or ports, at most 16. A bad entry makes
  `dd-worker policy check` fail and the worker decline every job.
  A project can ask for fewer of these hosts (from 0.2.0-rc3): its jobs then
  reach only the hosts in both lists, and the job log names each one dropped.
- **Cost** (Claude only: the other adapters report no cost): the server caps
  each job's agent cost (default $5). The loop
  stops with `budget` once the job's total, Resumes included, reaches it.
  `max_cost_usd` (whole dollars, 1 to 100) can lower that cap on this host,
  never raise it. With no cap from the server, the host's value is used, or $5.
  The job also records its cost by model (`agentRun.costByModel`,
  `review.costByModel`); the cap stays on the total.
- **Model** (Claude; from 0.2.0-rc4): the server sends each job's model
  (`sonnet` unless the project sets one) and the worker runs Claude with
  `--model`; reviewers use the job's model. `allowed_models` limits the
  models this host runs, for example `["sonnet", "haiku"]`: names match
  exactly and case-sensitively (an alias does not cover a full `claude-…` id), at most 16. A job
  for another model, or a Claude job with no model, is declined
  `model_not_allowed` and waits for another worker. Unset means any model;
  an empty list or a bad name makes `dd-worker policy check` fail
  (`bad_allowed_models`).
- **Verify:** a project's verify commands run in a separate sandbox, not the
  agent's; see [Verify sandbox](#verify-sandbox).

### Verify sandbox

From 0.2.0-rc9, a project's verify commands (Settings -> Verification,
written by the org's admins) run on the worker inside a bubblewrap sandbox,
once before the agent's change (the baseline) and once after it.
`verify_commands = "sandbox"` is the default. `verify_commands = "off"` skips
them on this host, and the server then gives jobs with checks to other
workers. There is no unsandboxed setting: if the sandbox can't start, the
checks are skipped with `verify_sandbox_unavailable`, the job log says so,
and no defect resolves from that job. `allow_verify_commands` is deprecated
(`dd-worker policy check` prints `deprecated: allow_verify_commands`) and read
until 1.0: on a host without `verify_commands`, `allow_verify_commands =
false` still means off, and `true` means sandbox. Replace it with
`verify_commands`.

What a verify command can reach:

- **Files:** the host, read-only. Empty in its place: `/etc/dd-worker`,
  `/var/lib/dd-worker` (the credential, `status.json`, the clones, other jobs,
  agent state), the systemd copy of `secrets.env`, the worker user's home,
  `/tmp`, `/run` and `/var/run`, and every path hidden from the agent
  (`~/.ssh`, `~/.config`, cloud credentials and the rest).
- **Writes:** only its job's worktrees, plus a temporary dir (`TMPDIR`) and a
  cache dir that the worker makes fresh in the job's own dir (not the
  agent's handoff) and removes after each run. Each worktree's `.git` and its
  clone's git dir stay read-only, so a check can't change what ships. A
  hidden dir takes writes up to 64 MiB, then thrown away.
- **Links:** every path the sandbox binds is checked first. If the agent
  left a symlink there (say, the worktree's `.git` pointing at a secret), the
  checks are skipped with `verify_sandbox_unavailable`; nothing is mounted.
- **Processes:** its own PID namespace: it can't see or signal the worker or
  other jobs, and can't make a user namespace of its own. A timeout
  (20 minutes per command) or a cancel kills every process the command
  started. Each command may open 8192 files, run 2048 processes and write
  files up to 8 GiB. Memory isn't capped by the sandbox: the systemd unit's
  limits apply to the whole worker.
- **Kernel calls:** bwrap loads a seccomp filter before the command starts,
  and nothing inside can lift it. Calls a build or test never needs fail with
  `EPERM`: mounting, changing root, creating or joining a namespace (`unshare`,
  `setns`, `clone` with a `CLONE_NEW*` flag), loading kernel code (modules,
  kexec), BPF, `perf_event_open`, keyrings, `userfaultfd`, file handles
  (`open_by_handle_at`), and setting the clock, swap, quotas, accounting, the
  kernel log or the power state. `clone3` and io_uring fail with `ENOSYS`, so
  libc falls back to `clone` and Node's libuv to its thread pool. A call
  through another architecture's table (i386 or x32 on amd64, arm32 on arm64)
  is refused. `ptrace` stays allowed: the sandbox has its own PID namespace,
  and sanitizers and debuggers need it. The self-test checks that the filter
  is loaded; without it the sandbox is unavailable.
- **Environment:** built for the check, not inherited: `PATH`, an empty
  `HOME`, `TMPDIR`, `LANG`, `CI=1`, the proxy variables, and cache dirs for
  npm, Go, pip, pub, cargo and XDG. Nothing from `secrets.env`, no adapter
  key, no test-account password.
- **Services:** none. A job stack's Postgres and services are for the agent
  only; tests that need a database must skip without one.
- **Network:** none of its own. `HTTPS_PROXY` and `HTTP_PROXY` point at a
  proxy in the worker that reaches only the hosts the job's agent may reach:
  `agent_allowed_domains`, narrowed by the project's agent hosts. A `CONNECT`
  to one of them on port 443 or 80, or a plain-HTTP request to one on port
  80, is passed. Anything else fails: other hosts, other ports, IP
  addresses, and connections that skip the proxy. The proxy answers 403 and
  the job log says `verify: egress refused <host>` once per host, with the
  host name only. The proxy never connects to an address on the host itself
  or a private network (loopback, link-local, the RFC 1918 private ranges,
  carrier-grade NAT and IPv6 unique-local addresses), even for an allowed
  name, so a registry on your LAN can't be used. It holds at most 48 connections
  at once and closes a tunnel idle for 2 minutes. A tool that ignores the
  proxy variables has no network.

Hosts a check typically needs, to add to `agent_allowed_domains`:

| Check | Hosts |
|---|---|
| `npm ci`, `pnpm install`, `yarn` (v2+) | `registry.npmjs.org` |
| `yarn` (v1) | `registry.yarnpkg.com` |
| `go mod download`, `go test` | `proxy.golang.org`, `sum.golang.org` |
| `pip install` | `pypi.org`, `files.pythonhosted.org` |
| `cargo build` | `index.crates.io`, `static.crates.io` |
| Maven | `repo.maven.apache.org` |

When a check fails on a fetch, the job log names each refused host.

The sandbox needs `bubblewrap` and `socat` and unprivileged user namespaces,
as Claude's sandbox does. The worker checks it at start and every 5 minutes:
it starts the sandbox and checks that a hidden file is gone, a read-only dir
refuses a write, and the proxy passes an allowed host and refuses another
(no network needed). If the sandbox fails to start for a check, that check
is skipped the same way, never counted as failed. While that passes, the worker tells
the server it can run checks (`verify_sandbox.v1`). `dd-worker status`
prints `verify sandbox: ready`, or `unavailable (<reason>)`, for example
`unavailable (bwrap: not found)`. A macOS worker has no verify sandbox: it
skips every check.

### After-fix screenshots

A fix job for a UI defect resolves only with an after-fix screenshot. A host
can take one with a headless browser. It's off by default.

1. Install a browser as root, for example Debian's Chromium:

   ```sh
   sudo apt-get install chromium
   ```

   Install CJK fonts too, so pages in Chinese, Japanese or Korean render as
   text in screenshots instead of boxes (about 89 MB; Chromium picks them up
   the next time it starts):

   ```sh
   sudo apt-get install --no-install-recommends fonts-noto-cjk
   ```

2. Set `agent_browser` in `policy.toml` to its absolute path, for example
   `agent_browser = "/usr/bin/chromium"`. The file must be owned by root and
   not group- or world-writable, or `dd-worker policy check` fails
   (`bad_agent_browser`) and the worker declines every job.
3. Restart the worker. The console's Settings → Workers shows the browser as
   ready, with its version and the hidden sockets; on the host,
   `sudo cat /var/lib/dd-worker/status.json` shows the same under `tools`
   and `policy.masked_sockets`.

What it changes, for jobs the server sends with screenshots on:

- The agent's sandbox may use unix sockets: the browser needs them, and so
  do some test runners. Every other rule above stays.
- To keep the agent off the host's own services, the worker looks for unix
  sockets under `/run`, `/var/run`, `/tmp` and `/dev/shm` that its user could
  connect to and doesn't own, and hides them from the agent: the socket's
  directory, or the socket itself when it sits directly in `/run` or `/tmp`.
  It looks at start, every 5 minutes, and again when a browser job starts,
  without connecting to anything. A directory the worker can enter but not
  list is hidden whole. The list shows in the console and in `status.json`.
  `agent_deny_sockets = ["/run/example"]` (absolute paths, at most 32) hides
  more. If the scan can't finish (more than 20,000 entries in one root, or
  over 3 seconds), the browser reports `scan_incomplete` and the host takes
  no browser jobs.
- The worker writes a `dd-shot` helper into the job. The agent runs the
  app's preview command (a project setting) in its worktree, screenshots pages
  with the browser into the job's evidence, and stops the preview, all in
  one sandboxed command. The preview gets no app secrets.
- A request this host doesn't allow is dropped and noted in the job log; the
  job runs without it.

Only Linux hosts take browser jobs. Move the control plane to a release that
knows browser workers (0.2.0-rc3 or later) **before** setting
`agent_browser` on any host: an older server refuses the browser's readiness
entry and keeps closing the worker's connection.

### Job stack

A project whose pages need a backend can define a job stack in Defect
Drainer: up to 4 services (install, migrate, seeds, start, a port and a
health path) and a throwaway Postgres. A browser job then boots that stack
inside the agent's sandbox, both for the agent to try the app and for the
after-fix screenshot. It needs a browser host (above); Postgres needs one more
policy key.

1. Install the Postgres server binaries as root and turn off the system
   service, which the stack doesn't use, for example on Debian:

   ```sh
   sudo apt-get install postgresql
   sudo systemctl disable --now postgresql
   ```

2. Set `agent_postgres` in `policy.toml` to the directory holding `initdb`,
   `pg_ctl` and `postgres`, for example
   `agent_postgres = "/usr/lib/postgresql/15/bin"`. The directory and the three
   files must be owned by root and not group- or world-writable, or
   `dd-worker policy check` fails (`bad_agent_postgres`) and the worker
   declines every job.
3. Restart the worker. Settings → Workers shows `postgres` as ready with its
   version; `status.json` shows it under `tools` and `policy.postgres`.

What it changes, for jobs of a project with a job stack:

- The worker writes a `dd-stack` helper next to `dd-shot`. In one sandboxed
  command it creates a fresh database in the job's temp directory (loopback
  only, no unix socket), runs each service's steps in its worktree, waits up
  to 120 s for each health URL, runs the agent's or dd-shot's command, then
  stops everything. Its logs stay in `.dd-stack/` in the job's handoff.
- The env it gives the services holds dev-only values the project's admin set,
  plus a fresh random value per job run, which the worker removes from every
  log, note and event it sends.
- A project can also set a **Stack sign-in**: its own seed user (an email and
  password valid only in the job's throwaway database) and how to sign in, a
  form or an HTTP call. `dd-shot` then signs in before its screenshots with
  `dd-worker shot`, a browser driver built into the worker binary (nothing
  is installed), and takes no screenshot if sign-in fails. The password is
  removed from everything the worker sends, and a fix whose diff adds it
  never resolves or ships.
- A job whose services need Postgres goes only to a host with
  `agent_postgres` set and Postgres ready; this worker declines it otherwise
  (`policy_denied`, "no Postgres (policy)"). Services on a job that isn't a
  browser job here are dropped and noted in the job log.

Move the control plane to a release that knows the job stack (0.2.0-rc6
or later) **before** setting `agent_postgres` on any host: an older server
refuses the `postgres` readiness entry.

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
is restarted. The file is written root-owned 0600 (`dd-worker`-owned on a host
rolled back to 0.2.0-rc7 or older, whose unit has no `LoadCredential`).

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
sudo sh install.sh status    # unit state, version, secrets.env, enrolment, policy, connection
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
seconds, it switches back (the old unit and `secrets.env` owner too) and
restarts the old version on its own. Only the current and previous versions
are kept.

If you also run the server, upgrade the server first, then its workers.

```sh
sudo sh install.sh rollback
```

switches to the previous version, with that version's unit, and restarts. Run
it with the current version's installer
(`/usr/local/lib/dd-worker/<current>/install.sh`): installers before 0.2.0-rc8
don't restore the unit.

Rolling back to 0.2.0-rc7 or older first hands `secrets.env` back to
`dd-worker` (0600), because those versions read the file themselves. If a host
manager owns `secrets.env`, remove its entry first: it only manages root-owned
files.

Rolling back to a version older
than 0.2.0 needs the credential copied back to `/etc/dd-worker/env` by hand
(0.2.0 moved it to `/var/lib/dd-worker/credential/`).

Versions before 0.2.0-rc2 refuse `agent_allowed_domains` and `max_cost_usd`
as unknown keys and then decline every job. Before rolling back past rc2,
remove both keys from `policy.toml`; after an upgrade to rc2, add them only
once the upgrade has succeeded.
Versions before 0.2.0-rc3 refuse `agent_browser` and `agent_deny_sockets` the
same way: remove them before rolling back past rc3.
Versions before 0.2.0-rc4 refuse `allowed_models` the same way: remove it
before rolling back past rc4.
Versions before 0.2.0-rc6 refuse `agent_postgres` the same way: remove it
before rolling back past rc6.
Versions before 0.2.0-rc9 refuse `verify_commands` the same way: remove it
before rolling back past rc9. They run verify commands unsandboxed, and only
with `allow_verify_commands = true`: leave that key out (or false) on a host
you roll back. A host that never set `verify_commands` and keeps
`allow_verify_commands = false` skips verify on every version.

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
