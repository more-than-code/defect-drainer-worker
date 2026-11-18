package workerproto

// GitHub App repo access (§14, N6). This file is shared by N6a (server) and
// N6b (worker): both add it byte for byte, so the merge is clean. Change it
// only through Main Dev.

// CapGitTokenV1: the worker fetches per-job GitHub tokens from PathGitToken
// and ships by push only; the server opens PRs (§14).
const CapGitTokenV1 = "gittoken.v1"

// GitTokenPathSuffix is appended to PathPrefix + "jobs/{job_id}".
const GitTokenPathSuffix = "/git-token"

// Git token access levels.
const (
	GitAccessRead  = "read"
	GitAccessWrite = "write"
)

// GitTokenRequest is the body of POST /worker/v1/jobs/{job_id}/git-token.
type GitTokenRequest struct {
	Repo   string `json:"repo"`
	Access string `json:"access"`
}

// GitTokenResponse never reaches a log, the spool, meta.json or the DB.
type GitTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	URL       string `json:"url"`
}

// Error codes of the git-token endpoint and remote start (§14).
const (
	CodeGitHubAppNotConfigured = "github_app_not_configured"
	CodeGitHubNotInstalled     = "github_not_installed"
	CodeRepoNotInstalled       = "repo_not_installed"
	CodeRepoNotGitHub          = "repo_not_github"
	CodeGitHubUnavailable      = "github_unavailable"
)

// ReasonRepoAuth prefixes a job or ship failure caused by GitHub auth:
// "repo_auth: <repo>".
const ReasonRepoAuth = "repo_auth"

// DetailServerLacksGitToken is the bad_spec decline detail from a
// gittoken.v1 worker whose server did not list the capability.
const DetailServerLacksGitToken = "server lacks gittoken.v1"

// ShipPushResult is a ship control_result under gittoken.v1: the worker
// pushes only, and the server opens or updates the PR per repo.
type ShipPushResult struct {
	Repos []ShipPushRepo `json:"repos"`
}

type ShipPushRepo struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	Pushed  bool   `json:"pushed"`
	HeadSHA string `json:"head_sha,omitempty"`
	Error   string `json:"error,omitempty"`
}
