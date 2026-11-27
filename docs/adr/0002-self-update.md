# ADR 0002: dd-worker updates itself to the server's release (opt-in)

- **Status:** Proposed, Tier 3. No code until a maintainer approves this
  spec. A change after approval updates this file and is approved again.
- **Defect:** DEF-20261123-feature-dd-worker-updates-itself-to-the-svks
- **Scope:** the worker protocol (`hello_ok`, `hello`), the host policy,
  `install.sh`, a new root-run update step, the release workflow, and the
  control plane's Workers view. Additive everywhere: a host that does not
  opt in behaves exactly as today.

## 1. Context

Upgrading a worker today needs someone with root on the host to run
`sudo sh install.sh upgrade X.Y.Z`. The control plane already knows which
release it wants (`DEFECT_DRAINER_WORKER_RELEASE`, shown as
`target_release` in the Workers view and the MCP `list_workers` tool), but
nothing acts on it. A worker that falls behind silently loses features the
server depends on (for example, without `verify_sandbox.v1` no fix can be
verified).

Goal: when an operator changes the server's target release, every host
that opted in moves to it by itself, without a person on the host, without
cutting off a job, and with an automatic way back if the new version does
not come up.

Non-goals: updating the coding-agent CLIs, Chromium, Postgres or the OS;
editing `policy.toml`; jumping minor or major versions; a fleet rollout
plan (canary, percentages). Those can come later.

## 2. Threat model

The trust rule is the one the worker already lives by: **the server cannot
exceed what the host owner allows.**

| Party compromised | What it must not be able to do |
|---|---|
| The control plane (or its database, or an operator account) | Make a host run any binary that the release key did not sign; downgrade a host; jump a minor version; update a host that did not opt in. |
| The GitHub repository or its release assets (anyone who can push a branch, edit a release or open a PR) | Make a host run a binary the release key did not sign. |
| The worker process (the `dd-worker` user, which the agent shares) | Gain root, or install anything except a signed release the policy allows. |
| The network path | Anything beyond a failed download. |

Consequences:

1. **The server names only a version.** It never sends a URL, a hash or a
   command.
2. **The host checks a signature with a key the server does not hold.** A
   SHA-256 alone proves nothing against a compromised release, because the
   sums come from the same place as the tarball. The defect's first draft
   proposed "the release's SHA256SUMS **and** a hash the server announces";
   that is dropped in favour of a signature, which neither the server nor
   GitHub can forge (§9, alternative A).
3. **The download address, the key, the opt-in and the limits are fixed on
   the host**: compiled into the root-owned binary or read from the
   root-owned `policy.toml`.
4. **Residual risk:** whoever holds the maintainer's own GitHub account
   can push a tag and approve the signing run (§3). That is the same trust
   the maintainer already is; CloudTrail records every signature, and
   disabling the KMS key stops it at once.
5. **The worker stays unprivileged.** It can only *ask*. A separate step
   that runs as root re-checks everything itself, and treats the request as
   one untrusted string.

## 3. Release signing (AWS KMS, signed in the release workflow)

The signing key is an AWS KMS Ed25519 key that never leaves KMS. The
release workflow signs with it through GitHub OIDC, so no private key exists
on a person's machine, in GitHub secrets or in a log. This is the scheme
the same maintainer already runs for another product's agent releases.
This repository gets its **own** keys: one leak does not reach both
products.

**What is signed.** Each release gains one asset, `SHA256SUMS.sig`: one
line, the base64 of a plain Ed25519 signature (RFC 8032 PureEdDSA) over the
exact bytes of `SHA256SUMS`, which already lists every tarball and
`install.sh`. KMS key spec `ECC_NIST_EDWARDS25519`, key usage
`SIGN_VERIFY`, algorithm `ED25519_SHA_512`, `MessageType` `RAW` gives
exactly that signature, so the host checks it with Go's `ed25519.Verify`
and needs no AWS code. A `RAW` message is at most 4096 bytes; `SHA256SUMS`
is well under that, and the sign job refuses one over it.

**Keys on the host.** `internal/release.PublicKeys` is a fixed list
compiled into `dd-worker`: the **release key** (KMS, used by CI) and a
**recovery key** (a second KMS key that no role may use; it signs only a
release that rotates keys). The build ships a placeholder (all zero bytes)
until the real keys exist; a binary with only the placeholder refuses
every update (`key_placeholder`), and `go test -tags release
./internal/release` fails on a placeholder, an empty list or a duplicate.
Public keys are public: they live in this repository.

**The release workflow** (`.github/workflows/release.yml`) keeps its
trigger, a pushed tag `vX.Y.Z` on a commit that is on `main`, and its
`check`, `gates` and `build` jobs. Two changes:

1. A new **`sign`** job between `build` and `release`:
   - `environment: release`. The environment admits only tags `v*` and
     has the maintainer as required reviewer, so each signing waits for
     one tap in GitHub (approvable from a phone);
   - `permissions: id-token: write`, `contents: read`, and no secret;
   - it rebuilds the release itself from the same commit and requires its
     `SHA256SUMS` to equal the `build` job's byte for byte, so it never
     signs what another job produced;
   - it gets a GitHub OIDC token for audience `sts.amazonaws.com`,
     exchanges it with `sts:AssumeRoleWithWebIdentity` for short-lived
     credentials (session name `release-<run id>`), and calls `kms:Sign`.
     The region, role ARN and key ARN are **environment variables** of
     `release`, set in GitHub, never committed;
   - it checks the signature against the compiled `PublicKeys` before
     uploading `SHA256SUMS.sig`, so a signature hosts would refuse is
     never published;
   - only first-party actions pinned by SHA, the runner's own `aws` CLI,
     `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`.
2. **`release`** publishes `SHA256SUMS.sig` with the other assets, and
   refuses to run unless `github.workflow_ref` is this repository's
   `.github/workflows/release.yml` at the tag.

**AWS side (the maintainer's step, §13).** Region and account are not
recorded here.

- KMS key `dd-worker-release`: asymmetric, sign and verify,
  `ECC_NIST_EDWARDS25519`; key administrator the maintainer only.
- KMS key `dd-worker-recovery`: the same type; no role or user may use it.
- IAM role `dd-worker-release-signer`, web identity for the existing
  GitHub OIDC provider. Trust policy: `aud` = `sts.amazonaws.com` and
  `sub` = `repo:more-than-code/defect-drainer-worker:environment:release`
  exactly, no wildcard. One inline policy: `kms:Sign` on the release key's
  ARN only, with `kms:SigningAlgorithm` = `ED25519_SHA_512`. Maximum
  session one hour.
- Agent sessions get no AWS credential that can sign, change these keys or
  role, or assume the role; the account's existing deny policy for the
  agents' IAM user is extended to these two keys and this role.

**Who can cause a signature.** Three gates must all pass: a `v*` tag on a
commit that is on `main` (only the maintainer, or a maintainer's local
session, pushes tags; cloud sessions cannot); the `release` environment,
which admits only `v*` tags and waits for the maintainer's tap; and AWS,
which hands credentials only to a job whose OIDC `sub` names that
environment. A tag ruleset on `v*` (restrict creation, update and
deletion to repository admins) and immutable releases keep a published
release from moving. A workflow on any other branch or tag gets a
different `sub` or never reaches the environment, and AWS refuses it.
Every `Sign` call is in CloudTrail with the run id.

**Rotation and revocation.** Disabling the release key in KMS stops all
signing at once. A key is added to or removed from `PublicKeys` only by a
release signed with a key hosts already trust: after a compromise, the
maintainer grants the signer role use of the recovery key for one run,
releases a version whose list drops the old key and adds a new one, and
revokes the grant. Hosts that update to it stop trusting the old key.
If both keys are lost, hosts are updated by hand once.

## 4. Protocol (worker-protocol v1, new §23, capability `self_update.v1`)

All additive, under the existing rule that a change after the freeze is a
new capability.

- **Worker → server, `hello`:**
  - `capabilities` lists `self_update.v1` when the binary has the update
    code (whatever the policy says).
  - `self_update` (object, optional, display only):
    `{"mode": "off"|"server", "state": "idle"|"draining"|"updating"|"cooldown", "target": "<ver>", "reason": "<token>", "last": {"from","to","result","reason","at"}}`.
    `target` is the server's target as the worker last saw it, `reason`
    says why an idle or cooling-down worker is not updating to it, and
    `last` (omitted before the first update) is the root step's last
    result. `result` is `succeeded`, `rolled_back`, `refused` or `failed`;
    `reason` is a token from §6.
- **Worker → server, `status`:** the same `self_update` object, sent
  whenever the state, reason or last result changes, so the console sees
  draining and the result without waiting for a reconnect. (Added during
  implementation, 2026-11-27: the approved draft carried it in `hello`
  only.)
- **Server → worker, `hello_ok`:** `target_release` (string, optional),
  sent only to a worker that lists `self_update.v1`, and only when
  `DEFECT_DRAINER_WORKER_RELEASE` is set. The value is checked by the
  server's existing parser before it is sent.
- No new frame. A change of `DEFECT_DRAINER_WORKER_RELEASE` restarts
  `serve`, every worker reconnects, and the new `hello_ok` carries the new
  target.
- While draining for an update, the worker declines new offers with the
  existing reason `busy`, so the server sends the job elsewhere. No new
  decline reason.

## 5. Host policy (`policy.toml`)

New keys; versions before the first release with this feature refuse them
(`unknown_key`), as for every new key, and the example file says so.

```toml
# Update dd-worker to the server's target release by itself.
# "off" (the default) never updates; "server" follows the server's
# target_release within the limits below.
auto_update = "off"

# Optional: only start an update inside this local-time window (HH:MM-HH:MM,
# may wrap midnight). Unset: any time.
# auto_update_window = "02:00-05:00"

# Optional: the only proxy the download may use (http(s)://host:port).
# Nothing from the worker's environment is used.
# auto_update_https_proxy = "http://proxy.internal:3128"
```

Rules the root step enforces (the worker checks them too, only to avoid a
pointless request):

1. **Newer only.** Version order is SemVer, with one rule SemVer does not
   give: a pre-release must be `rcN` and `rcN` compares by N, so
   `0.2.0-rc9 < 0.2.0-rc10 < 0.2.0`. Any other pre-release form is refused
   (`not_allowed`). Equal or older is refused `not_newer`. A downgrade
   happens only through rollback (§7).
2. **Same major and minor.** `0.2.x` may go to any newer `0.2.y` (patch or
   RC). `0.2.x → 0.3.0` is refused `cross_minor`; that stays a manual
   `install.sh upgrade`, because a minor release may need a policy edit.
3. **Signed.** §3.

## 6. What happens on the host

### 6.1 The worker (unprivileged)

On every `hello_ok` with `target_release`, and once a minute while one is
pending:

1. Policy `auto_update = "server"`, the target is newer and allowed (§5),
   the window (if any) is open, and the version is not in cool-down.
   Otherwise nothing happens; the state says why.
2. **Drain:** state `draining`; new offers are declined `busy`. Running
   jobs finish normally (they are already bounded by `max_wall_sec`). No
   job is cut off.
3. **Request:** once idle, the worker writes the version, and nothing
   else, to `/var/lib/dd-worker/update/request` (its own directory, 0700)
   by write-and-rename, and goes to state `updating`. It keeps declining
   offers until it is restarted or the root step reports a result.
4. **Cool-down:** a version that ended `rolled_back`, `refused` or
   `failed` is not requested again for 6 hours, or until the server's
   target changes.

### 6.2 The root step (`dd-worker-update`)

Installed once by `install.sh` (§8): `dd-worker-update.path` watches the
request file and starts `dd-worker-update.service`, a root oneshot that
runs `/usr/local/bin/dd-worker update-run`. That is the **installed,
root-owned** binary, not anything the worker can write.

1. Take `flock /run/dd-worker-update.lock` without waiting (a second run
   exits).
2. Read the request: `open` with `O_NOFOLLOW`, a regular file owned by
   `dd-worker`, at most 64 bytes, one line matching
   `^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-rc[0-9]{1,4})?$`. Delete it.
   Anything else: `refused bad_request`.
3. Load `policy.toml` itself (root-owned, as today) and re-check §5
   against the installed version (`readlink /usr/local/bin/dd-worker`).
   `refused off|not_newer|cross_minor|not_allowed|outside_window`.
4. Re-check idle with `dd-worker health --idle`. Busy: `refused busy` (the
   worker asks again later). The worker could lie about being idle; that
   only cuts off its own jobs.
5. **Download** from the address compiled into the binary,
   `https://github.com/more-than-code/defect-drainer-worker/releases/download/v<version>/`:
   HTTPS on port 443 only, redirects (at most 5) only to `github.com`,
   `objects.githubusercontent.com` and `release-assets.githubusercontent.com`,
   at most 64 MiB and 120 s per file, the only proxy being
   `auto_update_https_proxy`. Order: `SHA256SUMS` and `SHA256SUMS.sig`
   first, **verify the signature**, then the one tarball for this
   platform, then check it against the signed sums.
   `failed download_failed|signature_invalid|sum_mismatch|key_placeholder`.
6. **Stage** into a root-owned 0700 directory: regular files only, no
   absolute paths, `..` or links, the file list `dist.sh` makes and no
   other. The staged `dd-worker version` must print the version
   (`failed version_mismatch`).
7. **Install** by running the *new* release's `install.sh upgrade <version>`
   (it is in the signed `SHA256SUMS`) with
   `DD_WORKER_RELEASE_BASE=file://<staging dir>`, so it re-checks the sums
   and uses the existing tested path: unpack alongside, switch the unit and
   the binary link, restart, wait for `connected (protocol` in the journal
   (60 s), roll back to the previous version if that does not appear.
8. Afterwards, `dd-worker health --wait` must pass within 60 s; otherwise
   `install.sh rollback` and restart.
9. **Result:** write
   `/var/lib/dd-worker-update/result.json`
   (`{"from","to","result","reason","at"}`, root-owned 0644, readable by
   the worker). The (new or restored) worker reads it at start and reports
   it in `hello.self_update.last`.

Unit hardening for `dd-worker-update.service`: `ProtectSystem=strict` with
`ReadWritePaths` only for `/usr/local/lib/dd-worker`, `/usr/local/bin`,
`/etc/systemd/system`, `/var/lib/dd-worker/update`, `/var/lib/dd-worker-update`
and `/run`; `PrivateTmp`, `NoNewPrivileges`,
`RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, `RuntimeMaxSec=900`,
and `StartLimitIntervalSec=1h` / `StartLimitBurst=4` so the worker cannot
make it spin. It needs D-Bus to systemd for the restart, as `install.sh`
does.

**Root helper scope:** it installs a signed dd-worker release and rolls
back to the previous one. It reads nothing else from the worker than one
version string, runs no command from the request, and never touches
`policy.toml`, `secrets.env` or the credential.

## 7. Failure and rollback

| Failure | Outcome | The host is left on |
|---|---|---|
| Bad signature, sum, tarball or version | `failed <reason>`; nothing on the host changed | old version |
| Download error or timeout | `failed download_failed` | old version |
| New version does not connect within 60 s | `rolled_back not_connected` (install.sh's existing rollback) | old version |
| Connects but `health --wait` fails | `rolled_back health` | old version |
| Rollback itself fails | `failed rollback_failed`; the worker may be down. The server's existing `worker offline` signal fires; a person runs `install.sh rollback` | unknown |
| Power loss mid-update | install.sh switches with atomic renames, so the link points at one complete version; the next boot starts it | old or new |

A manual rollback stays `sudo sh install.sh rollback`. Because the
previous version always has the update code (from the second updating
release on), its policy keys stay valid after a rollback.

## 8. `install.sh` and packaging

- `install` and `upgrade` also install `dd-worker-update.path` and
  `dd-worker-update.service` from the tarball, create
  `/var/lib/dd-worker/update` (dd-worker 0700) and
  `/var/lib/dd-worker-update` (root 0755), and enable the path unit. Both
  are harmless while `auto_update = "off"`: the root step refuses.
- `remove` disables and deletes them.
- `status` prints the auto-update mode, state and last result.
- The tarball's file list gains the two unit files; `dist.sh` and the
  release asset check follow.

## 9. Alternatives considered

- **A. Trust SHA256SUMS plus a hash the server announces (the defect's
  draft).** A bad binary would need both GitHub and the server. Rejected
  as the only check: the server becomes part of the trust path, and the
  server is the party most exposed to the internet. A signature with an
  KMS-held key needs neither. (The server-announced hash adds nothing once
  the signature is checked, so it is not sent.)
- **B. Sigstore / GitHub artifact attestations (keyless).** No key to
  hold, but the verifier needs a large dependency tree, which this module's
  dependency rule (stdlib, `coder/websocket`, `x/sys`) does not allow, and
  it trusts GitHub's identity for the workflow.
- **C. Let the host manager do it (for example Deploybot's agent, which
  already has a signed self-upgrade).** Works only on hosts that run that
  manager; dd-worker is a public, standalone module and must work without
  it.
- **D. The worker updates its own binary in place.** Needs the worker to
  write root-owned files, which breaks the whole sandbox model.
- **E. A private key held offline on the maintainer's computer, or as a
  GitHub environment secret.** The first needs a computer for every
  release; the second can leak through GitHub. KMS keeps the key
  unexportable and still signs from a phone tap.
- **F. apt repository.** Proper packaging, but a separate signing and
  hosting chain to run, and the hosts are mostly a single Pi each.

## 10. Contract diff

| Interface | Before | After |
|---|---|---|
| `hello.capabilities` | … | adds `self_update.v1` |
| `hello` | — | `self_update` object (optional) |
| `hello_ok` | — | `target_release` (optional, only to `self_update.v1` workers) |
| `policy.toml` | — | `auto_update`, `auto_update_window`, `auto_update_https_proxy` |
| Release assets | tarballs, `SHA256SUMS`, `install.sh` | adds `SHA256SUMS.sig` |
| Tarball | `dd-worker`, unit, docs | adds `dd-worker-update.path`, `dd-worker-update.service` |
| CLI | — | `dd-worker update-run` (root only), `dd-worker status` shows auto-update; `cmd/release-sign verify-compiled` (CI only, not shipped) |
| Release workflow | check → gates → build → release | check → gates → build → **sign** (environment `release`, OIDC, KMS) → release |
| `install.sh` | install, upgrade, rollback, remove, status | same verbs; install/upgrade/remove manage the update units |
| Control plane | Workers view shows version vs target | also shows auto-update mode, state and last result; MCP `list_workers` adds the same fields; `whats_new` entry. No migration expected: the hello details are stored as they are today (to be confirmed in the control-plane PR) |

## 11. Decisions

1. **Signing key:** AWS KMS, signed by the release workflow through OIDC,
   like the maintainer's other product (§3). Separate keys from that
   product's.
2. **Recovery key:** a second KMS key nobody may use until a rotation
   (recommended), so no key ever has to sit on a computer.
3. **Pre-releases:** `rcN` updates are allowed under
   `auto_update = "server"` while every release is an RC. A later release
   can add `auto_update_prerelease` if that changes.

## 12. Tests and acceptance

- Workflow: `actionlint`; a run from a branch or a non-`v*` tag cannot reach
  `release` (checked once with a throwaway run); the log holds no
  credential.
- Unit: version order (`rc9 < rc10 < final`, refusals), policy keys and
  their refusals, signature verify (good, bad, wrong key, placeholder),
  request-file parsing (size, symlink, owner, bad version), redirect and
  host allowlist, tarball staging (links, `..`, extra files), drain
  (offers declined `busy` while draining, a running job finishes), cool-down.
- Fake server: `target_release` sent only to `self_update.v1` workers;
  older workers unchanged (`hello_compat_test`).
- Container, the real path (`make worker-package-test` extended,
  `debian:bookworm` with systemd, local `file://` release signed with a
  test key): announce → drain → download → verify → install → restart →
  healthy; and each of bad signature, bad sum, download error, a release
  that never connects (rolled back), a cross-minor target and a downgrade
  target (refused, nothing changed).
- Gates: `make vet`, `make test`, `make tidy-check`,
  `make worker-package-test`; control-plane `go test` for the hub and API
  changes, plus the console checks.
- Docs in the same PRs: `docs/host-setup.md` (opting in, the window, what
  root runs), `docs/contracts/worker-protocol-v1.md` §23,
  `packaging/README-worker.md`, `policy.example.toml`.

## 13. Rollout

0. The maintainer creates the two KMS keys, the signer role and the
   `release` environment (§3) and shares the two public keys (public).
1. Worker PR: this ADR, `internal/release` with the real public keys, the
   `sign` job, the update code, units, `install.sh`, tests, docs. It
   touches `.github/`, so it merges only with the maintainer's OK.
2. Control-plane PR: `target_release` in `hello_ok`, the hello field in the
   Workers view and `list_workers`, docs, `whats_new`.
3. First release with the feature, signed in CI. Every existing
   host needs **one last manual upgrade** to it, because older binaries have
   no update code; the host owner then sets `auto_update = "server"`.
4. From the next release on, changing the server's
   `DEFECT_DRAINER_WORKER_RELEASE` is enough.
