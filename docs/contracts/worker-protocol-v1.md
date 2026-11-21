# Worker protocol v1

**Status:** **frozen**, 2026-11-15 (the maintainers). It's r2: r1 went through an independent contract and security review, and its 17 findings are folded in here. Frozen means: the W2 slices (W2a `cmd/dd-worker`, W2b the server hub plus REST, W2c the console) build against this file in parallel, and nobody changes it without Main Dev re-approving. A change after the freeze is a new capability (§10), never an edit to a v1 frame.

**Parent spec:** `worker-agent.md` (a control-plane design doc, not published) §3.2–3.4, D1–D8, and the answers to Q3 and Q4 (§11). Where names differ, this file supersedes the parent's §5 contract diff (for example `adapter_unavailable` replaces `runner_unavailable`). **Seam it carries:** `internal/exec` `JobSpec` / `Event` / `Control` as merged in W1a (`05ee5f7`), plus the W2 additions in §8.

**Conventions:**
- Frame keys are `snake_case`.
- Embedded `exec` types keep their Go JSON tags **exactly as they are**: `JobSpec`, `Event`, `Control` and their children are camelCase, while `Defect`, `Batch`, `RepoEntry`, `VerifyCommand`, `TestRun` and `RunStep` are snake_case. Implementers must not normalise them.
- "Must", "must not" and "may" are normative.

## 1. Transport

| Item | Rule |
|---|---|
| Endpoint | `wss://<PUBLIC_ORIGIN>/worker/v1/ws`. In the Mac dev loop, `ws://127.0.0.1:<port>/…` is allowed only when the server runs `AUTH=none` on loopback |
| Who dials | The worker, always |
| Auth | `Authorization: Bearer dd_wrk_<secret>` on upgrade; the enrol socket uses `Bearer dd_wjn_<secret>` (§3). A cookie or ingest token gets 401 |
| Redirects | The worker must refuse any 3xx |
| Route | `/worker/` is auth kind `worker` in `internal/api/routes.go`; `isAPIPath` covers it |
| First frame | `hello` (or `enroll`) within **10 s** of upgrade, else close 1008 |
| Frames | Text, UTF-8 JSON, one object per frame, **≤ 1 MiB**. A single `exec.Event` must be **≤ 768 KiB**: the worker truncates oversized string fields (§6.2) and never emits a larger event. A frame over 1 MiB gets `error {code:"frame_too_large"}`, then a close with 1009 |
| Envelope | `{"v":1,"type":"<type>", …}` |
| Sockets | One per worker. A newer authenticated socket for the same worker replaces the old (§4.5) |
| Close codes | 1000 normal; 1008 policy or auth; 1009 too big; 4000 `replaced`; 4001 `revoked`; 4002 `protocol_unsupported`; 4003 `org_mismatch` (the `hello.policy.orgs` doesn't contain the credential's org) |

**HTTPS side-channel** (§7):
- base `https://<PUBLIC_ORIGIN>/worker/v1/`;
- `Authorization: Bearer dd_wjt_<secret>`, a job-scoped token.

## 2. Decoding and validation

- **Lenient on unknown keys** in frames and embedded `exec` types.
- **Strict on required keys and types:** violations get `error {code:"bad_frame", ref_type, detail}`, and the frame is ignored. Three within 60 s close the socket with 1008.
- **Unknown `type`:** the receiver ignores it and sends `error {code:"unknown_type"}` once per type per socket.
- **Enums:** an unknown value in an optional field is ignored; in a required field it's `bad_frame`.
- **Times** are RFC 3339 UTC; **durations** are integer seconds, `*_sec`.
- **Ids** (`job_id`, `lease_id`, `request_id`, `worker_id`) must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`, so no `.` or `:`, and never `.`/`..`.
- **`sha256`** values are 64 lowercase hex characters.
- **Path-ish fields:** `Worktrees[].Repo`, `RepoEntry.Name`, `EvidenceRef.Path`, `HarvestFile.Filename` and every file in a `ship` list must be relative, with no `..` segment, no leading `/` or `\`, and no NUL, and each segment must match `^[A-Za-z0-9._-]+$`. A violation in a spec makes the worker refuse it (`bad_spec`); in an event, the server refuses it (`bad_frame`) and writes nothing. The worker also resolves every path it builds and checks it stays under that job's dir.
  - **Files inside a repo** (additive, worker 0.2.0-rc6): a `ship` control's `repos[].files` follow the app's own naming, so their segments match `^[A-Za-z0-9._+@()\[\]~-]+$` (`workerproto.ValidRepoFilePath`), for route files such as `src/routes/[id]/+page.svelte`, `src/routes/(app)/+layout.svelte` and `app/@modal/page.tsx`. They still have no empty, `.` or `..` segment, no segment starting with `-`, no leading `/`, no `\` and no NUL. The worker stages them as literal paths (`GIT_LITERAL_PATHSPECS`), so `[id]` is never a glob. A worker before rc6 refuses such a control `bad_frame`; every other path-ish field keeps the stricter rule.
- **Duplicate job ids:** `exec.Event.jobId` and `exec.Control.jobId` must equal the frame's `job_id`, else `bad_frame`.

## 3. Enrolment (one-shot socket)

1. An org admin creates a join token in Settings → Workers: `dd_wjn_…`, single use, 1 hour, bound to that org, stored as SHA-256.
2. On the host: `dd-worker init --server <origin> --join-token-stdin`.
3. The worker dials with `Bearer dd_wjn_…` and sends `{"v":1,"type":"enroll","hostname":"worker-1","machine_id_hash":"<sha256 of /etc/machine-id>","version":"0.1.0","os":"linux","arch":"arm64"}`.
4. The server, in one transaction:
   - marks the join token used;
   - creates or updates the worker (same org plus same `machine_id_hash` means the same `worker_id`);
   - revokes every previous credential of that worker and ends its live leases `worker_lost` (a re-enrolled machine has lost its state);
   - mints a credential, storing only its hash;
   - writes an audit row.

   It replies once, `{"v":1,"type":"enrolled","worker_id":"wrk_…","org_id":"org_…","credential":"dd_wrk_…"}`, and closes with 1000. A bad, used or expired token gets close 1008 with no frame.
5. The worker writes the credential atomically (temp, fsync, rename), mode 0600, before exiting 0.

## 4. Session

### 4.1 Handshake

Worker → server:

```json
{"v":1,"type":"hello","worker_id":"wrk_…","version":"0.1.0","protocol":1,"capabilities":["blobs.v1"],
 "max_jobs":1,"busy_job_ids":["bjob_…"],
 "adapters":[{"name":"claude","version":"2.1.292","credential_mode":"api_key","ready":true,"min_ok":true,"reason":""}],
 "tools":[{"name":"git","version":"2.47.1","ready":true,"reason":""},{"name":"gh","version":"2.62.0","ready":false,"reason":"signed_out"}],
 "policy":{"valid":true,"enabled":true,"orgs":["org_…"],"adapters":["claude","grok"],"allow_verify_commands":true,"allow_ship":false,
           "repos":["https://github.com/more-than-code/*"],"min_sandbox":"strict","max_turns":200,"max_wall_sec":2700}}
```

**The server must check:**
- `worker_id` equals the credential's worker, else close 1008;
- `policy.orgs` contains the credential's org, else close 4003;
- `adapters` has at most 8 entries;
- `tools`, when present, has at most 8 entries, each named `git`, `gh` or `browser` (`browser` since 0.2.0-rc3, §15), else close 1008 (`bad hello`). A hello without `tools` (a worker older than N5) is accepted;
- each `busy_job_ids` entry is a lease held **by this worker** in this org. Others are ignored, and the server sends `lease_lost` for each.

**Field rules:**
- `protocol` is the highest the worker speaks. The server answers with the one it chose, or closes 4002.
- `adapters[].ready` comes from the readiness check run **under the exact child env a job would get**. Its `reason` is one of `missing`, `too_old`, `signed_out`, `no_key`, `unsupported_os`.
- `tools` (optional, added 2026-11-17 as N5; protocol stays 1) says whether the host can clone (D1) and ship (D2). The worker checks them under the env its own clone and push get (`git.HostToolEnv`, git with hooks off), from `/`, at hello and every 5 minutes (never before an `accept`: `gh auth status` may go to the network, and the offer has a 30 s answer window):
  - `git`: `git --version` (`missing`; `too_old` under 2.32.0, raised from 2.25.0 in W3: the pinned worktree git needs `GIT_CONFIG_GLOBAL`), then a GitHub route: `git config --get-urlmatch credential.helper https://github.com` names `gh auth git-credential`, or `SSH_AUTH_SOCK` is set; else `no_git_helper`;
  - `gh`: `gh --version` (`missing`; `too_old` under 2.0.0), then `gh auth status --hostname github.com` exits 0; else `signed_out`.

  Only `name`, `version`, `ready` and `reason` are sent: never a token, an account name or gh's output. `tools` is display only; the server stores it in `workers.status_doc` and does not schedule on it.
- **`busy_job_ids` lists every job with a live agent child OR any unacked spool entry, OR an unacked `control_result`** (§6.3). A worker that restarted lists only jobs whose spool or results remain; jobs whose child died with it are reported as in §6.4.
- `policy` is informational for the server, which uses it to schedule. **The worker enforces its own policy regardless** (§6.5).

Server → worker: `{"v":1,"type":"hello_ok","protocol":1,"heartbeat_sec":20,"server_time":"…","capabilities":["blobs.v1"]}`. The server stops processing the replaced socket before it sends `hello_ok` on the new one. After `hello_ok`:
- the server re-attaches leases (§5.3);
- the worker replays its spool;
- the server redelivers unanswered controls (§6.3).

### 4.2 Liveness and status

- **Ping:** `ping {job_ids:[…]}` / `pong {}`. The worker pings every `heartbeat_sec`, and `job_ids` (at most `max_jobs` entries) renews only leases held by this worker.
- **Offline:** no frame for 60 s.
- **Status:** worker → server `status {adapters, tools, policy, max_jobs, busy_job_ids}`, sent on any change and after every readiness re-check (every 5 minutes, and before each `accept`). `tools` is optional and follows the §4.1 rules; it is refreshed on hello and by the 5-minute re-check only, so a pre-accept status carries the last result. A status that breaks the rules is answered `bad_frame` and not saved.

### 4.3 Credential rotation (no gap, no lockout)

1. Server → `rotated {credential:"dd_wrk_B"}`. The server sends it every 30 days, or on an admin's demand.
2. The worker renames its current file to `env.prev` (holding A), writes B to `env` atomically, then sends `rotate_ack {}`.
3. On `rotate_ack`, the server revokes every credential of this worker older than B.

- **On dial,** the worker tries `env`, then `env.prev`. A successful auth with a pending credential (B) counts as an implicit `rotate_ack`.
- **Re-sending a rotation** mints a new pending credential C, but **never revokes a previously issued pending one**. At most 2 are pending, and a pending credential not used within 7 days expires.
- **Hard cutoff:** the superseded credential A is revoked no later than 7 days after the first `rotated`, acked or not.
- The worker deletes `env.prev` after a successful dial with `env`.

### 4.4 Revocation

- **Server side:** an admin revoke closes the socket with 4001. The server ends the worker's live leases `worker_lost` and revokes its `dd_wjt_` job tokens. Later upgrades get 401.
- **Worker side, on 4001 or 401:**
  - it sends SIGINT, then SIGKILL after 3 s, to every agent child;
  - it deletes its credential files, its job tokens and its spool;
  - it keeps worktrees on disk, for the owner to inspect;
  - it exits non-zero without retrying.

### 4.5 Replaced sockets

- On close 4000, the worker waits at least 60 s with jitter before redialling. If it's replaced 3 times within 10 minutes, it exits non-zero: two hosts are sharing one credential, and the owner must re-enrol one of them.
- An offer is bound to the socket it was sent on. An `accept` on another socket gets `lease_lost`.

## 5. Jobs

### 5.1 Scheduling

- **Candidates:** the org's online workers that meet every one of these:
  - `policy.valid` and `policy.enabled`;
  - every repo of the app matches `policy.repos` (§6.5 canonical match);
  - outstanding offers plus busy jobs under `max_jobs`;
  - the adapter rule (Q4) holds.
- **Q4, adapter rule:**
  - the app's agent setting is **empty**: a candidate needs at least one adapter that is both ready and in `policy.adapters`. The worker picks the first ready one in `policy.adapters` order. Its default order is `claude, grok, codex, antigravity`.
  - the setting is **set**: that adapter must be ready **and** allowed by `policy.adapters`. There is no fallback.
- **Choice:** the candidate with the fewest active jobs; ties go to the earliest `last_seen`.
- **No candidate:** the job stays `queued`, `queued_reason:"no_worker"`, with a `queued_detail` naming why (for example `worker-1: claude signed_out`).
- **Declines:** a `decline` or a timeout excludes that worker for that job, and the server tries the next candidate. Once every candidate has declined, the job is `queued no_worker` with the decline reasons in `queued_detail`. It's re-tried when any worker sends `status` or `hello`.
- **Sticky:** Resume, the reviewer and shipping go to the worker that started the job. If that worker is offline or revoked, the API answers 409 `worker_unavailable`.

### 5.2 Offer, validate, accept

Server → worker:

```json
{"v":1,"type":"offer","job_id":"bjob_…","lease_id":"ls_…","lease_until":"…","adapter":"claude","model":"",
 "mode":"fresh","spec_url":"/worker/v1/jobs/bjob_…/spec","job_token":"dd_wjt_…"}
```

- **Before answering,** the worker **must** fetch and validate the spec (`GET spec_url`; the token is valid from offer time) and apply its policy (§6.5).
- It then answers **within 30 s** with `accept {job_id, lease_id}`, or with `decline {job_id, lease_id, reason}`. `reason` is one of `busy`, `adapter_unavailable`, `policy_denied`, `secret_missing`, `bad_spec` or `internal`, plus `model_not_allowed` from worker 0.2.0-rc4 (§8 item 10).
- Blob downloads, the D1 clone and cutting worktrees happen **after** `accept`.
- With no answer in 30 s, the offer is withdrawn, and a late `accept` gets `lease_lost`. The worker must not start a job until its `accept` has drawn no `lease_lost` within 5 s, or until the first `ack` for it arrives.

### 5.3 Lease

- A lease lasts 120 s. It's renewed by every `progress` frame and by every `ping` that lists the job.
- **Server restarts don't count:**
  - on boot, the server sets every live lease's `lease_until = now + 120 s` and runs no lapse sweep before then;
  - a lease counts down only while the server is up.

  A reconnect after a server restart is a re-attach (below).
- **Lapse:** the server ends the job `failed` with reason `worker_lost` and doesn't re-queue it. The operator can Rerun.
- **Re-attach:** for each valid `busy_job_ids` entry whose lease hasn't lapsed, including a job that already sent `done` and isn't acked yet, the server sends `lease {job_id, lease_id, lease_until, job_token}`, with the **same** `lease_id` and a fresh `job_token`, and the worker resumes streaming from its spool.
- **A frame for a lapsed or unknown lease** gets `error {code:"lease_lost", job_id, lease_id}`. The worker then:
  - stops that job's agent child, if any;
  - drops **only that job's** spool entries and stored control results;
  - **keeps its worktrees** for a later Rerun or `discard`.

  A `done` arriving after its lease lapsed is discarded, and the job stays `worker_lost`.

### 5.4 Job token

- `dd_wjt_` tokens are stored as SHA-256 and scoped to one job.
- **Expiry:** the current `lease_until`. A renewal extends it, and a previously issued token stays valid until its own expiry. After `done` is acked, it ends 10 minutes later.
- **Fresh tokens** arrive in `offer`, `lease` and `control` frames that need HTTPS (`resume`). The worker always uses the newest one.
- **Revocation:** a worker revocation revokes all of them (§4.4).

## 6. Streaming, control and policy

### 6.1 Progress and ack

Worker → server: `{"v":1,"type":"progress","job_id":"…","lease_id":"…","seq":42,"events":[<exec.Event>, …]}`

- `seq` is per job, starts at 1, has no gaps, and increases by 1 per frame. Batches hold at most 64 events or 1 s, split so a frame stays ≤ 1 MiB.
- **Spool:** each frame is written to the spool (mode 0600) **before** it's sent, and kept until acked.
  - A worker whose spool exceeds **200 MiB** declines new offers `busy`. If one running job alone exceeds it, the worker ends that job `done {state:"failed", line:"spool_full: …"}`.
  - The **24 h** cap applies only to entries of jobs that already ended (their `done` was spooled) and are still unacked. Those are dropped after 24 h, and the server will have marked the job `worker_lost`.
- **Server side:** the server applies a frame's events **and** updates `job_leases.last_seq` in **one transaction**, and sends `{"v":1,"type":"ack","job_id":"…","seq":42}` only after commit. Acks are cumulative.
- **Idempotent:**
  - a `seq` ≤ `last_seq` is re-acked and ignored;
  - a gap gets `error {code:"seq_gap", job_id, expected}`;
  - after any rejection of a job's frame (`seq_gap`, `blob_missing`, `bad_frame`), the server applies **no later frame for that job** until the worker resends from `expected`.
- **Last event:** a job's last event is `exec.Event{kind:"done"}`. After acking it, the server ends the lease; the worker deletes that job's spool entries and keeps the worktrees until `discard`.

### 6.2 Size limits inside events

- **Strings:** any string field in an `exec.Event` over 256 KiB is truncated by the worker to 256 KiB, and a `log` line `field truncated: <path>` is emitted. This covers the `VerificationRun` output tails, `HarvestedDefect.Note` and test-step `Actual`.
- **Images and notes:** these travel as blobs (§7). Inline `Data` is never sent remotely.

### 6.3 Control (durable, idempotent)

Server → worker:

```json
{"v":1,"type":"control","job_id":"…","lease_id":"…","request_id":"rq_…","control":<exec.Control>,"job_token":"dd_wjt_…"}
```

| `control.kind` | Params (in `exec.Control`, §8.5) | `control_result.result` |
|---|---|---|
| `cancel` | none | `{}`. Events end with `done {state:"cancelled"}` within 5 s |
| `resume` | `specUrl` (the next run's spec, fetched with the frame's `job_token`; never inline) | `{}`. A new `progress` stream continues the job's `seq` |
| `ship_preview` | none | `{"repos":[{"repo":"…","files":["…"],"message":"…"}]}` |
| `diff` | `repo` | `{"repo":"…","diff":"…","truncated":false}`, with `diff` ≤ 512 KiB |
| `ship` | `repos:[{repo, files[], message, title, body}]` | `{"prs":[<job.prs[] entry: kind:"pr", repo, branch, base, status, url, ghNumber, ghState, error>]}` |
| `discard` | none | `{}`. Worktrees and handoff are removed |

- **With `gittoken.v1` (§14), a `ship` pushes only**: the worker answers per repo `{repo, branch, base, pushed, head_sha, error}`, and the server opens or updates the PR through the GitHub API and fills `job.prs[]`. The `gh pr create` path below is the pre-`gittoken.v1` behaviour.
- **Ship is PR-only in v1:** push plus `gh pr create` from the worker, per D2. The Mac's `base_source=local` merge has no remote equivalent. A `ship` for a repo whose entry is `local` gets `ok:false, code:"bad_spec"`.

Worker → server: `{"v":1,"type":"control_result","job_id":"…","request_id":"rq_…","ok":true,"result":{…},"code":"","detail":""}`

- **Durable:** the worker persists each `control_result`, keyed by `request_id`, **before** sending it. It keeps it until `control_ack {job_id, request_id}` arrives.
- **Idempotent:** a repeated `request_id` is answered from the stored result and **never executed twice** (no second push or PR).
- **Redelivery:** after a re-attach, the server resends every control that has no `control_result`, with the same `request_id`. That includes an unanswered `cancel`.
- **Leases for controls:** shipping and discard on a finished job need a lease. The server first sends a 120 s `lease` frame for it.
- **Offline worker:** the API answers 409 `worker_unavailable` and nothing is sent.

### 6.4 Worker restart

On start, for each job dir with no live agent child:
- if its spool holds a `done`, the job is listed in `busy_job_ids` and replayed;
- otherwise, the worker appends `exec.Event{kind:"done", done:{state:"failed", line:"worker_restarted: agent child lost"}}` to the spool and lists the job.

Either way, the worktrees stay for Resume or Rerun.

### 6.5 Host policy is the worker's call (the server can't widen it)

The worker applies its `policy.toml` to **every** `JobSpec`, including each `resume` spec, before it acts:

| Spec input | Worker rule |
|---|---|
| repos (`RepoEntry.URL`) | Canonicalise first: scheme `https` or `ssh`/`git@` only; no userinfo; no `ext::`, `file:` or a leading `-`; lowercase host; strip a trailing `.git`. Then the host must match exactly, and the path is compared segment by segment, where `*` matches **one** segment. No match means `decline policy_denied` |
| `AppSettings.Sandbox` | Effective sandbox = the stricter of the spec's and `policy.min_sandbox` (order: `read-only` > `strict` > `workspace`) |
| `Caps`, `AppSettings.MaxTurns` | Clamped to `policy.max_turns` and `policy.max_wall_sec` |
| `adapter`, `model` | `adapter` must be in `policy.adapters` and ready. `model` must match `^[A-Za-z0-9._/-]{0,64}$` and must not start with `-`. With `policy.allowed_models` (0.2.0-rc4) the model must be listed exactly, else `decline model_not_allowed`; an empty model is declined for `claude` and `antigravity` and allowed for `grok` |
| `VerifyCommands` | These are **arbitrary shell chosen by org admins**. They run only if `allow_verify_commands`; otherwise the job runs without verify, logs "verify skipped by host policy", and emits `verification {ran:false}` |
| `ship` control | Only if `allow_ship`; otherwise `control_result {ok:false, code:"policy_denied"}` |
| `AppSettings.agentHosts` (§15) | Empty: the job's sandbox may reach `policy.agent_allowed_domains`. Otherwise only the hosts in both lists; each dropped host is a job-log line |
| `AppSettings.browser` (§15) | Kept only with `policy.agent_browser`, a ready browser, a complete socket scan, adapter `claude` and kind `batch_fix`; otherwise cleared, with a job-log line naming why |
| `AppSettings.jobStack` (§17) | Kept only when `browser` is kept; otherwise cleared with `previewEnv` and `previewTimeoutSec`, with the job-log line `job stack off: browser jobs only`. Kept services that need Postgres need `policy.agent_postgres` and a ready `postgres` tool, else `decline policy_denied` |
| invalid or non-root-owned `policy.toml` | `policy.valid:false`; every offer is declined `policy_denied` |

### 6.6 Secrets on the host (D5)

- **`secret_missing`** is decided **only** from `TestRunSpec.SecretEnv`, the one variable this job needs. If it's absent from `/etc/dd-worker/secrets.env`, the offer is declined `secret_missing`.
- **The deny set is the worker's own.** It takes every name in `secrets.env` except `SecretEnv` and the chosen adapter's own key, plus the spec's `StripEnv`; names that aren't valid env-var names are ignored. The server builds `StripEnv` **from the job's org only**.
- **Never sent or stored:** no secret value appears in a frame, the spool, an event or a log (`redact.Exact`).

## 7. HTTPS: spec and blobs

| Method and path | Body | Notes |
|---|---|---|
| `GET /worker/v1/jobs/{job_id}/spec` | → `exec.JobSpec` (remote shape, §8) | Valid from offer time. The same 404 is returned for another job's id or an unknown one |
| `GET /worker/v1/jobs/{job_id}/blobs/{sha256}` | → bytes | 404 unless this job's spec references that hash |
| `PUT /worker/v1/jobs/{job_id}/blobs/{sha256}` | bytes | Harvest images and notes. See the rules below |
| `POST /worker/v1/jobs/{job_id}/git-token` | `{"repo","access"}` → `{"token","expires_at","url"}` | `gittoken.v1` only. See §14 |

**Rules for `PUT` blobs:**
- **Allowed types:** `image/png`, `image/jpeg`, `image/webp`, `image/gif` and `text/plain`.
- **Checks:** the server sniffs the magic bytes (415 on a mismatch; SVG always 415) and verifies the hash (400 `hash_mismatch`).
- **Caps:** per file, intake's limit (413); per job, 200 blobs and 100 MiB (413 `job_blob_limit`). A repeat `PUT` is idempotent.
- **Storage key** includes the job (`job_blobs(job, sha256)`), so no cross-job existence oracle. The server stores the sniffed type and serves blobs to the console with `X-Content-Type-Options: nosniff` as an attachment.
- **Upload order:** an event may reference a blob only after its `PUT` returned 2xx. An event naming an unknown hash gets `error {code:"blob_missing", job_id, sha256}`. The worker re-uploads, then resends from that `seq` (§6.1).

## 8. W2 changes to the `internal/exec` types (additive)

These close the local-only assumptions in W1a's seam. The `local` transport keeps today's behaviour.

1. **Server-local fields are omitted from a remote spec:**
   - `JobSpec.Handoff` and `DefectsRoot`;
   - `Worktrees[].WorktreeAbs` and `PrimaryAbs`;
   - `Defect.Path` and `Batch.Path`.
2. **Worker-side derivation:**
   - handoff `{state}/jobs/<job_id>/handoff`;
   - clone `{state}/repos/<sha256 of canonical URL>` (D1);
   - worktrees `{state}/jobs/<job_id>/worktrees/<repo>`;
   - evidence materialised at `{state}/jobs/<job_id>/evidence/<EvidenceRef.Path>`, so `DefectsRoot` = `{state}/jobs/<job_id>/evidence`.
3. **Worktree fields in a remote spec:**
   - the **server** assigns `Worktrees[].Branch` (`dd/<job_id>/<repo>`) and sets `RepoURL` (canonical), `BaseSource:"origin"` and the base branch;
   - the **worker** resolves `BaseRef` from its clone after a fetch.

   If the base is missing, the job ends `done {state:"failed", line:"base_missing: <repo> <branch>"}`. `base_source:"local"` is refused for remote jobs (`bad_spec`).
4. **Blobs:**
   - `EvidenceRef.SHA256` is required for remote jobs;
   - `HarvestFile` gains `sha256` and `size`;
   - `HarvestedDefect` gains `noteSha256` and `noteSize` (the note can go as a blob when over 64 KiB).

   Remote events omit `data`; `local` keeps it inline.
5. **`Control`** gains `requestId`, `specUrl`, `repo` and `repos []ShipRepo{repo, files, message, title, body}` (§6.3). New kinds: `ship_preview`, `diff`, `ship`, `discard`.
6. **Adapter choice (Q4):** `JobSpec` gains `adapter` (empty means the worker picks, per §5.1) and `model`. Resume and Rerun specs carry the adapter that **started** the job.
7. **Seq:** `seq` stays at frame level only. `exec.Event` is unchanged.
8. **Remote job fixes (worker 0.2.0-rc2, additive; old peers ignore every field under §2):**
   - `JobSpec.Caps.agentMaxCostUsd` (number, optional): the per-job agent cost cap. The worker lowers it to policy `max_cost_usd` when that is smaller; with none from the server it uses the policy value, else 5. The loop counts the job's total, Resumes included.
   - `JobSpec.App.verifySkipped` (string, optional, `"host policy"`) and `verifySkippedCommands` (number, optional): set **only by the worker** at offer when policy `allow_verify_commands` is false and the spec had verify commands. A value from the server is dropped. Such a run records `verification {ran:false, skippedReason}` and its harvest has `allowResolve:false`, so no defect resolves.
   - `VerificationRun.skippedReason` (string, optional).
   - `AgentRun.stoppedBy` gains `blocked` (a `BLOCKED: <reason>` line in `NOTES.md`), `no_progress` (two iterations in a row changed nothing and left the same artifacts missing) and `budget` (the cost cap was reached). A stop of any of these still verifies, reviews and harvests, like `iterations`. The loop checks, first match wins: `exit`, `artifacts`, `blocked`, `no_progress`, `budget`, `wall`, `iterations`.
   - `AgentRun.stopReason` (string, optional): the agent's reason for `blocked`, at most 200 characters.
   - `JobSpec.App.sandbox`: for any adapter but grok the worker narrows it to `strict` (and `simulatorWrites` to false) at offer.
9. **Worker browser (worker 0.2.0-rc3, additive, `browser.v1`, §15):** `JobSpec.App` gains `browser` (bool), `previewCmd`, `previewUrl` (strings) and `agentHosts` (string list), all optional.
10. **Agent model and cost by model (worker 0.2.0-rc4, additive; old peers ignore every field under §2):**
    - `JobSpec.model` now reaches Claude as `--model <model>`, for the implementer and every review lens; an empty model passes no flag (the CLI's default). The server resolves the default (`sonnet`); the worker adds none.
    - Hello and status `policy` gain `allowed_models` (string list, at most 16, each matching the `model` pattern): policy `allowed_models`. Omitted when unset, which means any model. Display only: the worker enforces it (§6.5).
    - Decline reason `model_not_allowed`: the job's model is not in `allowed_models`. A server before rc4 accepts any non-empty reason.
    - `AgentRun.costByModel`, `Review.costByModel` and each lens's `costByModel` (object, optional): `{"<model>": {"costUsd": number, "inputTokens": number, "outputTokens": number}}`. From Claude's result `modelUsage`, keyed by `canonicalModel` when present, else the map key; `inputTokens` excludes cache reads and writes. Cost no model was reported for (grok, a prior record from before rc4, a key that isn't a valid model name) is under `"unknown"`, so the values sum to `totalCostUsd`. `AgentRun.costByModel` is cumulative across iterations, Resumes and Reruns, like `totalCostUsd`; `Review.costByModel` sums the lenses. The cost cap stays on the total.

11. **Verify a submitted ref (worker after 0.2.0-rc4, additive, `verify_submitted.v1`, §16):** `JobSpec.mode` gains `verify_submitted`; `JobSpec` gains `submittedRefs` and the worker-derived `baselineWorktrees`; `Status` gains `submittedHeads`.

## 9. Errors and job reasons

**Frame `error` codes:**
- `bad_frame`;
- `unknown_type`;
- `frame_too_large`;
- `lease_lost`;
- `seq_gap`;
- `blob_missing`;
- `not_owner`;
- `internal`.

**Job reasons:** a failed job's reason is the **normative prefix** `"<code>: "` on `Done.Line` (and `Status.Error`), as `exec` does today. Additive job JSON: `worker:{id,name}`, `queued_reason`, `queued_detail`.

| Where | Code | When |
|---|---|---|
| Start fix, Resume, Rerun, Create PRs, diff, discard | 409 `worker_unavailable` | sticky worker offline or revoked |
| `queued_reason` | `no_worker` | no candidate, or all declined (§5.1) |
| reason | `worker_lost` | lease lapsed, or worker re-enrolled or revoked |
| reason | `worker_restarted` | agent child lost in a worker restart |
| reason | `secret_missing` | `SecretEnv` absent on the host |
| reason / decline | `policy_denied` | host policy refuses |
| reason | `adapter_unavailable` | chosen adapter went not-ready |
| decline | `model_not_allowed` | the job's model is not in policy `allowed_models` (0.2.0-rc4) |
| decline | `policy_denied` | the job's stack needs Postgres and the host has no `agent_postgres` (detail `no Postgres (policy)`), or its `postgres` tool isn't ready (detail `postgres not ready: <reason>`) (§17) |
| reason | `base_missing` | base branch absent in the worker's clone |
| reason | `lfs_missing` | the repo uses Git LFS and the host has no `git-lfs` (W3, additive) |
| reason | `spool_full` | one job exceeded the spool cap |
| reason | `agent_auth` | unchanged |
| reason | `submitted_ref_missing` | `verify_submitted` (§16): the remote has no such ref, or a commit-id ref resolved to another commit |
| reason | `no_merge_base` | `verify_submitted` (§16): the submitted commit shares no history with the base |

**Policy reasons** (`dd-worker policy check`, the hello `policy` block's reason; additive in 0.2.0-rc2): `bad_agent_allowed_domains` (not exact lowercase hostnames, a wildcard, an IP address, a duplicate, or more than 16) and `bad_max_cost_usd` (not 1 to 100). A worker before 0.2.0-rc2 reports both keys as `unknown_key`.

Additive in 0.2.0-rc3 (§15): `bad_agent_browser` (not an absolute, clean path; or an existing file that isn't a regular executable owned by root, or is group- or world-writable) and `bad_agent_deny_sockets` (a path that isn't absolute and clean, `/`, a duplicate, or more than 32). A worker before 0.2.0-rc3 reports both keys as `unknown_key`.

Additive in 0.2.0-rc4: `bad_allowed_models` (an empty list, an empty or invalid name, a duplicate, or more than 16). A worker before 0.2.0-rc4 reports the key as `unknown_key`.

Additive in 0.2.0-rc6 (§17): `bad_agent_postgres` (not an absolute, clean path; or an existing directory that isn't owned by root or is group- or world-writable, or whose `initdb`, `pg_ctl` or `postgres` isn't a regular executable owned by root and writable only by root). A worker before 0.2.0-rc6 reports the key as `unknown_key`.

## 10. Capabilities

- **Names:** a capability is a lowercase dotted name with a version, for example `blobs.v1`.
- **Gating:** a gated frame or field is sent only if both `hello` lists name it. Absent means the old behaviour, and a v1 worker keeps working when the server adds one. Criterion 11 is tested with a fake server and a fake worker.
- **Baseline:** every v1 worker must advertise `blobs.v1`. Since N6, the server offers remote jobs only to workers that also advertise `gittoken.v1` (§14). Future additions (`sse_log`, `codex_adapter`, `multi_job`, …) are new capabilities. Since the worker after 0.2.0-rc4, `verify_submitted.v1` (§16) gates the `verify_submitted` mode. Since 0.2.0-rc6, `job_stack.v1` (§17) gates the job-stack spec fields.

## 11. Security and limits

**Org boundary:**
- Every server read or write of a job, spec, blob, control or lease checks: worker's org = job's org, and lease holder = this worker.
- A mismatch is `not_owner` or 404, never data.

**Tokens:**
- `dd_wjn_`, `dd_wrk_` and `dd_wjt_` are stored as SHA-256 only, and every mint is audited (A3 pattern).
- Neither side may log the `enrolled`, `rotated`, `offer`, `lease` or `control` frames' `credential` or `job_token` fields.
- Those fields never enter the spool.

**Agent children:**
- They never get the worker credential, a job token, the spool or another job's secrets.
- On top of that: agentadapter `ChildEnv` plus the deny set in §6.6.

**Limits:**
- 1000 frames per minute per socket;
- at most 3 unauthenticated sockets per IP;
- enrolment: 10 per IP per hour;
- upgrades: 30 per credential per minute;
- blob `PUT`s: 600 per job token per hour, plus §7's per-job caps.

**Per-IP rule:** "per IP" uses the trusted-proxy rule (`TRUSTED_PROXY_CIDRS`), so a client can't spoof `X-Forwarded-For`.

**Grep test:** criterion 10's grep covers the spool, `agent.log`, events and the worker's journal after a test-account run.

## 12. Acceptance additions for W2 (from this contract)

1. A `done` sent just before a socket drop is applied after the reconnect (spool replay with a re-attach).
2. A server restart of 90 s mid-job: the job re-attaches and finishes.
3. A socket drop after a `ship` push: the retry returns the stored result, and exactly one PR exists.
4. A rotation with the ack lost and a re-sent rotation: the worker can still authenticate, and the old credential is revoked within 7 days.
5. Path-traversal ids and filenames (`..`, absolute, NUL) are refused on both sides, and nothing is written outside the job dir.
6. A spec asking for a looser sandbox, a higher cap, a disallowed adapter, an `ext::` repo URL, or verify without `allow_verify_commands` is narrowed or refused as in §6.5.
7. An SVG or a mismatched-type blob is rejected 415.

## 13. Decisions at freeze (the maintainers, 2026-11-15)

1. **Adapter order** when the app's agent is empty: each host's `policy.adapters` order, default `claude, grok, codex, antigravity`.
2. **`allow_ship` defaults to `false`** in `policy.toml`: no Create PRs from a Pi until its owner opts in.

## 14. GitHub App repo access (`gittoken.v1`, N6)

Approved 2026-11-17 (`github-app.md` (a control-plane design doc, not published)). This replaces D1/D2 (host `git`/`gh` login) for remote jobs.

**Capability.** The worker lists `gittoken.v1` in `hello.capabilities`, and the server lists it in `hello_ok.capabilities` when the GitHub App is configured.
- The server offers remote jobs only to a worker with it.
- A worker whose server lacks it declines offers with `bad_spec`. The `decline` frame has no detail field, so the reason `server lacks gittoken.v1` goes to the worker's log only.

**Token endpoint.** `POST /worker/v1/jobs/{job_id}/git-token`, with `Authorization: Bearer dd_wjt_…` (the job token, §5.4). The lease must be live and the org must match.

- **Request:** `{"repo":"<repo entry name>","access":"read"|"write"}`.
- **Response 200:** `{"token":"ghs_…","expires_at":"<RFC3339>","url":"https://github.com/<owner>/<repo>"}`, with `Cache-Control: no-store`.
- **Rules:**
  - `repo` must be one of this job's repos, else 403.
  - `write` is allowed only while a `ship` control naming that repo is unanswered for this job, and never for `kind:"test_run"`, else 403.
  - The token is a GitHub App installation token narrowed to that one repo, with `contents: read|write` and `metadata: read`, for about an hour.
  - The server stores nothing.
  - 60 mints per job per hour, else 429.
- **Errors:**
  - 409 `github_app_not_configured`, `github_not_installed` or `repo_not_installed`;
  - 502 `github_unavailable`;
  - 401 when the lease isn't live.
- **Audit:** `git_token.mint` with job, repo and access, never the token.

**Worker rules.**
- **Fetching:** the worker fetches a token right before a remote git command: `read` for clone and fetch, `write` for push. It may reuse a token until 5 minutes before `expires_at`.
- **Where a token may live:** only in memory. It is added to the redactor before first use. It never appears in the spec, `meta.json`, the spool, a `control_result`, an event, a log or `.git/config`.
- **How git gets it:** through per-command `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` env, never argv. Three entries:
  - `credential.helper` = empty, which resets host helpers;
  - `http.https://github.com/.extraheader` = empty, which drops any header the host config sets for that origin;
  - `http.https://github.com/.extraheader` = `Authorization: Basic base64(x-access-token:<token>)`.
- **Also set for these commands:** `GIT_TERMINAL_PROMPT=0`, so a refused token fails instead of prompting, and `GIT_ALLOW_PROTOCOL=https`, so a host `url.*.insteadOf` rewrite to ssh or file fails instead of using another credential.
- **Where it runs:** only in the worker's own clone, never with `-C` on an agent-writable worktree, so a rewritten `.git` in the worktree can't redirect the token.
- **What else git loses:** host `GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`, `GH_ENTERPRISE_TOKEN`, `SSH_AUTH_SOCK`, `SSH_ASKPASS`, `GIT_ASKPASS` and `GIT_CONFIG_PARAMETERS` are removed from these commands' env.
- **Agent children** (agent, reviewer, verify) never receive the token.
- **Errors:** git failure text is redacted before it reaches a `control_result` or the job log. A clone, fetch or push auth failure ends the job, or fails that repo's ship entry, with reason `repo_auth: <repo>`. It is never swallowed.

**Ship result** (`gittoken.v1`):

```json
{"repos":[{"repo":"webapp","branch":"dd/bjob_…/webapp","base":"main","pushed":true,"head_sha":"<40 hex>","error":""}]}
```

The server then opens or updates the PR per repo:
- **Lookup:** `GET /repos/{o}/{r}/pulls?head=<o>:<branch>&base=<base>&state=all`, then `POST /repos/{o}/{r}/pulls` only when there is none.
- **Recording:** the result goes in `job.prs[]` (`kind:"pr"`, `repo`, `branch`, `base`, `url`, `ghNumber`, `ghState`, `status`, `error`), as before.
- **A repo with `pushed:false`** gets no PR call. It is recorded with `status:"skipped"` when its `error` is `no commits ahead of base`, else `status:"failed"` with the worker's (redacted) error, the same words as the local path.

Idempotency stays as in §6.3. A redelivered `ship` is answered from the stored result, and the server's PR lookup prevents a second PR.

**Readiness.** With `gittoken.v1` the worker's `tools` lists `git` only (reasons `missing`, `too_old`). The server still accepts `gh` entries and the reasons `signed_out`/`no_git_helper` from older workers (display only).

## 15. Worker browser (`browser.v1`, worker 0.2.0-rc3)

Additive (worker-browser spec, decisions B1–B8). A remote fix job for a UI defect can take its own after-fix screenshot on a host whose owner allows it.

**Capability.** Every rc3 worker advertises `browser.v1`: it understands the spec fields below. Whether its browser is ready is the `browser` tool, not the capability. The server sends `browser: true` only to workers that advertise `browser.v1` and report `browser` ready.

**Policy block** (hello, status; always present from rc3):

```json
"policy":{…, "allowed_domains":["registry.npmjs.org"], "max_cost_usd":5,
          "browser":"/usr/bin/chromium", "masked_sockets":["/run/example","/run/example.sock"]}
```

- `allowed_domains`: policy `agent_allowed_domains` (`[]` when unset). `max_cost_usd`: policy `max_cost_usd` (0 when unset). Display only: the worker enforces both.
- `browser`: policy `agent_browser`, `""` when unset.
- `masked_sockets`: the last socket scan, at most 128 entries; `[]` without `agent_browser`.
- An invalid policy sends `[]`, 0, `""` and `[]`.

**Tool.** With `agent_browser` set, `tools` carries `{"name":"browser","version":"154.0.8037","ready":true}`. **Only then:** a server before rc3 refuses the name (`ValidateTools`) and closes the socket after 3 bad frames, so the control plane moves to rc3 before any host sets `agent_browser`. Not-ready reasons:
- `missing`: the file is absent, or `<browser> --version` fails or prints no version;
- `unsafe`: no longer a regular executable owned by root and writable only by root;
- `scan_incomplete`: the socket scan hit a bound, so no browser job runs;
- `unsupported_os`: not Linux.

**Socket scan** (B2), at start, every 5 minutes and again when a browser job starts:
- **Roots:** `/run`, `/var/run`, `/tmp`, `/dev/shm`, symlinks resolved, duplicates dropped.
- **Walk:** never follows a symlink, never connects. It skips directories the worker owns that others can't write, and directories it can't enter.
- **Counts:** a unix socket the worker doesn't own and may write (connect needs write). Its directory is masked, or the socket itself when it sits directly in a root. A directory the worker may enter but not list is masked whole, and so is one at depth 6 below a root.
- **Bounds:** more than 20,000 entries in a root, or over 3 s in total, leaves the scan incomplete.
- **Then:** policy `agent_deny_sockets` is added, entries under a masked ancestor are dropped, and a mask that is or contains the worker's state dir is refused (logged).

**Spec fields** (`JobSpec.App`):

| Field | Rule |
|---|---|
| `browser` | See §6.5. When kept: the agent's sandbox gets unix sockets with every masked socket in `denyRead`; the handoff gets `dd-shot`; the brief, prompt and continue prompt get the screenshot rules. When cleared, the job runs as before. |
| `previewCmd`, `previewUrl` | Both or neither, else `decline bad_spec`. `previewCmd`: at most 512 bytes, one line. `previewUrl`: `http://` with host `127.0.0.1`, `localhost` or `[::1]`, a port, and at most a `/` path. The child gets `DD_PREVIEW_CMD` and `DD_PREVIEW_URL`, never an app secret. Neither empty: the brief says the app has no preview. |
| `agentHosts` | The `agent_allowed_domains` rules, else `decline bad_spec`. Intersected with the policy (§6.5). |

**`dd-shot`** (in the handoff; POSIX sh): `sh dd-shot <DEF-id> <path> [<path>…]`, at most 8 paths, each starting with `/`.
- It runs the preview in the first worktree as its own process group, waits up to 90 s for the URL, and screenshots each path with the browser (`--headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage`, a fresh profile in a short temp dir, 60 s each) into `fix-evidence/<DEF-id>/fix-NN.png`, numbered after the files already there. Then it stops the preview.
- It prints only `DD-SHOT OK <DEF-id> <file> <bytes>B <path>` or `DD-SHOT FAIL <DEF-id> <why>` lines. Exit 0 when every shot was written, 1 otherwise, 2 on bad arguments.
- **NO-SCREENSHOT** on a browser job is meant for a `dd-shot` failure (its `DD-SHOT FAIL` line quoted in `NOTES.md`) or an app with no preview. The loop releases on the marker as for any strict job, and harvest never resolves without a fresh image.

## 16. Verify a submitted ref (`verify_submitted.v1`, worker after 0.2.0-rc4)

Additive (the control plane's bring-your-own-agent spec, decision M5). A user's own agent fixes a defect outside Defect Drainer and submits a branch. A worker verifies it with the same gates as a DD fix (baseline, the app's checks, diff hygiene, the reviewer), with no coding agent, and never pushes.

**Capability.** The worker lists `verify_submitted.v1` in `hello.capabilities`. The server offers a `verify_submitted` job only to such a worker. A worker without it declines the offer `bad_spec` (`spec mode differs from the offer`), as for any mode it doesn't know.

**Offer.** The offer frame's `mode` is `fresh`. The spec's `mode` is `verify_submitted`; that pair is the only one where the two differ.

**Spec** (`kind` must be `batch_fix`), else `decline bad_spec`:

| Field | Rule |
|---|---|
| `mode` | `"verify_submitted"`. |
| `submittedRefs` | `[{"repo": "<repo>", "ref": "<ref>"}]`: exactly one entry per `worktrees[].repo`, no other repo. `ref` is a branch name or a 40- or 64-hex commit id, with the base-branch rules: `[A-Za-z0-9._/-]{1,200}`, not starting with `-`, no `..`, `//`, trailing `/` or `.lock`. Any other mode carries no `submittedRefs`. |
| `worktrees[].branch` | `""`: the worktrees are detached. A `dd/…` branch is refused. |
| `diffBase`, `baseline`, `priorAgentRun`, `resumeSession` | Absent: the worker measures them. |
| `baselineWorktrees` | Never sent by a server, in any mode. Derived by the worker (§8.2 style): detached worktrees at each base, `{state}/jobs/<job_id>/baseline-worktrees/<repo>`. |
| `app.browser` | Ignored: no screenshots are taken. Fix evidence comes from the server. |

**Setup**, per repo, before any check:
1. The base is fetched and `BaseRef` resolved as for any remote job (§8.3, §14).
2. The submitted ref is fetched with the job's **read** token (`git fetch --no-tags origin -- <ref>`) and pinned to the commit it names. A ref the remote lacks, or a commit-id ref that resolves to another commit, fails the job `submitted_ref_missing: <repo> <ref>`. An auth refusal is `repo_auth`.
3. A detached worktree is cut at that commit (`worktrees/<repo>`), and another at `BaseRef` (`baseline-worktrees/<repo>`).
4. The diff base is the merge-base of the commit and `BaseRef`. If there is none, the job fails `no_merge_base: <repo> <ref>`.

A failure at any step ends the run `done {state:"failed", line:"<reason>: …"}` before the baseline. No token with write access is asked for.

**Run.**
- **No coding agent:** no prompt is built and no implementer starts. There is no `agent_run` event. The reviewer is the only agent spend, with the job's `adapter` and `model`.
- **Events, in order:**
  - `status` phases `handoff`, `prepared`, `ready` (the adapter's login is checked, for the reviewer);
  - `baseline`, from the base worktrees;
  - `status running` with `diffBase` (the merge-bases) and **`submittedHeads`** (`{"<repo>": "<commit verified>"}`), so the server can tell whether the branch moved before it opens a PR;
  - `status loop_ended`, then `status agent_exit` with `failed` false;
  - `verification` (with `diffHygiene` from the merge-base to the commit), `review` and `harvest`, as for a batch fix;
  - `done {state:"completed", line:"verify complete — submitted refs checked; nothing pushed"}`, then `status exited`.
- **Verdict:** the rules are unchanged (§8 item 8). `harvest.defects` carries no worker images; the server applies its own evidence rule.

**After the run.** The job never ships, resumes or reruns. `ship` and `resume` controls fail `bad_spec`, and a rerun offer is declined `bad_spec`. `diff`, `ship_preview` and `discard` work as usual; `discard` also removes the base worktrees.

## 17. Job stack (`job_stack.v1`, worker 0.2.0-rc6)

Additive (the control plane's job-stack spec, decisions S1–S9). An app defines its job stack once: up to 4 services and an optional throwaway Postgres. A browser job (§15) boots it inside the agent's sandbox with the `dd-stack` helper, for the agent to try the app and for `dd-shot`'s after-fix screenshot.

**Capability.** Every rc6 worker advertises `job_stack.v1`: it understands the spec fields below. Whether Postgres is ready is the `postgres` tool. The server sends services only to workers that advertise the capability; one whose services need Postgres goes only to a worker whose policy block has `postgres` set and whose `postgres` tool is ready. An older worker would ignore the fields, so it must not be offered such a job.

**Policy block** (hello, status): `"postgres": "/usr/lib/postgresql/15/bin"`, policy `agent_postgres`. Omitted when unset or when the policy is invalid.

**Tool.** With `agent_postgres` set, `tools` carries `{"name":"postgres","version":"15.10","ready":true}` (the version from `<dir>/postgres --version`). **Only then:** a server before rc6 refuses the name (`ValidateTools`), so the control plane moves to rc6 before any host sets `agent_postgres`. Not-ready reasons:
- `missing`: the directory or one of `initdb`, `pg_ctl`, `postgres` is absent, or `--version` fails or prints no version;
- `unsafe`: the directory or a binary is no longer root's alone;
- `unsupported_os`: not Linux.

**Spec fields** (`JobSpec.App`), else `decline bad_spec`:

| Field | Rule |
|---|---|
| `jobStack` | At most 4, in start order. Each: `name` `[a-z][a-z0-9-]{0,31}`, unique; `repo` one of the job's `worktrees[].repo`; `install`, `migrate` (optional), `seed` (at most 4, none blank) and `start` (required), each one line of at most 500 bytes; `port` 1024–65535, unique, not the `previewUrl` port; `health` a path starting with `/` (`[A-Za-z0-9/._~%?=&-]`, at most 200 bytes); `env` (below); `needsPostgres`. |
| `env`, `previewEnv` | At most 32 entries. Names pass `ValidEnvName`; values are one line of at most 1024 bytes. Any `{token}` of letters, digits and `._:-` must be `{random}`, `{services.<name>.url}` of a listed service, `{postgres.url}` when a service has `needsPostgres`, or `{signin.url}`, `{signin.email}`, `{signin.password}` with a `jobStackSignIn`; anything else, such as `{secret:NAME}`, is refused. |
| `previewTimeoutSec` | 0 (meaning 90) or 30–300. The whole stack's bound and `dd-shot`'s wait for the preview URL. |
| `jobStackSignIn` | Optional Stack sign-in (S10): `{url, email, password, method, form, http, successPath}`. Needs `jobStack` and `previewUrl`. `url` and `successPath` follow the `health` path rule. `email`: one line, 1–254 bytes. `password`: one line, 8–256 bytes (the app's seed user, never a real credential). `method` `"form"` needs `form: {emailSelector, passwordSelector, submitSelector}` (CSS, one line, at most 200 bytes each) and no `http`; `"http"` needs `http: {method, path, bodyTemplate}` (`POST`, `PUT` or `PATCH`; `path` as `url`; a JSON body of at most 2048 bytes using `{random}`, `{services.<name>.url}` and `{signin.*}` only) and no `form`. |

`previewEnv` and `previewTimeoutSec` are checked without services too, and then ignored: a job without services runs as before, `dd-shot` byte for byte.

**Placeholders.** `{services.<name>.url}` is `http://127.0.0.1:<port>`. `{postgres.url}` is `postgresql://postgres@127.0.0.1:<port>/postgres` on a free loopback port. `{random}` is 32 random bytes in hex, one value per job run, the same in every entry, so a service and the preview can share it. `{signin.url}` is the app URL's origin plus `jobStackSignIn.url`; `{signin.email}` and `{signin.password}` are the seed user's. The worker removes the `{random}` value and the sign-in password from every event it sends (log lines, notes, diff hygiene, review and verification text) and from `agent.log`, as `[redacted]`, and from the `diff` control's result.

**Diff check** (S10). After the agent, a worktree diff (added lines, or an untracked file) that holds the password or the `{random}` value sets `diffHygiene.stackSecret: true` with a job-log warning, and the run's `harvest.allowResolve` is false. A `ship` control whose diff adds the password is refused `policy_denied`. A value already at the base (an app's own seed file) doesn't count.

**`dd-stack`** (in the handoff next to `dd-shot`; POSIX sh): `sh dd-stack run -- <cmd> [<arg>…]`, in one sandboxed call.
1. If a service needs Postgres: `initdb` in a fresh directory under `$TMPDIR`, trust auth, then `pg_ctl start` on a random loopback port (no unix socket).
2. Each service in order, in its worktree with its env: `install`, `migrate`, each seed, then `start` as its own process group, waiting up to 120 s for `http://127.0.0.1:<port><health>` to answer 2xx. The whole stack must be up within `previewTimeoutSec`.
3. `<cmd>` runs with `previewEnv`, not the services' env.
4. Everything stops on any exit, Postgres included, and the temp directory is removed.

With a `jobStackSignIn` it also exports `DD_SIGNIN_URL`, `DD_SIGNIN_EMAIL` and `DD_SIGNIN_PASSWORD` to every service step and to `<cmd>`.

It prints only these lines, never an env value: `DD-STACK UP postgres 127.0.0.1:<port>`, `DD-STACK OK <service> <step> <secs>s`, `DD-STACK UP <service> http://127.0.0.1:<port>`, `DD-STACK RUN`, `DD-STACK FAIL <service> <step> log=.dd-stack/<file>` followed by that log's last 20 lines, and `DD-STACK DOWN`. Steps are `install`, `migrate`, `seed-<n>`, `start`, `health` (and `initdb`, `start` for `postgres`). Exit: `<cmd>`'s status once the stack is up, 125 when the stack failed, 2 on bad arguments. Logs stay in `.dd-stack/` in the handoff.

**`dd-shot`** with services: the preview runs as `dd-stack run -- sh -c <previewCmd>`, and `dd-shot` waits `previewTimeoutSec` for the URL instead of 90 s. A failed stack prints `DD-SHOT FAIL <DEF-id> stack <service> <step> log=.dd-stack/<file>` and that log's last 20 lines. Arguments, output and exit codes are otherwise §15's.

**Stack sign-in in `dd-shot`** (S10). Once the app answers, `dd-shot` runs the worker's own binary, `dd-worker shot` (a DevTools-protocol client built into the worker; nothing is installed at run time), with the sign-in rendered into `dd-shot-signin.json` (mode 0600) in the handoff. It starts the host browser headless with a DevTools port on loopback, then:
- **form:** opens `{signin.url}`, waits up to 15 s for the three selectors, sets the email and password the way typing would (the native value setter, then `input` and `change` events), clicks submit and waits up to 15 s to leave the sign-in page;
- **http:** opens `{signin.url}` and calls `fetch(path, {method, body, credentials: "include"})` from that page, so the session cookie lands in the browser; it needs a 2xx;
- **success check,** when `successPath` is set: the path must load as itself (not redirected) with a status below 400.

It prints `DD-SHOT SIGNIN OK <form|http>`, then takes each screenshot at 1280×800 after the page's network settles, with §15's lines. A failed sign-in prints `DD-SHOT FAIL <DEF-id> signin <reason>` (`no-field <email|password|submit>`, `still-on-signin`, `http <status>`, `redirected <path>`, `open …`, `browser …`), takes no screenshot and exits 1. Neither prints a credential.

**Brief.** A `## Stack` section lists each service's name, repo, steps, URL and health path, and Postgres when used (never an env value), with how to run `dd-stack` and the rule that setup files never go in a worktree. With a Stack sign-in it names the test user's email and the `DD_SIGNIN_*` env, never the password, and the Screenshots section says `dd-shot` signs in first.
