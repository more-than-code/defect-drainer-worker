# Defect Drainer worker (`dd-worker`)

The Defect Drainer worker that runs on your machine is open source (Apache-2.0).

`dd-worker` runs Defect Drainer coding-agent jobs on a host you own. It connects
out to your Defect Drainer server (defectdrainer.com or your own), takes only the
jobs your local policy allows, runs your coding agent in a per-job sandbox, and
pushes the job's branch when an operator ships it. This repository holds the
worker, its install script and release tooling, and the protocol it speaks. The
Defect Drainer server is a separate product and is not part of this repository.

This page lists what the worker does on your machine, what it sends to the
server and what it never sends. Each point names the file that does it, so you
can check it against the code and against the binary you install.

## What it does on your machine

- **Enrols once.** `dd-worker init` reads a one-time join token (`dd_wjn_…`)
  from stdin, never from the command line. It stores the worker credential in
  `/var/lib/dd-worker/credential/env` (file 0600, directory 0700), and refuses
  to use a credential directory that is a symlink or open to other users.
  (`internal/worker/enroll.go`, `internal/worker/credential.go`,
  `internal/botagent/config/config.go`)
- **Connects out only.** It opens no listening port. It keeps one outbound
  WebSocket to the server and makes outbound HTTPS calls to the same server for
  the job spec, uploads and per-job git tokens. It only talks to an `https://`
  origin and refuses redirects. (`internal/botagent/conn/conn.go`,
  `internal/worker/http.go`)
- **Clones and pushes with per-job tokens.** For each job it asks the server for
  a short-lived git token for one repository and checks the answer names that
  repository. The server scopes the token (see
  `docs/contracts/worker-protocol-v1.md`). The worker keeps the token in memory
  and passes it to git through `GIT_CONFIG_*` environment variables, never on a
  command line or in `.git/config`. Your own `GH_TOKEN`, SSH agent and git
  credential helpers are not used. Only `https://github.com/…` repositories are
  accepted. (`internal/worker/gittoken.go`, `internal/worker/offer.go`)
- **Runs your coding agent in a per-job directory.** Claude Code is supported on
  workers today. The agent works in the job's own directory with the job's git
  worktrees. It is denied your agent and cloud credential files (`~/.claude`,
  `~/.ssh`, `~/.aws`, `~/.config` and others) and web fetch and web search,
  and it runs with a strict MCP configuration. (`agentadapter/claude/claude.go`,
  `exec/host.go`, `internal/worker/run.go`)
- **Pushes a branch only if you allow it.** Shipping needs `allow_ship = true` in
  your `/etc/dd-worker/policy.toml`, which must be owned by root. The worker
  commits only the files the operator named, as
  `Defect Drainer <dd-worker@users.noreply.defectdrainer.com>`, and pushes one
  branch. The server opens the pull request. (`internal/worker/control.go`,
  `internal/worker/policy.go`)
- **Follows your policy.** The policy sets which orgs, repositories and agents it
  accepts, the sandbox level, turn and time limits, and whether verify commands
  and shipping are allowed. Without a valid policy it declines every job.
  Verify commands run as plain host shell commands, so they are off unless you
  set `allow_verify_commands = true`. (`internal/worker/policy.go`,
  `packaging/policy.example.toml`)

## What it sends to the server

- **At enrolment:** the host name, a SHA-256 hash of `/etc/machine-id` (on macOS,
  a random ID kept in the worker's state directory), the worker version, OS and
  architecture. (`internal/worker/enroll.go`, `workerproto/frames.go`)
- **While connected:** which agents are installed, their versions and readiness,
  and *how* each is signed in (`subscription` or `api_key`, never the value);
  the git version; a summary of your policy; and which jobs are running.
  (`internal/worker/session.go`, `workerproto/frames.go`)
- **For each job:** job events and the agent's output lines; the diff of the
  job's worktree when the server asks for it; and harvest files (the agent's
  defect notes, screenshots and test results). Harvest files come only from the
  job's own directory, and only regular files created after the job started.
  (`internal/worker/sink.go`, `internal/worker/control.go`,
  `exec/harvest_files.go`)
- **Redaction:** before anything is sent, the worker replaces the exact values
  of the secrets it knows: the values in `secrets.env`, its own credential and
  the job and git tokens (also in base64 and JSON-escaped form).
  (`internal/worker/sink.go`, `internal/botagent/redact/exact.go`)

## What it never sends

- Your agent credentials: neither the credential files nor API key values.
- The values in `/etc/dd-worker/secrets.env`. They stay in the worker process,
  are given only to the agent that needs them, and are redacted from output.
  (`internal/worker/secrets.go`)
- Files from outside the job's directory and worktrees: the worker reads
  nothing else to send.

## Limits you should know

- Redaction matches exact known values. It is not a secrecy boundary: a secret
  the worker does not know about, or one the agent transforms, is not caught.
  (`internal/botagent/redact/exact.go`)
- The agent's shell is denied the credential paths above but can read other
  files on the host that the `dd-worker` user can read. Anything it prints is
  part of the job's output. Run the worker as the dedicated `dd-worker` user, as
  the systemd unit does (`packaging/dd-worker.service`: no capabilities,
  read-only home and system).

## Install

Linux on arm64 or amd64, with `git` 2.32 or newer, `bubblewrap`, `socat`,
`curl` and Claude Code at `/usr/local/bin/claude`:

```sh
curl -fsSLO https://github.com/more-than-code/defect-drainer-worker/releases/download/vX.Y.Z/install.sh
sudo sh install.sh install X.Y.Z
```

`install.sh` checks each download against the release's `SHA256SUMS` before it
changes anything. The full guide, from policy and secrets to enrolment,
upgrades and removal, is [docs/host-setup.md](docs/host-setup.md).

## Build from source

Go 1.25 or newer:

```sh
make worker          # bin/dd-worker
make test            # go test ./... -race -count=1
make worker-dist WORKER_VERSION=X.Y.Z   # release tarballs and SHA256SUMS in dist/
```

A release tag `vX.Y.Z` builds the binary with `dd-worker version` printing
`X.Y.Z`, so you can match an installed binary to its source.

## Protocol

The worker and the server speak the protocol in
[docs/contracts/worker-protocol-v1.md](docs/contracts/worker-protocol-v1.md).
The server's worker REST API is in
[docs/contracts/workers-rest-v1.md](docs/contracts/workers-rest-v1.md).

## Licence and trademarks

Apache License 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE). Code in
`internal/botagent` is derived from bot-agent, also Apache-2.0
(`internal/botagent/NOTICE`). The name "Defect Drainer" is not covered by the
licence: see [TRADEMARKS.md](TRADEMARKS.md). Contributions are welcome under
the terms in [CONTRIBUTING.md](CONTRIBUTING.md).
