// Package selfupdate is dd-worker's opt-in self-update
// (docs/adr/0002-self-update.md): the policy values it reads, the request
// the unprivileged worker writes, the result the root step writes, and the
// root step itself (Run, `dd-worker update-run`).
package selfupdate

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Window is policy auto_update_window: local-time minutes since midnight,
// [Start, End). Start > End wraps midnight. The zero Window (Set false)
// is always open.
type Window struct {
	Set        bool
	Start, End int
}

// ErrBadWindow: not HH:MM-HH:MM, or an empty window.
var ErrBadWindow = errors.New("auto_update_window is not HH:MM-HH:MM")

// ParseWindow reads "HH:MM-HH:MM" (24-hour, local time). Start and end must
// differ.
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, ErrBadWindow
	}
	start, err1 := parseHM(a)
	end, err2 := parseHM(b)
	if err1 != nil || err2 != nil || start == end {
		return Window{}, ErrBadWindow
	}
	return Window{Set: true, Start: start, End: end}, nil
}

func parseHM(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, ErrBadWindow
	}
	h, err := strconv.Atoi(s[:2])
	if err != nil || h < 0 || h > 23 {
		return 0, ErrBadWindow
	}
	m, err := strconv.Atoi(s[3:])
	if err != nil || m < 0 || m > 59 {
		return 0, ErrBadWindow
	}
	return h*60 + m, nil
}

// Open reports whether t (in its own location) is inside the window.
func (w Window) Open(t time.Time) bool {
	if !w.Set {
		return true
	}
	now := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return now >= w.Start && now < w.End
	}
	return now >= w.Start || now < w.End
}

// MaxProxyURL bounds auto_update_https_proxy.
const MaxProxyURL = 256

// ValidProxy reports whether s is an http or https proxy of the form
// scheme://host:port with no userinfo, path, query or fragment.
func ValidProxy(s string) bool {
	if s == "" || len(s) > MaxProxyURL {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.Host == "" || u.Port() == "" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	p, err := strconv.Atoi(u.Port())
	return err == nil && p > 0 && p < 65536
}
