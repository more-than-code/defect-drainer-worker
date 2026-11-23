package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/egress"
)

// Self-test names: never resolved, the proxy's dialer answers for them.
const (
	selfTestAllowed = "allowed.selftest.invalid"
	selfTestRefused = "refused.selftest.invalid"
)

// SelfTest is the startup check behind verify_sandbox.v1 (V9). It runs the
// sandbox with the verify layout's parts (a hidden dir, a read-only bind, a
// writable dir, the egress bridge) and checks each: the hidden file is
// gone, the read-only dir refuses a write, the writable one takes it, a
// CONNECT to an allowed host is tunnelled and one to another host is
// refused. It needs no network: the allowed host is a fake target in this
// process. The error, if any, wraps ErrUnavailable and reads as a reason.
func (l Launcher) SelfTest(ctx context.Context) error {
	if l.Bwrap == "" {
		return fmt.Errorf("%w: bwrap: not found", ErrUnavailable)
	}
	dir, err := os.MkdirTemp("", "ddv-selftest-")
	if err != nil {
		return fmt.Errorf("%w: self-test: %v", ErrUnavailable, err)
	}
	defer os.RemoveAll(dir)
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return fmt.Errorf("%w: self-test: %v", ErrUnavailable, err)
	}
	sockDirHost := filepath.Join(dir, "egress")
	work := filepath.Join(dir, "work")
	secret := filepath.Join(dir, "secret")
	ro := filepath.Join(dir, "ro")
	for _, d := range []string{sockDirHost, work, secret, ro} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("%w: self-test: %v", ErrUnavailable, err)
		}
	}
	if err := os.WriteFile(filepath.Join(secret, "key"), []byte("x"), 0o600); err != nil {
		return fmt.Errorf("%w: self-test: %v", ErrUnavailable, err)
	}
	sock := filepath.Join(sockDirHost, "s")
	fake := func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	p, err := egress.Listen(sock, []string{selfTestAllowed}, egress.Options{Dial: fake})
	if err != nil {
		return fmt.Errorf("%w: egress: %v", ErrUnavailable, err)
	}
	defer p.Close()

	ask := func(host string) string {
		return `printf 'CONNECT ` + host + `:443 HTTP/1.1\r\nHost: ` + host + `:443\r\n\r\n' | "$DD_SOCAT" -t 5 - TCP:127.0.0.1:` + fmt.Sprint(ProxyPort) + ` | head -n 1`
	}
	script := strings.Join([]string{
		`echo "A:$(` + ask(selfTestAllowed) + `)"`,
		`echo "R:$(` + ask(selfTestRefused) + `)"`,
		`if cat ` + filepath.Join(secret, "key") + ` >/dev/null 2>&1; then echo SECRET; fi`,
		`if touch ` + filepath.Join(ro, "w") + ` 2>/dev/null; then echo ROWRITE; fi`,
		`echo "F:$(sed -n 's/^Seccomp_filters:[[:space:]]*//p' /proc/self/status)"`,
		`echo "M:$(sed -n 's/^Seccomp:[[:space:]]*//p' /proc/self/status)"`,
		`touch ./ok`,
	}, "\n")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proc, err := l.Command(ctx, Run{
		Command: script, Dir: work, Home: os.Getenv("HOME"),
		Hide:      []string{secret},
		Roots:     []string{dir},
		Writable:  []string{work},
		ReadOnly:  []string{ro},
		Env:       []string{"PATH=/usr/local/bin:/usr/bin:/bin", "DD_SOCAT=" + l.Socat},
		ProxySock: sock,
	})
	if err != nil {
		return err
	}
	var out bytes.Buffer
	proc.Cmd.Stdout, proc.Cmd.Stderr = &out, &out
	err = proc.Start()
	if err == nil {
		err = proc.Cmd.Wait()
	}
	if !proc.Reached() {
		return fmt.Errorf("%w: bwrap: %s", ErrUnavailable, firstLine(out.String(), err))
	}
	if err != nil {
		return fmt.Errorf("%w: self-test: %s", ErrUnavailable, firstLine(out.String(), err))
	}
	text := out.String()
	switch {
	case !strings.Contains(text, "A:HTTP/1.1 200"):
		return fmt.Errorf("%w: egress: an allowed CONNECT was not tunnelled", ErrUnavailable)
	case !strings.Contains(text, "R:HTTP/1.1 403"):
		return fmt.Errorf("%w: egress: a refused CONNECT was not denied", ErrUnavailable)
	case strings.Contains(text, "SECRET"):
		return fmt.Errorf("%w: bwrap: a hidden file was readable", ErrUnavailable)
	case strings.Contains(text, "ROWRITE"):
		return fmt.Errorf("%w: bwrap: a read-only dir was writable", ErrUnavailable)
	}
	if !filterLoaded(text) {
		return fmt.Errorf("%w: seccomp: the filter was not loaded", ErrUnavailable)
	}
	if _, err := os.Stat(filepath.Join(work, "ok")); err != nil {
		return fmt.Errorf("%w: bwrap: the work dir is not writable", ErrUnavailable)
	}
	return nil
}

// filterLoaded reports whether the self-test's command ran under one more
// seccomp filter than this process (the systemd unit may add its own). A
// kernel without Seccomp_filters (before 5.9) shows only the mode: filter
// mode (2) inside is then accepted when this process has none.
func filterLoaded(text string) bool {
	field := func(src, prefix string) string {
		for _, l := range strings.Split(src, "\n") {
			if v, ok := strings.CutPrefix(l, prefix); ok {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	status, _ := os.ReadFile("/proc/self/status")
	inN, _ := strconv.Atoi(field(text, "F:"))
	if own := field(string(status), "Seccomp_filters:"); own != "" {
		n, err := strconv.Atoi(own)
		return err == nil && inN > n
	}
	return field(text, "M:") == "2" && field(string(status), "Seccomp:") == "0"
}

// Reason is err as `dd-worker status` and the hello show it.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrUnavailable.Error()+": ")
}

func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	if err == nil {
		return "the sandbox did not start"
	}
	return err.Error()
}
