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
- **`capabilities`** (added 2026-11-17, N6a): the capabilities from the worker's last `hello` ([worker protocol](worker-protocol-v1.md) §10, §14), sorted. **Omitted** until the worker says hello to a server that records them. A list without `gittoken.v1` means the worker gets no offers: the console shows "update dd-worker".
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
| `POST /api/orgs/{org}/github/check` | admin | `{}` | `200 GitHubStatus` after re-reading the installation: `state` becomes `active`, `suspended` or `gone` |
| `DELETE /api/orgs/{org}/github` | admin | — | `200 {"ok":true}`. Audit `github.disconnect`. The App stays installed on GitHub |

```json
GitHubStatus = {
  "configured": true, "appSlug": "defect-drainer",
  "installation": {"orgId":"org_…","installationId":12345678,"accountLogin":"acme","accountType":"Organization",
                   "installedBy":"usr_…","createdAt":"…","verifiedAt":"…","state":"active"},
  "access": {"all": false, "repos": ["acme/web"]},
  "accessError": ""
}
```

**Rules:**
- `configured:false` (no App env on this serve): `installation`/`access` are null, install is 409 `github_app_not_configured`.
- `installation` is null when the org has none. `access` is read live from GitHub (`GET /installation/repositories`, capped at 1000) only for an `active` installation; when GitHub fails it is null and `accessError` holds the code.
- No token of any kind is ever returned or stored.
- **The setup callback** is `GET /auth/github/setup` (`saas-auth.md` (a control-plane design doc, not published) §3): success is `303 /settings/github?github=installed`. A valid state with an `installation_id` but no `code` (the App was already installed) is `303` to GitHub's `/login/oauth/authorize?client_id&state` with a fresh state bound to that installation; the authorize callback carries only `code` and `state`; a refusal is a short HTML page with the status below and a link to `/settings/github?github_error=<code>`:
  - 400 `github_state` (unknown, used, expired, or another user's state), `github_no_installation` (no `installation_id`, e.g. an owner must approve), `github_code` (a bad OAuth code, or the authorize step declined), `github_state` also when the authorize callback names a different `installation_id`;
  - 403 `forbidden` (no longer admin), `github_installation_not_yours` (the id is not in the user's `GET /user/installations`; audited, nothing saved);
  - 409 `github_app_not_configured`, `github_not_installed`, `github_installation_taken` (another DD org holds that installation);
  - 502 `github_unavailable`.
- **Errors** on the org routes: 403 `forbidden` for a member writing; 404 `github_not_installed` for check or disconnect with no installation; 409 `github_app_not_configured`; 502 `github_unavailable`.
- **Branch listing** (`POST /api/git/branches`) in `AUTH=oidc` uses the App for `https://github.com/<owner>/<repo>`, default branch first: 400 `repo_not_installed` when none of the caller's orgs' active installations reaches it, 400 `repo_not_github` for other URLs, 400 `github_app_not_configured` without an App, 502 `github_unavailable`. `AUTH=none` still runs git.
