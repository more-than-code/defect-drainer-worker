// Package egress is the network a sandboxed verify command gets: an HTTP
// proxy on a Unix socket that reaches only an exact list of hosts.
//
// The sandbox has no network of its own. A bridge inside it forwards
// 127.0.0.1:<port> to this proxy's socket, and HTTPS_PROXY/HTTP_PROXY point
// there. A CONNECT to an allowed host is tunnelled; a plain-HTTP request in
// absolute form to an allowed host is forwarded. Everything else gets 403,
// and each refusal is reported by host name only (never a path, a query or
// a header, which can carry tokens).
//
// Limits keep one run from tying up the worker: at most maxConns
// connections at once, idle tunnels closed after tunnelIdle, ports 443 and
// 80 only, and never an address on this host or its private networks.
package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// maxConns bounds the proxy's open connections (each tunnel counts).
	maxConns = 48
	// tunnelIdle closes a tunnel with no traffic either way for this long.
	tunnelIdle = 2 * time.Minute
	// maxHostLen bounds a refused host as reported.
	maxHostLen = 253
)

// Dialer opens the upstream connection for an allowed host.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Options tune a Proxy. The zero value is the production proxy.
type Options struct {
	// Dial replaces the default dialer (tests and the worker's self-test
	// point allowed names at a local fake target).
	Dial Dialer
	// OnRefuse is called with the host of every refused request.
	OnRefuse func(host string)
}

// Proxy is one verify run's egress proxy.
type Proxy struct {
	allow    map[string]bool
	dial     Dialer
	onRefuse func(string)
	ln       net.Listener
	srv      *http.Server
	rp       *httputil.ReverseProxy

	mu      sync.Mutex
	closed  bool
	tunnels map[net.Conn]struct{}
	done    chan struct{}
}

// Listen starts a proxy on the Unix socket sock that allows exactly the
// hosts in allowed (case-insensitive, on port 443 or 80).
func Listen(sock string, allowed []string, opts Options) (*Proxy, error) {
	raw, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	ln := &limitListener{Listener: raw, sem: make(chan struct{}, maxConns)}
	p := &Proxy{allow: map[string]bool{}, dial: opts.Dial, onRefuse: opts.OnRefuse, ln: ln, tunnels: map[net.Conn]struct{}{}, done: make(chan struct{})}
	for _, h := range allowed {
		if h = normHost(h); h != "" {
			p.allow[h] = true
		}
	}
	if p.dial == nil {
		p.dial = publicDialer()
	}
	if p.onRefuse == nil {
		p.onRefuse = func(string) {}
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = pr.In.URL
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           p.dialAllowed,
			ResponseHeaderTimeout: 5 * time.Minute,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errRefused) {
				p.refuse(w, r.URL.Hostname())
				return
			}
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		_ = p.srv.Serve(ln)
		close(p.done)
	}()
	return p, nil
}

// Close stops the proxy and every open tunnel.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	err := p.srv.Close()
	p.mu.Lock()
	for c := range p.tunnels {
		_ = c.Close()
	}
	p.mu.Unlock()
	<-p.done
	return err
}

// Allowed reports whether host (without port) is on the list. A raw IP never
// is: the list holds names.
func (p *Proxy) Allowed(host string) bool {
	h := normHost(host)
	if h == "" || net.ParseIP(h) != nil {
		return false
	}
	return p.allow[h]
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Scheme != "http" || r.URL.Host == "" || (r.URL.Port() != "" && r.URL.Port() != "80") {
		p.refuse(w, r.URL.Hostname())
		return
	}
	if !p.Allowed(r.URL.Hostname()) {
		p.refuse(w, r.URL.Hostname())
		return
	}
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Proxy-Connection")
	p.rp.ServeHTTP(w, r)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || (port != "443" && port != "80") || !p.Allowed(host) {
		if err != nil {
			host = r.Host
		}
		p.refuse(w, host)
		return
	}
	up, err := p.dialAllowed(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		if errors.Is(err, errRefused) {
			p.refuse(w, host)
			return
		}
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "no tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	up, down = &idleConn{Conn: up}, &idleConn{Conn: down}
	if !p.track(up, down) {
		return
	}
	defer p.untrack(up, down)
	if _, err := io.WriteString(down, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent after the CONNECT head are already buffered.
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			return
		}
	}
	pipe(up, down)
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

func (p *Proxy) track(cs ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for _, c := range cs {
			_ = c.Close()
		}
		return false
	}
	for _, c := range cs {
		p.tunnels[c] = struct{}{}
	}
	return true
}

func (p *Proxy) untrack(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		delete(p.tunnels, c)
	}
}

func (p *Proxy) refuse(w http.ResponseWriter, host string) {
	p.onRefuse(reportHost(host))
	w.Header().Set("Connection", "close")
	http.Error(w, "egress refused by Defect Drainer: host not in the allowed domains", http.StatusForbidden)
}

// dialAllowed re-checks the host (the transport reuses this for plain HTTP)
// and dials it.
func (p *Proxy) dialAllowed(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !p.Allowed(host) {
		return nil, errRefused
	}
	return p.dial(ctx, network, addr)
}

var errRefused = errors.New("egress: destination refused")

// publicDialer refuses a destination that resolves to this host, its link
// or a private network (Blocked): an allowed name must not become a way to
// the host's own services or its LAN.
func publicDialer() Dialer {
	d := &net.Dialer{
		Timeout: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errRefused
			}
			if ip := net.ParseIP(host); ip == nil || Blocked(ip) {
				return errRefused
			}
			return nil
		},
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil && (errors.Is(err, errRefused) || strings.Contains(err.Error(), errRefused.Error())) {
			return nil, errRefused
		}
		return c, err
	}
}

// blockedNets are ranges the proxy never dials besides the classes Blocked
// tests: carrier-grade NAT, "this network", and the cloud metadata IPv6
// address (also inside the ULA range).
var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "0.0.0.0/8", "fd00:ec2::254/128"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// Blocked reports whether ip is one the proxy never dials: loopback,
// link-local (cloud metadata), private (RFC 1918, ULA), multicast,
// unspecified, carrier-grade NAT and 0.0.0.0/8.
func Blocked(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// reportHost is a refused host as logged: lowercase, at most maxHostLen
// bytes, and only host-name characters (anything else becomes "_"), so a
// request can't write arbitrary text into the job log.
func reportHost(h string) string {
	h = normHost(h)
	if len(h) > maxHostLen {
		h = h[:maxHostLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == ':':
			return r
		}
		return '_'
	}, h)
}

// limitListener holds Accept while maxConns connections are open.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &releaseConn{Conn: c, release: func() { <-l.sem }}, nil
}

// releaseConn frees its slot once, on the first Close.
type releaseConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *releaseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// idleConn pushes its deadline out on every read and write: a tunnel with
// no traffic for tunnelIdle ends.
type idleConn struct{ net.Conn }

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(tunnelIdle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(tunnelIdle))
	return c.Conn.Write(b)
}

func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return h
}
