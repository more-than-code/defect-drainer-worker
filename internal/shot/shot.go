// Package shot is `dd-worker shot`: dd-shot's browser driver for a Stack
// sign-in (job stack S10). It starts the host browser with a DevTools
// endpoint on loopback, signs in as the stack's seed user (a form, or an
// HTTP call made from the page so the session cookie lands in the
// browser), checks the sign-in, then screenshots each path. It speaks the
// Chrome DevTools Protocol over the websocket library the worker already
// uses, so nothing is installed at run time.
//
// It prints dd-shot's status lines only and never a credential:
//
//	DD-SHOT SIGNIN OK <form|http>
//	DD-SHOT FAIL <DEF-id> signin <reason>
//	DD-SHOT OK <DEF-id> <file> <bytes>B <path>
//	DD-SHOT FAIL <DEF-id> <path> no-png <reason>
//
// A failed sign-in takes no screenshot.
package shot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/more-than-code/defect-drainer-worker/exec"
)

// Options is one `dd-worker shot` call.
type Options struct {
	Browser string          // the host browser (policy agent_browser)
	SignIn  exec.ShotSignIn // from dd-shot-signin.json
	Def     string          // the defect id, for the status lines
	Dir     string          // fix-evidence/<DEF-id>
	After   int             // the highest fix-NN already there
	AppURL  string          // the app's URL, no trailing slash
	Paths   []string        // the pages to screenshot
	Timeout time.Duration   // each page's bound, and the sign-in's
	Out     io.Writer
}

// Status codes, as dd-shot's.
const (
	ExitOK   = 0
	ExitFail = 1
)

// Viewport is dd-shot's window size.
const viewportW, viewportH = 1280, 800

// Run signs in and screenshots; it returns ExitOK when every screenshot was
// written.
func Run(ctx context.Context, o Options) int {
	b, err := launch(ctx, o.Browser)
	if err != nil {
		fmt.Fprintf(o.Out, "DD-SHOT FAIL %s signin browser %s\n", o.Def, clean(err.Error()))
		return ExitFail
	}
	defer b.close()
	p, err := b.page(ctx)
	if err != nil {
		fmt.Fprintf(o.Out, "DD-SHOT FAIL %s signin browser %s\n", o.Def, clean(err.Error()))
		return ExitFail
	}
	sctx, cancel := context.WithTimeout(ctx, o.Timeout)
	reason := p.signIn(sctx, o.SignIn)
	cancel()
	if reason != "" {
		fmt.Fprintf(o.Out, "DD-SHOT FAIL %s signin %s\n", o.Def, reason)
		return ExitFail
	}
	fmt.Fprintf(o.Out, "DD-SHOT SIGNIN OK %s\n", o.SignIn.Method)
	status := ExitOK
	for i, path := range o.Paths {
		name := fmt.Sprintf("fix-%02d.png", o.After+i+1)
		file := filepath.Join(o.Dir, name)
		pctx, cancel := context.WithTimeout(ctx, o.Timeout)
		n, err := p.screenshot(pctx, o.AppURL+path, file)
		cancel()
		if err != nil {
			_ = os.Remove(file)
			fmt.Fprintf(o.Out, "DD-SHOT FAIL %s %s no-png %s\n", o.Def, path, clean(err.Error()))
			status = ExitFail
			continue
		}
		fmt.Fprintf(o.Out, "DD-SHOT OK %s %s %dB %s\n", o.Def, file, n, path)
	}
	return status
}

// clean keeps a reason to one short printable line.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120]
	}
	return strings.TrimSpace(s)
}

// browser is a launched browser and its DevTools connection.
type browser struct {
	cmd  *osexec.Cmd
	prof string
	cdp  *cdp
}

// launch starts the browser headless with a fresh profile under $TMPDIR and
// its DevTools endpoint on a loopback port it picks, and connects to it.
func launch(ctx context.Context, bin string) (*browser, error) {
	prof, err := os.MkdirTemp("", "dd-shot-cdp-")
	if err != nil {
		return nil, err
	}
	cmd := osexec.Command(bin, "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--user-data-dir="+prof, fmt.Sprintf("--window-size=%d,%d", viewportW, viewportH),
		"--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "about:blank")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr // dd-shot sends it to .dd-shot/browser.log
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(prof)
		return nil, errors.New("cannot start")
	}
	b := &browser{cmd: cmd, prof: prof}
	ws, err := devToolsURL(ctx, prof, 20*time.Second)
	if err != nil {
		b.close()
		return nil, err
	}
	c, err := dial(ctx, ws)
	if err != nil {
		b.close()
		return nil, errors.New("no devtools connection")
	}
	b.cdp = c
	return b, nil
}

// devToolsURL waits for the browser's DevToolsActivePort file: the port,
// then the browser target's path.
func devToolsURL(ctx context.Context, prof string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		if raw, err := os.ReadFile(filepath.Join(prof, "DevToolsActivePort")); err == nil {
			if u, ok := parseActivePort(string(raw)); ok {
				return u, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", errors.New("no devtools port")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// parseActivePort turns DevToolsActivePort's two lines into the browser's
// websocket URL on loopback.
func parseActivePort(s string) (string, bool) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) != 2 {
		return "", false
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	path := strings.TrimSpace(lines[1])
	if err != nil || port < 1 || port > 65535 || !strings.HasPrefix(path, "/devtools/browser/") {
		return "", false
	}
	return "ws://127.0.0.1:" + strconv.Itoa(port) + path, true
}

func (b *browser) close() {
	if b.cdp != nil {
		b.cdp.close()
	}
	if b.cmd.Process != nil {
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		_ = b.cmd.Process.Kill()
		_ = b.cmd.Wait()
	}
	_ = os.RemoveAll(b.prof)
}

// page is one tab, through a flat session on the browser connection.
type page struct {
	c       *cdp
	session string
}

func (b *browser) page(ctx context.Context) (*page, error) {
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := b.cdp.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &t); err != nil {
		return nil, err
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.cdp.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &a); err != nil {
		return nil, err
	}
	p := &page{c: b.cdp, session: a.SessionID}
	err := p.call(ctx, "Emulation.setDeviceMetricsOverride",
		map[string]any{"width": viewportW, "height": viewportH, "deviceScaleFactor": 1, "mobile": false}, nil)
	return p, err
}

func (p *page) call(ctx context.Context, method string, params any, out any) error {
	return p.c.call(ctx, p.session, method, params, out)
}

// eval runs expr in the page (awaiting a promise) and decodes its value.
func (p *page) eval(ctx context.Context, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &r); err != nil {
		return err
	}
	if r.ExceptionDetails != nil {
		return errors.New("script error")
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// navigate loads url and waits until the page is complete and its network
// has settled: no new resource for 700 ms, or 10 s at most.
func (p *page) navigate(ctx context.Context, url string) error {
	var r struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": url}, &r); err != nil {
		return err
	}
	if r.ErrorText != "" {
		return errors.New("navigate " + r.ErrorText)
	}
	return p.settle(ctx)
}

// settle waits for document.readyState complete, then for the resource
// count to hold still for 700 ms (capped at 10 s).
func (p *page) settle(ctx context.Context) error {
	for {
		var st string
		if err := p.eval(ctx, "document.readyState", &st); err == nil && st == "complete" {
			break
		}
		if err := sleep(ctx, 100*time.Millisecond); err != nil {
			return errors.New("timeout")
		}
	}
	limit := time.Now().Add(10 * time.Second)
	last, still := -1, time.Now()
	for time.Now().Before(limit) {
		var n int
		if err := p.eval(ctx, "performance.getEntriesByType('resource').length", &n); err != nil {
			return err
		}
		if n != last {
			last, still = n, time.Now()
		} else if time.Since(still) >= 700*time.Millisecond {
			return nil
		}
		if err := sleep(ctx, 100*time.Millisecond); err != nil {
			return errors.New("timeout")
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// js is v as a JavaScript literal.
func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// signIn signs in and checks it; it returns "" or the reason it failed.
func (p *page) signIn(ctx context.Context, s exec.ShotSignIn) string {
	if err := p.navigate(ctx, s.URL); err != nil {
		return "open " + clean(err.Error())
	}
	switch s.Method {
	case exec.SignInForm:
		if r := p.signInForm(ctx, s); r != "" {
			return r
		}
	case exec.SignInHTTP:
		var status int
		expr := "fetch(" + js(s.HTTPURL) + ",{method:" + js(s.HTTPMethod) +
			",headers:{'Content-Type':'application/json'},credentials:'include',body:" + js(s.HTTPBody) +
			"}).then(r=>r.status,()=>0)"
		if err := p.eval(ctx, expr, &status); err != nil {
			return "http " + clean(err.Error())
		}
		if status < 200 || status > 299 {
			return "http " + strconv.Itoa(status)
		}
	default:
		return "method " + clean(s.Method)
	}
	if s.SuccessURL == "" {
		return ""
	}
	if err := p.navigate(ctx, s.SuccessURL); err != nil {
		return "success-check " + clean(err.Error())
	}
	var got struct {
		Path   string `json:"path"`
		Status int    `json:"status"`
	}
	if err := p.eval(ctx, "({path:location.pathname,status:(performance.getEntriesByType('navigation')[0]||{}).responseStatus||0})", &got); err != nil {
		return "success-check " + clean(err.Error())
	}
	want := s.SuccessPath
	if i := strings.IndexAny(want, "?#"); i >= 0 {
		want = want[:i]
	}
	if got.Path != want {
		return "redirected " + clean(got.Path)
	}
	if got.Status >= 400 {
		return "http " + strconv.Itoa(got.Status)
	}
	return ""
}

// fieldWait bounds the wait for a form's fields to render.
const fieldWait = 15 * time.Second

// signInForm fills the fields as a user would (the native value setter,
// then input and change events, which frameworks listen to), clicks submit
// and waits to leave the sign-in page.
func (p *page) signInForm(ctx context.Context, s exec.ShotSignIn) string {
	f := s.Form
	if f == nil {
		return "no-form"
	}
	for _, fld := range []struct{ name, sel string }{{"email", f.EmailSelector}, {"password", f.PasswordSelector}, {"submit", f.SubmitSelector}} {
		if !p.waitFor(ctx, "document.querySelector("+js(fld.sel)+")!==null", fieldWait) {
			return "no-field " + fld.name
		}
	}
	const set = "((sel,v)=>{const e=document.querySelector(sel);const d=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(e),'value');" +
		"if(d&&d.set){d.set.call(e,v)}else{e.value=v};e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));return true})"
	var path string
	if err := p.eval(ctx, "location.pathname", &path); err != nil {
		return "form " + clean(err.Error())
	}
	for _, v := range [][2]string{{f.EmailSelector, s.Email}, {f.PasswordSelector, s.Password}} {
		if err := p.eval(ctx, set+"("+js(v[0])+","+js(v[1])+")", nil); err != nil {
			return "form fill"
		}
	}
	if err := p.eval(ctx, "document.querySelector("+js(f.SubmitSelector)+").click()", nil); err != nil {
		return "form submit"
	}
	if !p.waitFor(ctx, "location.pathname!=="+js(path)+"&&document.readyState==='complete'", fieldWait) {
		return "still-on-signin"
	}
	if err := p.settle(ctx); err != nil {
		return "form " + clean(err.Error())
	}
	return ""
}

// waitFor polls a boolean expression until it is true or wait passes.
func (p *page) waitFor(ctx context.Context, expr string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		var ok bool
		if err := p.eval(ctx, expr, &ok); err == nil && ok {
			return true
		}
		if sleep(ctx, 150*time.Millisecond) != nil {
			return false
		}
	}
	return false
}

// screenshot loads url and writes a PNG of the viewport to file.
func (p *page) screenshot(ctx context.Context, url, file string) (int, error) {
	if err := p.navigate(ctx, url); err != nil {
		return 0, err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, &r); err != nil {
		return 0, err
	}
	png, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil || len(png) == 0 {
		return 0, errors.New("empty capture")
	}
	if err := os.WriteFile(file, png, 0o644); err != nil {
		return 0, errors.New("cannot write")
	}
	return len(png), nil
}

// cdp is a DevTools connection: commands by id, events ignored.
type cdp struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	next    int64
	pending map[int64]chan reply
	done    chan struct{}
}

type reply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// maxMessage bounds one DevTools message (a full-page PNG in base64).
const maxMessage = 64 << 20

func dial(ctx context.Context, url string) (*cdp, error) {
	// Loopback only, never through a proxy from the environment.
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessage)
	c := &cdp{ws: ws, pending: map[int64]chan reply{}, done: make(chan struct{})}
	go c.read()
	return c, nil
}

func (c *cdp) read() {
	defer close(c.done)
	for {
		_, b, err := c.ws.Read(context.Background())
		if err != nil {
			return
		}
		var m struct {
			ID int64 `json:"id"`
			reply
		}
		if json.Unmarshal(b, &m) != nil || m.ID == 0 {
			continue // an event
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.reply
		}
	}
}

// call sends one command (on session, "" for the browser) and decodes its
// result into out.
func (c *cdp) call(ctx context.Context, session, method string, params any, out any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		msg["sessionId"] = session
	}
	b, _ := json.Marshal(msg)
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return errors.New("devtools write")
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return errors.New(method + ": " + r.Error.Message)
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-c.done:
		return errors.New("devtools closed")
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return errors.New("timeout")
	}
}

func (c *cdp) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}
