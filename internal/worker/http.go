package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Caps on what the worker reads over HTTPS.
const (
	maxSpecBytes = 16 << 20
	maxBlobBytes = workerproto.MaxBlobBytesPerJob
)

// httpClient refuses every redirect (§1): a 3xx is returned as is and
// treated as a failure, so a token is never replayed to another host.
var httpClient = &http.Client{
	Timeout: 60 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// errNotFound is a 404 from the side channel.
var errNotFound = errors.New("not found")

// httpStatusError is a non-2xx answer. It carries no body text that could
// echo a token.
type httpStatusError struct{ status int }

func (e httpStatusError) Error() string { return fmt.Sprintf("server answered %d", e.status) }

// jobPath is the only shape of a side-channel path the worker will use: the
// job's own spec or blob under /worker/v1/jobs/{job_id}/ (§7).
func jobPath(jobID, rest string) string {
	return workerproto.PathPrefix + "jobs/" + jobID + "/" + rest
}

// checkSpecURL accepts the spec_url of an offer or a resume control only
// when it is exactly this job's spec path, so the job token is never sent
// to another path or host.
func checkSpecURL(jobID, specURL string) error {
	if specURL != jobPath(jobID, "spec") {
		return fmt.Errorf("spec_url is not this job's spec path")
	}
	return nil
}

func (w *Worker) do(ctx context.Context, method, path, token string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.origin()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// url.Error repeats the URL, which holds no token; drop it anyway.
		return nil, errors.New("request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, httpStatusError{resp.StatusCode}
	}
	return resp, nil
}

// fetchSpec GETs the job's spec (§5.2, §7). Unknown keys are ignored.
func (w *Worker) fetchSpec(ctx context.Context, jobID, token string) (exec.JobSpec, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := w.do(ctx, http.MethodGet, jobPath(jobID, "spec"), token, nil, "")
	if err != nil {
		return exec.JobSpec{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return exec.JobSpec{}, err
	}
	if len(body) > maxSpecBytes {
		return exec.JobSpec{}, errors.New("spec too large")
	}
	var spec exec.JobSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		var conflict *exec.KeyConflictError
		if errors.As(err, &conflict) {
			return exec.JobSpec{}, conflict
		}
		return exec.JobSpec{}, errors.New("spec is not valid JSON")
	}
	return spec, nil
}

// getBlob downloads one blob and checks its hash.
func (w *Worker) getBlob(ctx context.Context, jobID, sha, token string) ([]byte, error) {
	if !workerproto.ValidSHA256(sha) {
		return nil, errors.New("invalid sha256")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := w.do(ctx, http.MethodGet, jobPath(jobID, "blobs/"+sha), token, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBlobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBlobBytes {
		return nil, errors.New("blob too large")
	}
	if hashHex(data) != sha {
		return nil, errors.New("blob hash mismatch")
	}
	return data, nil
}

// putBlob uploads one blob (§7). A repeat PUT is idempotent on the server.
func (w *Worker) putBlob(ctx context.Context, jobID, sha, contentType, token string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := w.do(ctx, http.MethodPut, jobPath(jobID, "blobs/"+sha), token, bytes.NewReader(data), contentType)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blobType is the content type a harvested file travels as, or "" when it
// is not one the server accepts (§7: png, jpeg, webp, gif; notes as text).
func blobType(data []byte) string {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch ct {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return ct
	}
	return ""
}
