# Workers REST v1 (console ↔ serve)

**Status:** Main Dev's contract for W2b (server) and W2c (console), 2026-11-15. It completes [`worker-protocol-v1.md`](worker-protocol-v1.md) (the worker ↔ server wire, frozen) on the console side. W2b implements these routes; W2c builds against them with mocked responses until W2b merges. A change goes through Main Dev.

**Conventions:**
- Routes come from the policy table in `internal/api/routes.go` as `authSession`/`capMember`, with the org role checked in the handler, as A3's orgs and tokens routes do.
- Errors use today's `{"error":"<code>"}` shape.
- Every write is audited (`internal/audit`).
- Ids are opaque strings.
- Times are RFC 3339 UTC.

## 1. Workers (Settings → Workers)

| Method and path | Role | Body | Response |
|---|---|---|---|
| `GET /api/orgs/{org}/workers` | member (read) | — | `200 {"workers":[Worker]}` |
| `POST /api/orgs/{org}/workers/join-tokens` | admin | `{}` | `201 {"token":"dd_wjn_…","expires_at":"…"}`. The token is **shown once** and never returned again |
| `PATCH /api/orgs/{org}/workers/{id}` | admin | `{"name":"worker-1 (kitchen)"}` (1–60 chars) | `200 {"worker":Worker}` |
| `POST /api/orgs/{org}/workers/{id}/rotate` | admin | `{}` | `202 {"worker":Worker}`. The server sends `rotated` when the worker is online, or on its next `hello` |
| `POST /api/orgs/{org}/workers/{id}/revoke` | admin | `{}` | `200 {"worker":Worker}`. The socket is closed with 4001, its leases end `worker_lost`, and its job tokens are revoked. This can't be undone; the owner re-enrols the host |

```json
Worker = {
  "id": "wrk_…", "org_id": "org_…", "name": "worker-1", "hostname": "worker-1",
  "version": "0.1.0", "os": "linux", "arch": "arm64",
  "online": true, "last_seen": "…", "created_at": "…", "revoked_at": null,
  "max_jobs": 1, "active_jobs": 0,
  "adapters": [{"name":"claude","version":"2.1.292","credential_mode":"api_key","ready":true,"min_ok":true,"reason":""}],
  "tools": [{"name":"git","version":"2.47.1","ready":true},{"name":"gh","version":"2.62.0","ready":false,"reason":"signed_out"}],
  "capabilities": ["blobs.v1","gittoken.v1"],
  "policy": {"valid":true,"enabled":true,"adapters":["claude"],"allow_verify_commands":true,"allow_ship":false,"repos":["https://github.com/more-than-code/*"],"min_sandbox":"strict"},
  "rotation_pending": false
}
```

**Rules:**
- **Who sees what:** members see the list read-only; admins and owners see the actions. A worker of another org is 404.
- **Never exposed:** `machine_id_hash`, credential hashes and job tokens.
- **`tools`** (added 2026-11-17, N5): the git and gh readiness from the worker's last `hello` or `status` ([worker protocol](worker-protocol-v1.md) §4.1); `reason` is one of `missing`, `too_old`, `signed_out`, `no_git_helper` and is left out when empty. **Omitted** when the worker never reported tools (an older `dd-worker`): the console shows "tools unknown". Display only; scheduling ignores it.
- **`policy`** is the worker's last reported policy for display, without `orgs` (another org's ids never reach this org's members). Added 2026-11-21 (WP-a): `max_turns` and `max_wall_sec` (ints; 0 means not reported: never connected, or an invalid policy). Added 2026-11-22 (WP-b, worker 0.2.0-rc6): `hash` (the sha256 of the host's `policy.toml`, 64 lowercase hex, shown as the policy version) and `managed_url` (where a host manager such as Deploybot edits the file: https only, at most 512 bytes; serve drops any other value). Both are **omitted** when the worker didn't report them (before 0.2.0-rc6, or `managed_url` unset); the console then says "not reported by this worker version" or "Edited on the host".
- **`capabilities`** (added 2026-11-17, N6a): the capabilities from the worker's last `hello` ([worker protocol](worker-protocol-v1.md) §10, §14), sorted. **Omitted** until the worker says hello to a server that records them. A list without `gittoken.v1` means the worker gets no offers: the console shows "update dd-worker". A list without `verify_submitted.v1` (a worker after 0.2.0-rc4) means the worker is never offered a job that verifies a submitted ref ([worker protocol](worker-protocol-v1.md) §16).
- **Errors:**
  - 403 `forbidden` for a member writing;
  - 404 `not_found`;
  - 409 `worker_revoked` for rotate or revoke on a revoked worker;
  - 429 `rate_limited` (join tokens: 10 per org per hour).

## 2. App agent setting (worker spec Q4)

`GET/PATCH /api/apps/{id}` gain an optional `agent` object (admin to write, as other host-affecting fields):

```json
"agent": {"adapter": "", "model": ""}
```

- `adapter` is `""` (any ready adapter), `claude`, `grok`, `codex` or `antigravity`. Any other value is 400 `invalid_agent`.
- `model` is `""` or matches `^[A-Za-z0-9._/-]{0,64}$` and doesn't start with `-`. Otherwise it's 400 `invalid_agent`.
- Absent in a PATCH means unchanged. A GET always returns the object, with `""` defaults.
- **Storage:** two nullable text columns on `apps` in migration `0004_workers` (W2b).
- **The local transport ignores it** (the host's `DEFECT_DRAINER_AGENT_RUNNER` decides on the Mac). The remote scheduler uses it (protocol §5.1).

## 3. Job JSON additions (additive)

```json
"worker": {"id": "wrk_…", "name": "worker-1"},
"queued_reason": "no_worker",
"queued_detail": "worker-1: claude signed_out; worker-2: offline"
```

- `worker` is absent for a local job and set once a remote job is accepted.
- `queued_reason` and `queued_detail` are set only while the job is `queued` for want of a worker.
- A failed remote job's reason is the `"<code>: "` prefix of its error line (protocol §9), shown by the console as today.

## 4. Errors on existing actions

| Action | New error | When |
|---|---|---|
| Start fix, Resume, Rerun, Create PRs, ship-preview, diff, Delete (worktrees) | 409 `worker_unavailable` | the job's sticky worker is offline or revoked |
| Start fix, Rerun | none; the job is accepted and queued (`queued_reason:"no_worker"`) | no worker can take it now; a worker without `gittoken.v1` is listed in `queued_detail` as `update dd-worker (no gittoken.v1)` |
| Start fix, Resume, Rerun (remote runner) | 409 `github_app_not_configured`, `github_not_installed`, `repo_not_github`, `repo_not_installed`, `github_unavailable` (N6a, `github-app.md` (a control-plane design doc, not published) §3.4) | no App on this serve; the org has no active installation; a repo is not a `https://github.com/<owner>/<repo>` URL; the installation cannot reach a repo (checked by minting a read token); GitHub did not answer |
| Refresh PRs (remote job) | none: it now works through the GitHub API (it was 409) | — |

## 5. Console (W2c) surfaces

- **Settings → Workers** (admins: actions; members: read-only):
  - the table: name, status (online/offline/revoked), version, adapters with ready badges and the not-ready reason, repo tools (a git and a gh badge with the not-ready reason and its fix in the tooltip, "tools unknown" for an older worker, "ship off" when the worker reported a valid policy with `allow_ship` false; a never-connected worker shows no "ship off"), active jobs, last seen;
  - "Add worker": create a join token, show it once with copy and the `dd-worker init` one-liner;
  - rename, rotate, revoke (with a confirm).
- **App settings:** the "Coding agent" select (Any ready / Claude / Grok / Codex / Antigravity) plus an optional model field.
- **Settings → GitHub** (`/settings/github`, N6a): the install button (admins), the account, state badge, last verified time and repo access; Check, "Manage repos on GitHub", Disconnect (with a confirm); "Install again" when `gone`. The callback's `?github=installed` / `?github_error=` shows once. In `AUTH=oidc` App settings marks each GitHub repo row "App can reach", "App cannot reach" or "not GitHub".
- **Workers:** an "update dd-worker" badge on a worker whose `capabilities` lack `gittoken.v1`.
- **Job drawer:** "Worker: worker-1" for remote jobs, and for queued jobs the `queued_detail`. A 409 `worker_unavailable` shows "The worker that ran this job is offline or revoked".
- No SSE in W2c: the drawer keeps today's 1.5 s poll (SSE is a later capability).

## 6. GitHub App (Settings → GitHub; N6a, `github-app.md` (a control-plane design doc, not published) §3.3)

| Method and path | Role | Body | Response |
|---|---|---|---|
| `GET /api/orgs/{org}/github` | member (read) | — | `200 GitHubStatus` |
| `POST /api/orgs/{org}/github/install` | admin | `{}` | `200 {"url":"https://github.com/apps/<slug>/installations/new?state=…"}`. The state is single-use, lives 10 minutes, is bound to this org and user, and is held in serve's memory as a SHA-256 only (a restart drops it) |
| `POST /api/orgs/{org}/github/installations/{id}/check` | admin | `{}` | `200 GitHubStatus` after re-reading that installation: its `state` becomes `active`, `suspended` or `gone`; no other installation is touched (GH-a, 2026-11-22) |
| `DELETE /api/orgs/{org}/github/installations/{id}` | admin | — | `200 {"ok":true}`. Audit `github.disconnect` with the installation id. The App stays installed on GitHub (GH-a) |
| `POST /api/orgs/{org}/github/check` | admin | `{}` | **Deprecated** (GH-a): the org's only installation's check; 409 `github_installation_ambiguous` when the org has two or more |
| `DELETE /api/orgs/{org}/github` | admin | — | **Deprecated** (GH-a): the org's only installation's Disconnect; 409 `github_installation_ambiguous` when the org has two or more |

```json
GitHubStatus = {
  "configured": true, "appSlug": "defect-drainer",
  "installations": [{"installation": Installation, "access": {"all": false, "repos": ["acme/web"]}, "accessError": ""}],
  "installation": {"orgId":"org_…","installationId":12345678,"accountLogin":"acme","accountType":"Organization",
                   "installedBy":"usr_…","createdAt":"…","verifiedAt":"…","state":"active"},
  "access": {"all": false, "repos": ["acme/web"]},
  "accessError": ""
}
```

**Rules:**
- **Several installations per org** (GH-a, 2026-11-22): `installations` lists every one, oldest first, each with its own `access` and `accessError`; an org holds at most one per GitHub account. `Installation` is the object shown under `installation`, plus `accountId` (GitHub's account id, absent for rows saved before GH-a). The top-level `installation`, `access` and `accessError` are **deprecated** for one release: the first installation's fields.
- `configured:false` (no App env on this serve): `installations` is `[]`, `installation`/`access` are null, install is 409 `github_app_not_configured`.
- `installation` is null when the org has none. `access` is read live from GitHub (`GET /installation/repositories`, capped at 1000) only for an `active` installation; when GitHub fails it is null and `accessError` holds the code.
- No token of any kind is ever returned or stored.
- **The setup callback** is `GET /auth/github/setup` (`saas-auth.md` (a control-plane design doc, not published) §3): success is `303 /settings/github?github=installed`. A valid state with an `installation_id` but no `code` (the App was already installed) is `303` to GitHub's `/login/oauth/authorize?client_id&state` with a fresh state bound to that installation; the authorize callback carries only `code` and `state`; a refusal is a short HTML page with the status below and a link to `/settings/github?github_error=<code>`:
  - 400 `github_state` (unknown, used, expired, or another user's state), `github_no_installation` (no `installation_id`, e.g. an owner must approve), `github_code` (a bad OAuth code, or the authorize step declined), `github_state` also when the authorize callback names a different `installation_id`;
  - 403 `forbidden` (no longer admin), `github_installation_not_yours` (the id is not in the user's `GET /user/installations`; audited, nothing saved);
  - 409 `github_app_not_configured`, `github_not_installed`, `github_installation_taken` (another DD org holds that installation);
  - 502 `github_unavailable`.
- **Errors** on the org routes: 403 `forbidden` for a member writing; 404 `github_not_installed` for check or disconnect with no installation, or an `{id}` this org doesn't hold (another org's included); 409 `github_installation_ambiguous` (the deprecated org-level check or disconnect with two or more installations); 409 `github_app_not_configured`; 502 `github_unavailable`.
- **Which installation a job uses** (GH-a): the org's active installation for the repo's owner (the GitHub account, ignoring case). Starting a remote fix or Resume on a repo whose owner has none is 409 `github_not_installed_for_owner`, its reason naming the owner; the worker's git token request is still refused with `github_not_installed` on the wire.
- **Branch listing** (`POST /api/git/branches`) in `AUTH=oidc` uses the App for `https://github.com/<owner>/<repo>`, default branch first: 400 `repo_not_installed` when none of the caller's orgs' active installations reaches it, 400 `repo_not_github` for other URLs, 400 `github_app_not_configured` without an App, 502 `github_unavailable`. `AUTH=none` still runs git.

## 7. Worker browser (worker 0.2.0-rc3, additive)

After-fix screenshots on workers (worker-protocol-v1 §15). The server never asks a worker for more than its policy allows: the worker applies its own `policy.toml` to every field below.

### 7.1 App fields (GET/POST/PATCH /api/apps, /api/apps/{id})

Four new fields. GET always returns them, with defaults for an app that never set them.

| Field | Type | Default | Rule (400 code) |
|---|---|---|---|
| `worker_screenshots` | bool | `false` | Must be a JSON bool (`invalid_worker_screenshots`). On needs both `preview_cmd` and `preview_url` non-empty (`preview_required`), checked on the resulting row, so clearing a preview field while on is refused too. |
| `preview_cmd` | string | `""` | Must be a string. Trimmed; at most 500 bytes; no control characters (newline, CR, NUL, tab…) (`invalid_preview_cmd`). |
| `preview_url` | string | `""` | Must be a string. Trimmed; `""` or a base URL: `http` only, host exactly `127.0.0.1` or `localhost`, a port 1–65535 written canonically, path empty or `/`, no user info, query or fragment, at most 200 bytes (`invalid_preview_url`). No `[::1]`: the server stays stricter than the worker. |
| `agent_hosts` | string[] | `[]` | Must be an array of strings, or `null` (= `[]`) (`invalid_agent_hosts`). Each entry trimmed and lowercased; empties and duplicates dropped; then at most 16 hostnames of two or more labels, each `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, at most 253 characters, last label not all digits (no wildcard, IP or port) (`invalid_agent_hosts`). Empty means each worker's own `agent_allowed_domains`. |

A refused PATCH changes nothing. PATCH stays admin/owner; POST as today.

### 7.2 Jobs

- **Job JSON:**
  - `without_screenshots` (bool, persisted, omitted when false);
  - `needs_browser` (bool, computed on reads, omitted when false): true on a queued remote job only while just a browser-ready worker may take it.
- **Scheduling:**
  - A job needs a browser when all of these hold:
    - its app has `worker_screenshots` on;
    - the app's coding agent `adapter` is `"claude"` (once the job ran: the adapter that started it). `""` (Any ready) and any other adapter do **not** count, and such a job runs code-only, logging `screenshots need the claude agent: this job runs code-only, with no after-fix screenshots` once;
    - it isn't `without_screenshots`;
    - it isn't a test run.
  - Such a job is offered only to a worker with capability `browser.v1`, a policy `browser` that isn't empty, and a ready `browser` tool.
  - Per-worker `queued_detail` reasons, checked in this order after the adapter rule:
    1. `update dd-worker (no browser.v1)`
    2. `no browser (policy)`
    3. `browser not ready` or `browser not ready: <tool reason>` (`missing` when the tool is absent)
- **Spec (`JobSpec.App`):**
  - `browser` is true exactly when the job needs a browser;
  - `previewCmd`/`previewUrl` are sent only with `browser`;
  - `agentHosts` is sent whenever the app lists hosts.

### 7.3 Run without screenshots

`POST /api/batch-jobs/{id}/run-without-screenshots`

- **Access:** members (session), console host only.
- **Request:** body ignored (send `{}`).
- **Success:** **200** `{"job": <job>}`. The job stays `queued` with `without_screenshots: true`, its queued reason and detail are cleared, a log line is added, and scheduling runs again. It then runs code-only on any worker.
- **409** `{"error":"not_waiting_for_browser"}` unless the job is queued, remote, `queued_reason: "no_worker"` and needs a browser.
- **404** for an unknown or foreign job; **400** `invalid id`.
- **Audit:** `batch_job.run_without_screenshots` for the success (`ok`) and for 409 refusals (`denied:not_waiting_for_browser`).

### 7.4 Workers (`GET /api/orgs/{org}/workers`)

`policy` gains `allowed_domains` (string[]), `max_cost_usd` (int, 0 = unset), `browser` (string, the `agent_browser` path, `""` = off) and `masked_sockets` (string[]). These are `[]`/`0`/`""` from a worker before 0.2.0-rc3. `tools[]` may now include `browser`.

From worker 0.2.0-rc4, `policy` may also carry `allowed_models` (string[], additive): the models the worker runs, matched exactly and case-sensitively (`sonnet` does not admit `claude-sonnet-…`). Absent means any model. A worker before 0.2.0-rc4 never sends it.
