package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReleaseBase is the only place a host downloads an update from (ADR 0002
// §6.2 step 5). It is compiled in: neither the server nor the worker can
// change it.
const ReleaseBase = "https://github.com/more-than-code/defect-drainer-worker/releases/download"

// Download limits.
const (
	MaxFile      = 64 << 20
	FileTimeout  = 120 * time.Second
	MaxRedirects = 5
)

// allowedHosts are the hosts a download and its redirects may reach.
var allowedHosts = map[string]bool{
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// Fetcher downloads one file of a release.
type Fetcher interface {
	Fetch(ctx context.Context, version, name string) ([]byte, error)
}

// checkURL allows https on port 443 to an allowed host, with no userinfo.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("download address not allowed: %s://%s", u.Scheme, u.Host)
	}
	return nil
}

// HTTPFetcher downloads from ReleaseBase. Proxy is the policy's
// auto_update_https_proxy ("": none); nothing is read from the environment.
type HTTPFetcher struct {
	Proxy string
	// Client is for tests; nil builds the production client.
	Client *http.Client
	// Base is for tests; "" is ReleaseBase.
	Base string
}

func (f HTTPFetcher) client() (*http.Client, error) {
	if f.Client != nil {
		return f.Client, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if f.Proxy != "" {
		pu, err := url.Parse(f.Proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxRedirects {
				return errors.New("too many redirects")
			}
			return checkURL(req.URL)
		},
	}, nil
}

// Fetch downloads <base>/v<version>/<name>, at most MaxFile bytes within
// FileTimeout.
func (f HTTPFetcher) Fetch(ctx context.Context, version, name string) ([]byte, error) {
	base := f.Base
	if base == "" {
		base = ReleaseBase
	}
	u, err := url.Parse(base + "/v" + version + "/" + name)
	if err != nil {
		return nil, err
	}
	if f.Client == nil {
		if err := checkURL(u); err != nil {
			return nil, err
		}
	}
	c, err := f.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, FileTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFile {
		return nil, fmt.Errorf("%s is over %d bytes", name, MaxFile)
	}
	return b, nil
}
