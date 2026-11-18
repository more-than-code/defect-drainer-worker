package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	osexec "os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// gitHubOrigin is the only origin a gittoken.v1 job clones from and pushes
// to, and the scope of the token header (§14). Tests point it at their own
// host; nothing else changes it.
var gitHubOrigin = "https://github.com/"

// gitAllowProtocol is the only transport a token git command may use, so a
// host url.*.insteadOf rewrite to ssh fails loudly instead of using the
// host's own key (A2). Tests add "file" for their bare origins.
var gitAllowProtocol = "https"

// gitTokenReuseMargin: a cached token is used until this long before its
// expires_at (§14). gitTokenMaxTTL caps how long one is cached whatever
// expires_at says (installation tokens live about an hour).
const (
	gitTokenReuseMargin = 5 * time.Minute
	gitTokenMaxTTL      = time.Hour
)

// gitTokenRe is the shape of a token the worker will put in a header: no
// whitespace, no control character, nothing that could start another
// header or config entry.
var gitTokenRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{8,512}$`)

// gitTokenEnvDrop are removed from every token git command: the host's own
// GitHub and SSH credentials (A2: the App only), and git's -c list, which git
// reads after GIT_CONFIG_COUNT and could re-add a credential helper.
var gitTokenEnvDrop = append(append([]string(nil), hostPushCredentials...),
	"GIT_CONFIG_PARAMETERS", "GIT_TERMINAL_PROMPT", "GIT_ALLOW_PROTOCOL")

// cachedGitToken is one minted token, in memory only.
type cachedGitToken struct {
	value   string
	expires time.Time
}

// repoAuthError ends a job, or fails one ship entry, with "repo_auth: <repo>".
type repoAuthError struct{ repo string }

func (e repoAuthError) Error() string { return workerproto.ReasonRepoAuth + ": " + e.repo }

// gitToken returns a token for one of the job's repos, from the cache or
// freshly minted (§14). canon is the repo's canonical URL: the server's url
// must name the same repo under gitHubOrigin.
func (j *job) gitToken(ctx context.Context, repo, access, canon string) (string, error) {
	key := repo + "\x00" + access
	j.mu.Lock()
	if t, ok := j.gitTokens[key]; ok && time.Now().Add(gitTokenReuseMargin).Before(t.expires) {
		j.mu.Unlock()
		return t.value, nil
	}
	jobToken := j.token
	j.mu.Unlock()

	resp, err := j.w.fetchGitToken(ctx, j.id, jobToken, repo, access)
	if err != nil {
		return "", err
	}
	if !gitTokenRe.MatchString(resp.Token) {
		return "", fmt.Errorf("%s: git-token for %s: malformed token", workerproto.CodeInternal, repo)
	}
	// Redact before first use: the token and the header form git gets.
	j.w.rememberToken(resp.Token)
	j.w.rememberToken(basicAuth(resp.Token))
	// GitHub owner and repo names are case-insensitive.
	if got, err := CanonicalRepoURL(resp.URL); err != nil || !strings.EqualFold(got, canon) {
		return "", fmt.Errorf("%s: git-token for %s names another repo", workerproto.CodeInternal, repo)
	}
	if exp, err := time.Parse(time.RFC3339, resp.ExpiresAt); err == nil {
		if limit := time.Now().Add(gitTokenMaxTTL); exp.After(limit) {
			exp = limit
		}
		j.mu.Lock()
		if j.gitTokens == nil {
			j.gitTokens = map[string]cachedGitToken{}
		}
		j.gitTokens[key] = cachedGitToken{value: resp.Token, expires: exp}
		j.mu.Unlock()
	}
	return resp.Token, nil
}

// fetchGitToken is POST /worker/v1/jobs/{id}/git-token with the job token.
// 403 (repo not the job's, no ship pending) and 409 (App not configured or
// not installed on the repo) mean this job cannot reach the repo:
// repo_auth. Anything else (401 lease not live, 429, 502) is internal.
// Neither carries the response body.
func (w *Worker) fetchGitToken(ctx context.Context, jobID, jobToken, repo, access string) (workerproto.GitTokenResponse, error) {
	var out workerproto.GitTokenResponse
	body, _ := json.Marshal(workerproto.GitTokenRequest{Repo: repo, Access: access})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := w.do(ctx, http.MethodPost, jobPath(jobID, strings.TrimPrefix(workerproto.GitTokenPathSuffix, "/")), jobToken, bytes.NewReader(body), "application/json")
	if err != nil {
		var se httpStatusError
		if errors.As(err, &se) && (se.status == http.StatusForbidden || se.status == http.StatusConflict) {
			return out, repoAuthError{repo}
		}
		return out, fmt.Errorf("%s: git-token for %s: %v", workerproto.CodeInternal, repo, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return out, fmt.Errorf("%s: git-token for %s: response is not valid JSON", workerproto.CodeInternal, repo)
	}
	return out, nil
}

func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// tokenGit is git.HostGit with the token given per command (§14): through
// GIT_CONFIG_COUNT env, never argv or .git/config. The empty
// credential.helper drops the host's helpers; the empty extraheader drops
// any header the host config sets for the origin before ours is added.
// GIT_TERMINAL_PROMPT=0 makes a refused token fail instead of prompting;
// GIT_ALLOW_PROTOCOL keeps it on https. Run it only in a git dir the agent
// cannot write (the clone), never with -C on a worktree.
func tokenGit(token string, args ...string) *osexec.Cmd {
	cmd := git.HostGit(args...)
	git.DropEnv(cmd, gitTokenEnvDrop...)
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL="+gitAllowProtocol)
	header := "http." + gitHubOrigin + ".extraheader"
	git.AddGitConfigEnv(cmd,
		"credential.helper", "",
		header, "",
		header, "Authorization: Basic "+basicAuth(token),
	)
	return cmd
}

// gitAuthFailure reports whether git's output says the remote refused the
// credentials (GitHub answers 404 "Repository not found" when a token
// cannot see a repo).
func gitAuthFailure(out []byte) bool {
	s := strings.ToLower(string(out))
	if strings.Contains(s, "rate limit") {
		return false
	}
	for _, m := range []string{
		"authentication failed", "could not read username", "could not read password",
		"terminal prompts disabled", "invalid username or password",
		"returned error: 401", "returned error: 403", "repository not found",
		"write access to repository not granted",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// gitFailure turns a failed token git command into the job's reason:
// repo_auth for an auth refusal, else internal with git's redacted last line.
// The redacted detail of an auth refusal goes to the worker log only.
func (w *Worker) gitFailure(op, repo string, out []byte) error {
	// Redact the whole output before cutting it: a secret across the cut
	// would no longer match.
	line := lastLine([]byte(w.currentRedactor().Values(string(out))))
	if gitAuthFailure(out) {
		w.logf("%s %s refused: %s", op, repo, line)
		return repoAuthError{repo}
	}
	return fmt.Errorf("%s: %s %s failed: %s", workerproto.CodeInternal, op, repo, line)
}
