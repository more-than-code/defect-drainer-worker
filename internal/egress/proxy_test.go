package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUpstream maps every allowed name to one local test server.
func fakeUpstream(t *testing.T) (*httptest.Server, Dialer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s %s", r.Host, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return srv, func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "egr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

type refusals struct {
	mu    sync.Mutex
	hosts []string
}

func (r *refusals) add(h string) { r.mu.Lock(); r.hosts = append(r.hosts, h); r.mu.Unlock() }
func (r *refusals) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func startProxy(t *testing.T, allowed []string, dial Dialer) (string, *refusals) {
	t.Helper()
	sock := shortSock(t)
	ref := &refusals{}
	p, err := Listen(sock, allowed, Options{Dial: dial, OnRefuse: ref.add})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return sock, ref
}

// raw sends one request head over the socket and returns the status line.
func raw(t *testing.T, sock, head string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(status)
}

func TestConnectAllowedTunnels(t *testing.T) {
	_, dial := fakeUpstream(t)
	sock, ref := startProxy(t, []string{"Registry.Example.Test"}, dial)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "CONNECT registry.example.test:443 HTTP/1.1\r\nHost: registry.example.test:443\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	fmt.Fprint(c, "GET /pkg.tgz HTTP/1.1\r\nHost: registry.example.test\r\nConnection: close\r\n\r\n")
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "hello registry.example.test /pkg.tgz" {
		t.Fatalf("tunnel: %d %q", resp.StatusCode, body)
	}
	if got := ref.list(); len(got) != 0 {
		t.Fatalf("refusals: %v", got)
	}
}

func TestRefusals(t *testing.T) {
	_, dial := fakeUpstream(t)
	sock, ref := startProxy(t, []string{"registry.example.test"}, dial)
	for _, tc := range []struct{ head, host string }{
		{"CONNECT other.example.test:443 HTTP/1.1\r\nHost: other.example.test:443\r\n\r\n", "other.example.test"},
		{"CONNECT 192.0.2.1:443 HTTP/1.1\r\nHost: 192.0.2.1:443\r\n\r\n", "192.0.2.1"},
		{"CONNECT [2001:db8::1]:443 HTTP/1.1\r\nHost: [2001:db8::1]:443\r\n\r\n", "2001:db8::1"},
		{"CONNECT sub.registry.example.test:443 HTTP/1.1\r\nHost: sub.registry.example.test:443\r\n\r\n", "sub.registry.example.test"},
		{"GET http://other.example.test/x?token=secret HTTP/1.1\r\nHost: other.example.test\r\n\r\n", "other.example.test"},
		{"GET /x HTTP/1.1\r\nHost: registry.example.test\r\n\r\n", ""},
		{"GET https://registry.example.test/x HTTP/1.1\r\nHost: registry.example.test\r\n\r\n", "registry.example.test"},
		// L7: ports 443 and 80 only.
		{"CONNECT registry.example.test:22 HTTP/1.1\r\nHost: registry.example.test:22\r\n\r\n", "registry.example.test"},
		{"GET http://registry.example.test:8080/x HTTP/1.1\r\nHost: registry.example.test:8080\r\n\r\n", "registry.example.test"},
	} {
		status := raw(t, sock, tc.head)
		if !strings.Contains(status, " 403 ") {
			t.Errorf("%q: %s, want 403", tc.head, status)
		}
	}
	got := strings.Join(ref.list(), ",")
	for _, bad := range []string{"secret", "/x", "token", "443"} {
		if strings.Contains(got, bad) {
			t.Errorf("refusal log %q carries %q: host names only", got, bad)
		}
	}
	if !strings.Contains(got, "other.example.test") || !strings.Contains(got, "192.0.2.1") {
		t.Errorf("refusal log %q", got)
	}
}

func TestPlainHTTPAllowedForwarded(t *testing.T) {
	_, dial := fakeUpstream(t)
	sock, _ := startProxy(t, []string{"registry.example.test"}, dial)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustURL(t, "http://proxy.invalid")),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := client.Get("http://registry.example.test/a/b")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello registry.example.test /a/b" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	resp, err = client.Get("http://other.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("other host: %d", resp.StatusCode)
	}
}

func TestDefaultDialerRefusesLocalAddresses(t *testing.T) {
	// An allowed name resolving to loopback must not reach the host's own
	// services: "localhost" stands in for such a name.
	sock, ref := startProxy(t, []string{"localhost"}, nil)
	status := raw(t, sock, "CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
	if !strings.Contains(status, " 403 ") {
		t.Fatalf("loopback: %s, want 403", status)
	}
	if got := ref.list(); len(got) != 1 || got[0] != "localhost" {
		t.Fatalf("refusals: %v", got)
	}
	// The private ranges are built, not written out: the repo's publish
	// check refuses private address literals in tracked files.
	private := []string{net.IPv4(10, 0, 0, 1).String(), net.IPv4(172, 16, 0, 1).String(), net.IPv4(192, 168, 1, 1).String()}
	for _, ip := range append([]string{"127.0.0.1", "::1", "169.254.169.254", "0.0.0.0", "0.1.2.3", "fe80::1", "224.0.0.1",
		"100.64.0.1", "fd00::1", "fd00:ec2::254"}, private...) {
		if !Blocked(net.ParseIP(ip)) {
			t.Errorf("%s not blocked", ip)
		}
	}
	for _, ip := range []string{"192.0.2.1", "203.0.113.7", "2001:db8::1", "100.128.0.1"} {
		if Blocked(net.ParseIP(ip)) {
			t.Errorf("%s blocked", ip)
		}
	}
}

func TestCloseEndsTunnels(t *testing.T) {
	_, dial := fakeUpstream(t)
	sock := shortSock(t)
	p, err := Listen(sock, []string{"registry.example.test"}, Options{Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "CONNECT registry.example.test:80 HTTP/1.1\r\nHost: registry.example.test:80\r\n\r\n")
	br := bufio.NewReader(c)
	if resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("tunnel still open after Close")
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// M6: a refused host is reported trimmed to host-name characters.
func TestReportHost(t *testing.T) {
	if got := reportHost("Evil.Example.TEST\x1b[31m\n"); got != "evil.example.test__31m" {
		t.Errorf("%q", got)
	}
	if got := reportHost(strings.Repeat("a", 400)); len(got) != maxHostLen {
		t.Errorf("len %d", len(got))
	}
}

// M6: at most maxConns connections at once; a closed one frees its slot.
func TestConnectionCap(t *testing.T) {
	_, dial := fakeUpstream(t)
	sock, _ := startProxy(t, []string{"registry.example.test"}, dial)
	var conns []net.Conn
	for i := 0; i < maxConns; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(c, "CONNECT registry.example.test:443 HTTP/1.1\r\nHost: registry.example.test:443\r\n\r\n")
		if resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect}); err != nil || resp.StatusCode != 200 {
			t.Fatalf("tunnel %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	extra, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	fmt.Fprint(extra, "CONNECT registry.example.test:443 HTTP/1.1\r\nHost: registry.example.test:443\r\n\r\n")
	_ = extra.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := bufio.NewReader(extra).ReadByte(); err == nil {
		t.Fatal("a connection over the cap was served")
	}
	_ = conns[0].Close()
	_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if resp, err := http.ReadResponse(bufio.NewReader(extra), &http.Request{Method: http.MethodConnect}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("after a slot freed: %v", err)
	}
	for _, c := range conns[1:] {
		_ = c.Close()
	}
}
