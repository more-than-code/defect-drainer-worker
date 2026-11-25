package git

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/grok"
)

// normalizeSandbox maps empty / restrict / restricted / anything else → strict.
// workspace is the only non-default profile (store.ParseGrokSandbox).
func normalizeSandbox(v string) string {
	if strings.ToLower(strings.TrimSpace(v)) == "workspace" {
		return "workspace"
	}
	return "strict"
}

// TS spawnGrokBatchFix coalesces at MAX_BLOCK = 24_000. We stay line-oriented
// but use the same cap so a child that dumps a huge blob with no newline
// cannot grow the pending buffer (or one job-log line) without bound.
const (
	maxLogBlock      = 24_000
	logTruncateMark  = "...[truncated]"
	lineQueueBound   = 64
	maxAgentLogBytes = 8 << 20 // handoff/agent.log total file cap (not served)
)

// cappedWriter stops writing after limit bytes. Write always reports
// len(p) so io.MultiWriter does not fail the agent stdout copy.
type cappedWriter struct {
	mu    sync.Mutex
	w     io.Writer
	n     int64
	limit int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c == nil || c.w == nil {
		return len(p), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n >= c.limit {
		return len(p), nil
	}
	left := c.limit - c.n
	if int64(len(p)) > left {
		if _, err := c.w.Write(p[:left]); err != nil {
			return len(p), err
		}
		c.n = c.limit
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return len(p), err
}

// lineForwarder splits writes into lines and hands each one to a consumer
// goroutine. Write never persists — it only buffers, splits, and sends.
// The consumer is the only caller of emit (appendLog / persist).
type lineForwarder struct {
	mu     sync.Mutex
	buf    []byte
	secret []byte
	emit   func(string)
	ch     chan string
	done   chan struct{}
	closed bool
}

func newLineForwarder(emit func(string)) *lineForwarder {
	f := &lineForwarder{
		emit: emit,
		ch:   make(chan string, lineQueueBound),
		done: make(chan struct{}),
	}
	go f.consume()
	return f
}

func (f *lineForwarder) consume() {
	defer close(f.done)
	for line := range f.ch {
		if f.emit != nil {
			f.emit(line)
		}
	}
}

func capLine(s string) string {
	if len(s) <= maxLogBlock {
		return s
	}
	keep := maxLogBlock - len(logTruncateMark)
	if keep < 0 {
		keep = 0
	}
	return s[:keep] + logTruncateMark
}

func (f *lineForwarder) Write(p []byte) (int, error) {
	if f == nil {
		return len(p), nil
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return len(p), nil
	}
	f.buf = append(f.buf, p...)
	f.redactBuf()
	lines := f.drain(false)
	ch := f.ch
	f.mu.Unlock()
	for _, line := range lines {
		ch <- line
	}
	return len(p), nil
}

// redactBuf replaces the secret before any newline normalization or cut, and
// holds back len(secret)-1 bytes inside drain so a value split across writes
// is still one match.
func (f *lineForwarder) redactBuf() {
	if len(f.secret) == 0 || len(f.buf) == 0 {
		return
	}
	f.buf = bytes.ReplaceAll(f.buf, f.secret, []byte(redactedMark))
}

func (f *lineForwarder) drain(flush bool) []string {
	limit := f.normalizeSafe(flush)
	if limit == 0 {
		return nil
	}
	var lines []string
	for {
		window := f.buf[:limit]
		i := bytes.IndexByte(window, '\n')
		if i >= 0 {
			line := string(f.buf[:i])
			f.buf = f.buf[i+1:]
			limit = len(f.buf)
			if !flush && len(f.secret) > 1 {
				keep := len(f.secret) - 1
				if limit <= keep {
					break
				}
				limit -= keep
			}
			if strings.TrimSpace(line) != "" {
				lines = append(lines, capLine(line))
			}
			continue
		}
		if limit >= maxLogBlock {
			lines = append(lines, capLine(string(f.buf[:maxLogBlock])))
			f.buf = append([]byte(nil), f.buf[maxLogBlock:]...)
			limit = len(f.buf)
			if !flush && len(f.secret) > 1 {
				keep := len(f.secret) - 1
				if limit <= keep {
					break
				}
				limit -= keep
			}
			continue
		}
		break
	}
	return lines
}

// normalizeSafe turns CR line endings into LF in the portion of buf that is
// safe to emit. The held-back tail is left raw so a secret containing a
// carriage return is still intact for the next Write.
func (f *lineForwarder) normalizeSafe(flush bool) int {
	keep := 0
	if !flush && len(f.secret) > 1 {
		keep = len(f.secret) - 1
	}
	if keep >= len(f.buf) {
		return 0
	}
	head := f.buf[:len(f.buf)-keep]
	tail := append([]byte(nil), f.buf[len(f.buf)-keep:]...)
	head = bytes.ReplaceAll(head, []byte("\r\n"), []byte("\n"))
	head = bytes.ReplaceAll(head, []byte("\r"), []byte("\n"))
	f.buf = append(head, tail...)
	return len(head)
}

// Flush emits a trailing partial line, closes the consumer, and waits for it
// to exit. Call after Wait — os/exec has finished copying stdout/stderr by
// then, so Write cannot race this close.
func (f *lineForwarder) Flush() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		<-f.done
		return
	}
	f.redactBuf()
	lines := f.drain(true)
	f.buf = nil
	f.closed = true
	ch := f.ch
	f.mu.Unlock()
	for _, line := range lines {
		ch <- line
	}
	close(ch)
	<-f.done
}

// VerifySpec is one operator verification command, for the spawn prompt only.
// defaultBatchMaxTurns is the fallback when neither App Settings nor the env
// sets one. Kept as the historical value so an unset app behaves as before.
const defaultBatchMaxTurns = "80"

type VerifySpec struct {
	Repo    string
	Command string
}

// AgentEnd is the run's end event, normalised by the adapter. Grok's field
// names: stopReason and sessionId are camel, num_turns and total_cost_usd are
// snake (verified against Grok Build 1.0.40).
type AgentEnd = agentadapter.End

// ContinuePrompt resumes an iteration. It is a code constant, never
// model-authored: the worker may not widen its own completion contract, and the
// marker spellings here are the ones exec.ArtifactsComplete matches exactly.
const ContinuePrompt = `Continue executing BRIEF.md and PROCESS.md. Resume where you left off.
Do not stop early. Required artifacts (exact; Defect Drainer matches these, not prose):
- NOTES.md
- for each defect you claim fixed: fix-notes/<DEF-id>.md AND
  fix-evidence/<DEF-id>/fix-01.png
- to decline a defect, a full line in NOTES.md: UNFIXED: <DEF-id>
- if the sandbox is strict and you cannot screenshot, a full line:
  NO-SCREENSHOT: <DEF-id>
  (that stops the loop only; harvest will not resolve without a new image)
- if something outside your control blocks the work, a full line:
  BLOCKED: <reason>
  (that stops the loop; delete the line once the blocker is gone)
Leave your changes uncommitted; Defect Drainer commits them when the operator ships.
Do not invent other marker spellings. Do not delegate onward.`

// ContinuePromptBrowser is ContinuePrompt for a worker browser job
// (worker-browser B6): screenshots come from dd-shot, and NO-SCREENSHOT is
// for a dd-shot failure or an app without a preview only.
const ContinuePromptBrowser = `Continue executing BRIEF.md and PROCESS.md. Resume where you left off.
Do not stop early. Required artifacts (exact; Defect Drainer matches these, not prose):
- NOTES.md
- for each defect you claim fixed: fix-notes/<DEF-id>.md AND
  fix-evidence/<DEF-id>/fix-01.png
- take after-fix screenshots with: sh dd-shot <DEF-id> <path> [<path>...]
  (from cwd, in ONE Bash call; it serves the app and stops it again)
- to decline a defect, a full line in NOTES.md: UNFIXED: <DEF-id>
- only if dd-shot failed, or BRIEF.md says the app has no preview, a full line:
  NO-SCREENSHOT: <DEF-id>
  with the DD-SHOT FAIL line dd-shot printed quoted under it
  (that stops the loop only; harvest will not resolve without a new image)
- if something outside your control blocks the work, a full line:
  BLOCKED: <reason>
  (that stops the loop; delete the line once the blocker is gone)
Leave your changes uncommitted; Defect Drainer commits them when the operator ships.
Do not invent other marker spellings. Do not delegate onward.`

// CodingAgentOpts is extra spawn wiring (toolchain PATH, job-scoped sandbox
// profile, prompt notes). Nil is a no-op.
type CodingAgentOpts struct {
	// CgroupFD is an open cgroup v2 dir the child starts in (Linux
	// CLONE_INTO_CGROUP), so it and every descendant share the job's
	// memory, CPU and task limits. 0 starts it in the caller's cgroup.
	CgroupFD       int
	ExtraEnv       map[string]string
	SandboxProfile string
	ToolchainNotes []string
	VerifyCommands []VerifySpec
	AlreadyFailing []string
	// Continue resumes the most recent session for this cwd (-c) with
	// ContinuePrompt instead of sending the full brief prompt again.
	Continue bool
	// ResumeSession resumes that exact session (-r <id>) with ContinuePrompt.
	// An operator Resume uses it so the log names the session continued.
	// Wins over Continue.
	ResumeSession string
	// OnEnd receives the `end` event; OnMaxTurns fires on the separate
	// max_turns_reached event, which is how turn exhaustion is told apart from
	// a genuine failure (both exit non-zero).
	OnEnd      func(AgentEnd)
	OnMaxTurns func()
	// MaxTurns is the app's configured turn cap. Zero falls back to
	// MaxTurnsArg, then the built-in default.
	MaxTurns int
	// MaxTurnsArg is the host's turn-cap override, passed to grok verbatim
	// (the orchestrator reads DEFECT_DRAINER_GROK_BATCH_MAX_TURNS, then
	// GROK_BATCH_MAX_TURNS). Empty means the built-in default.
	MaxTurnsArg string
	// GrokBypassPermissions and GrokTools are the host's grok knobs
	// (DEFECT_DRAINER_GROK_BYPASS_PERMISSIONS, _GROK_TOOLS), read by the
	// orchestrator: this package reads no DD setting.
	GrokBypassPermissions bool
	GrokTools             string
	// Prompt, when set, replaces the batch-fix prompt. Empty keeps today's text.
	Prompt string
	// PromptSuffix is appended to the prompt sent, first or continue (job
	// messages: MESSAGES.md and the inbox). Empty changes nothing.
	PromptSuffix string
	// ContinuePrompt, when set, replaces ContinuePrompt on -c / -r. Empty
	// keeps the batch-fix continue prompt.
	ContinuePrompt string
	// Secret is redacted from agent.log. It is never written anywhere else
	// by this function. Empty disables the redactor.
	Secret string
	// ExtraSecrets are also redacted from agent.log (a job stack's {random}
	// value and sign-in password). Its caller redacts the job-log lines.
	ExtraSecrets []string
	// StripEnv are host environment names removed before ExtraEnv is applied.
	// DD_TEST_ACCOUNT_PASSWORD is always removed from the inherited environment.
	StripEnv []string
	// Adapter is the coding-agent CLI. Nil is grok (DefaultAdapter).
	Adapter agentadapter.Adapter
	// Model is the job's model, passed to the adapter's Task ("" keeps the
	// CLI's default).
	Model string
	// Credential, StateDir, Timeout and DenyRead are passed to the adapter's
	// Task.
	Credential agentadapter.Credential
	StateDir   string
	Timeout    time.Duration
	DenyRead   []string
	// ReadOnly are paths the agent may read but never edit (on a worker:
	// the job's repo clone .git and its report evidence). AllowedDomains are
	// the hosts its sandbox may reach. Both go to the adapter's Task.
	ReadOnly       []string
	AllowedDomains []string
	// DenyWrite goes to the adapter's Task (spec §4.1.11).
	DenyWrite []string
	// Browser is a worker browser job (worker-browser B1–B6): the adapter's
	// Task gets UnixSockets, the prompt names dd-shot, and the continue
	// prompt is ContinuePromptBrowser. The caller has added the host's
	// masked sockets to DenyRead. NoPreview says the app has no preview.
	Browser   bool
	NoPreview bool
}

// agentStream parses streaming-json (NDJSON) stdout **before** lineForwarder
// sees it. The order is load-bearing: lineForwarder caps a line at maxLogBlock
// mid-line, so unmarshalling its output would slice a JSON object in half, put
// thoughts in the job log before they could be filtered, and could lose the
// `end` event entirely. Every raw byte still reaches agent.log.
type agentStream struct {
	raw        io.Writer
	text       *lineForwarder
	errs       *lineForwarder
	onEnd      func(AgentEnd)
	onMaxTurns func()
	// onText receives each text payload before it is split into log lines.
	// Nil for the implementer. The reviewer reassembles findings from these
	// chunks; the log forwarder is the wrong source because it cuts a line
	// at maxLogBlock.
	onText func(string)
	// onTool receives tool_call and tool_call_update bodies. Nil for the
	// implementer. The reviewer uses them as proof a worktree diff was read.
	onTool func([]byte)
	pend   []byte
	// pendLimit caps a newline-less stdout blob. Zero keeps maxAgentLogBytes
	// so production stays at 8 MiB; tests set a smaller value.
	pendLimit int
	// textPend joins word-sized `text` deltas. Write runs on os/exec's copy
	// loop and Flush runs after Wait, so this buffer needs no lock.
	textPend []byte
	secret   []byte
	// dec decodes this CLI's stream. Nil is grok.
	dec agentadapter.Decoder
}

func (a *agentStream) Write(p []byte) (int, error) {
	if a.raw != nil {
		_, _ = a.raw.Write(p)
	}
	a.pend = append(a.pend, p...)
	for {
		i := bytes.IndexByte(a.pend, '\n')
		if i < 0 {
			// A child dumping a huge blob with no newline must not grow this
			// buffer without bound. Drop it with a marker rather than unmarshal.
			if len(a.pend) > a.pendCap() {
				// Prose already parsed must stay ahead of the marker. The
				// blob itself is dropped; agent.log already has the bytes.
				a.flushText()
				a.emit(a.text, logTruncateMark)
				a.pend = a.pend[:0]
			}
			break
		}
		line := append([]byte(nil), a.pend[:i]...)
		a.pend = a.pend[i+1:]
		a.handle(line)
	}
	return len(p), nil
}

func (a *agentStream) pendCap() int {
	if a != nil && a.pendLimit > 0 {
		return a.pendLimit
	}
	return maxAgentLogBytes
}

func (a *agentStream) emit(f *lineForwarder, line string) {
	if f == nil || line == "" {
		return
	}
	_, _ = f.Write([]byte(line + "\n"))
}

func (a *agentStream) handle(line []byte) {
	dec := a.dec
	if dec == nil {
		dec = grok.Decoder{}
	}
	for _, ev := range dec.Decode(line) {
		a.apply(ev)
	}
}

// apply acts on one decoded event. Prose is joined until a non-text event,
// so a callback or a stderr line always sees the prose before it.
func (a *agentStream) apply(ev agentadapter.Event) {
	switch ev.Kind {
	case agentadapter.Text:
		if a.onText != nil && ev.Text != "" {
			a.onText(ev.Text)
		}
		a.appendText(ev.Text)
	case agentadapter.EndEvent:
		a.flushText()
		if a.onEnd != nil {
			a.onEnd(ev.End)
		}
	case agentadapter.Tool:
		if a.onTool != nil {
			a.onTool(ev.Raw)
		}
		// The body stays in agent.log, not the job log.
		a.flushText()
	case agentadapter.MaxTurns:
		// The positive signal for turn exhaustion, which also exits non-zero.
		a.flushText()
		if a.onMaxTurns != nil {
			a.onMaxTurns()
		}
	case agentadapter.Error:
		a.flushText()
		a.emit(a.errs, "agent error: "+ev.Text)
	case agentadapter.Raw:
		a.flushText()
		a.emit(a.text, ev.Text)
	case agentadapter.Invalid:
		// A truncated tool_call_update is still one `{` line, tens of KB.
		// The bytes are in agent.log; the job log gets a length, not the body.
		a.flushText()
		a.emit(a.text, fmt.Sprintf("invalid agent event (%d bytes; see agent.log)", len(ev.Raw)))
	default:
		// thoughts, usage, and whatever a later version adds. agent.log keeps
		// them; job.log stays readable. Flush first so prose before the event
		// is one line.
		a.flushText()
	}
}

func (a *agentStream) appendText(payload string) {
	if payload == "" {
		return
	}
	a.textPend = append(a.textPend, payload...)
	if len(a.secret) > 0 {
		a.textPend = bytes.ReplaceAll(a.textPend, a.secret, []byte(redactedMark))
	}
	a.takeTextLines()
}

// takeTextLines emits completed lines and any maxLogBlock chunk. A short
// partial stays buffered. Newline-terminated lines keep their own spacing;
// a line that is only whitespace is dropped.
func (a *agentStream) textLimit(flush bool) int {
	n := len(a.textPend)
	if !flush && len(a.secret) > 1 {
		keep := len(a.secret) - 1
		if n <= keep {
			return 0
		}
		return n - keep
	}
	return n
}

func (a *agentStream) takeTextLines() {
	a.takeText(false)
}

func (a *agentStream) takeText(flush bool) {
	limit := a.textLimit(flush)
	for limit > 0 {
		end := limit
		if end > maxLogBlock {
			end = maxLogBlock
		}
		if i := bytes.IndexByte(a.textPend[:end], '\n'); i >= 0 {
			line := string(a.textPend[:i])
			a.textPend = a.textPend[i+1:]
			limit = a.textLimit(flush)
			if strings.TrimSpace(line) != "" {
				a.emit(a.text, line)
			}
			continue
		}
		if !flush && limit < maxLogBlock {
			break
		}
		if len(a.textPend) < maxLogBlock {
			break
		}
		cut := textCut(a.textPend)
		if cut < 1 {
			cut = 1
		}
		chunk := string(a.textPend[:cut])
		a.textPend = a.textPend[cut:]
		limit = a.textLimit(flush)
		if strings.TrimSpace(chunk) != "" {
			a.emit(a.text, chunk)
		}
	}
	// Drop the backing array of a huge delta once the live partial is short,
	// so one long event cannot pin megabytes for the rest of the run.
	if cap(a.textPend) > maxLogBlock {
		a.textPend = append([]byte(nil), a.textPend...)
	}
}

// textCut is the next chunk length for a buffer that has reached maxLogBlock
// with no newline in that window. If a UTF-8 rune straddles the cap, the cut
// moves back at most 3 bytes to the rune start so the partial rune stays
// buffered. A window of continuation bytes still returns at least one byte.
func textCut(buf []byte) int {
	cut := maxLogBlock
	if cut > len(buf) {
		cut = len(buf)
	}
	if cut == 0 {
		return 0
	}
	if cut < len(buf) && !utf8.RuneStart(buf[cut]) {
		for n := 1; n <= 3 && cut-n > 0; n++ {
			if utf8.RuneStart(buf[cut-n]) {
				return cut - n
			}
		}
	}
	return cut
}

// flushText emits the short partial as one trimmed line.
func (a *agentStream) flushText() {
	if len(a.secret) > 0 {
		a.textPend = bytes.ReplaceAll(a.textPend, a.secret, []byte(redactedMark))
	}
	a.takeText(true)
	if s := strings.TrimSpace(string(a.textPend)); s != "" {
		a.emit(a.text, s)
	}
	a.textPend = nil
}

// Flush handles a trailing line with no newline, so a final `end` on a stream
// that does not end in \n is still recorded. Pending prose is emitted after
// that line is parsed: a trailing text event still joins the partial, and a
// trailing end / max_turns_reached / error flushes inside handle before its
// callback or stderr write.
func (a *agentStream) Flush() {
	if len(a.pend) > 0 {
		line := a.pend
		a.pend = nil
		a.handle(line)
	}
	a.flushText()
}

func buildBatchFixCLIPrompt(handoffAbs, batchID, defectsRoot, sandbox string, worktrees []WorktreeBinding, opts *CodingAgentOpts) string {
	wtLines := make([]string, 0, len(worktrees))
	if len(worktrees) == 0 {
		wtLines = []string{"- (none — do not edit product code)"}
	} else {
		for _, w := range worktrees {
			wtLines = append(wtLines, "- "+w.Repo+": EDIT ONLY "+w.WorktreeAbs+" (branch "+w.Branch+"); NEVER "+w.PrimaryAbs)
		}
	}
	// A job profile denies its own cwd/.grok (it holds the profile), so cwd is
	// writable except there.
	cwdWrite := "cwd (" + handoffAbs + ")"
	if opts != nil && opts.SandboxProfile != "" {
		cwdWrite = "cwd (" + handoffAbs + ") except its .grok/ (the locked job sandbox profile — no read or write)"
	}
	var sandboxNotes []string
	if opts != nil && opts.Adapter != nil && opts.Adapter.Name() != agentadapter.Grok {
		sandboxNotes = adapterSandboxNotes(opts.Adapter, handoffAbs, defectsRoot, opts.ReadOnly, opts.AllowedDomains)
	} else if sandbox == "workspace" {
		sandboxNotes = []string{
			"SANDBOX: --sandbox workspace (Project settings).",
			"- Read: host filesystem (iOS Simulator, Xcode, simctl data allowed).",
			"- Write: " + cwdWrite + ", ~/.grok/, and temp only.",
			"- Product code: edit only under cwd/worktrees/…",
			"- Visual check / fix screenshots: capture under cwd/fix-evidence/<DEF-id>/fix-01.png (handoff-writable).",
			"- Inventory root " + defectsRoot + " is readable — open report + **historical fix evidence** paths listed in BRIEF.md.",
		}
	} else {
		sandboxNotes = []string{
			"SANDBOX: --sandbox strict / restrict (Project settings).",
			"- Read: cwd + system paths only (no ~/Library Simulator data).",
			"- Write: " + cwdWrite + ", ~/.grok/, and temp.",
			"- Product code: edit only under cwd/worktrees/…",
			"- Historical fix evidence may be unreadable under strict if paths are outside cwd — prefer workspace sandbox for visual history.",
			"- Inventory updates under " + defectsRoot + " may be blocked; write new proof to handoff fix-evidence/.",
		}
	}
	parts := []string{
		"Batch defect fix session for " + batchID + ".",
		"Handoff directory (cwd): " + handoffAbs,
		"Inventory root: " + defectsRoot,
		"",
	}
	parts = append(parts, sandboxNotes...)
	if opts != nil && opts.Browser {
		parts = append(parts, "")
		parts = append(parts, browserPromptNotes(opts.NoPreview)...)
	}
	if opts != nil && len(opts.ToolchainNotes) > 0 {
		parts = append(parts, "")
		parts = append(parts, opts.ToolchainNotes...)
	}
	if opts != nil && len(opts.VerifyCommands) > 0 {
		parts = append(parts, "",
			"VERIFICATION (run by Defect Drainer after you exit — not by you, and not editable):",
		)
		for _, v := range opts.VerifyCommands {
			parts = append(parts, "- "+v.Repo+": "+v.Command)
		}
		parts = append(parts,
			"- A command you BREAK blocks the resolve, however good your screenshots are.",
			"- Run them yourself in the worktree before claiming DONE; fix what you broke.",
			"- Do not report test results you did not actually observe.",
		)
		if len(opts.AlreadyFailing) > 0 {
			parts = append(parts,
				"- ALREADY FAILING before you started (measured, not your doing —",
				"  do not chase these unless the defect is about them):",
			)
			for _, f := range opts.AlreadyFailing {
				parts = append(parts, "    · "+f)
			}
		} else {
			parts = append(parts, "- All of them passed before you started, so any failure after is yours.")
		}
	}
	parts = append(parts, "",
		"WORKTREE ENFORCEMENT (mandatory):",
	)
	parts = append(parts, wtLines...)
	parts = append(parts,
		"- Product edits outside listed worktree paths are forbidden.",
		"- Do not checkout branches on primary trees. Do not run git commands in primaryAbs.",
		"- READ each worktree's own contract file (AGENTS.md / CLAUDE.md — BRIEF.md lists",
		"  the exact paths) BEFORE editing it. They define that repo's mandatory gates,",
		"  skills and conventions; where stricter than this brief, they win. Follow its",
		"  house style — do not reformat code the fix does not need to touch.",
		"",
		"ROLE (SKILL_FORGE_AGENT_ROLE=worker is set on this process):",
		"- You are the worker for this job, not an orchestrator. Do not delegate onward.",
		"- Read, in this order, BEFORE the first product edit: BRIEF.md, PROCESS.md,",
		"  SKILLS.md, then each contract file SKILLS.md lists for the worktree you touch.",
		"- SKILLS.md carries absolute paths because cwd is the handoff, not a product repo;",
		"  directory-based skill discovery will not find them. If SKILLS.md lists none for a",
		"  worktree, use security-baseline, coding-discipline, code-quality and testing-strategy.",
		"- Classify each defect's blast radius (Tier 1/2/3) in NOTES.md, then proceed:",
		"  the operator starting this batch IS the approval. Do not wait to be approved.",
		"- Run the verification commands yourself inside the worktree. DD re-runs them after",
		"  you exit; a result you claim but did not observe is a defect in your notes, not a pass.",
		"- Leave your changes uncommitted in the worktree branch already checked out; Defect",
		"  Drainer commits them when the operator ships. Do not commit, push or switch branches.",
		"- If something outside your control blocks the work (a refused path, no network, a",
		"  missing tool), write a full line in NOTES.md: BLOCKED: <reason>. Do not work around it.",
		"",
		"Read BRIEF.md first. For each defect it lists:",
		"- report evidence (bug as reported)",
		"- historical fix evidence (prior proof on the defect SSOT — use as context / regression baseline)",
		"- what to deliver this run under handoff fix-evidence/ and fix-notes/",
		"",
		"TASK:",
		"1. For each defect: read report + historical fix evidence from BRIEF paths; implement fixes only under worktree paths.",
		"2. Verify (tests / smoke / visual) inside the worktree checkout; compare to historical fix shots when present.",
		"3. REQUIRED **new** deliverables under this cwd (backend harvests onto the defect):",
		"   - fix-evidence/<DEF-id>/fix-01.png (and more images as needed)",
		"   - fix-notes/<DEF-id>.md  (what changed + how verified; note relation to prior fix if any)",
		"   - NOTES.md (plan + tier per defect, decisions, departures, what you could not verify)",
		"   - fix-notes must answer EACH acceptance criterion listed for that defect in BRIEF.md.",
		"     Those criteria are the definition of fixed — not your own reading of the title.",
		"     If one cannot be met, say so explicitly instead of omitting it.",
		"4. Visual check when sandbox=workspace: Simulator / app screenshots into fix-evidence/.",
		"5. Do not claim DONE without those **handoff** files for each fixed defect (historical inventory files alone are not enough for this run).",
		"6. Reply: DONE "+batchID,
	)
	return strings.Join(parts, "\n")
}

// FilterInheritedEnv is the environment for a non-agent child (verify
// commands, the toolchain probe): agentadapter.ChildEnv with no credential
// kept. It drops every DEFECT_DRAINER_*, DEFECT_CHANNEL_* and DEFECTS_ROOT
// name, DD_TEST_ACCOUNT_PASSWORD, every coding-agent credential, and extra.
func FilterInheritedEnv(environ, extra []string) []string {
	return filterInheritedEnv(environ, extra)
}

// dropListedEnv removes strip-list keys after the child env is fully built.
// keep retains the test job's intentional DD_TEST_ACCOUNT_PASSWORD assignment.
func dropListedEnv(environ, strip []string, keep map[string]bool) []string {
	drop := map[string]bool{}
	for _, name := range strip {
		if name != "" && !keep[name] {
			drop[name] = true
		}
	}
	if len(drop) == 0 {
		return environ
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if ok && drop[key] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func filterInheritedEnv(environ, extra []string) []string {
	return agentadapter.ChildEnv(environ, nil, extra)
}

// StartCodingAgent execs the resolved bin. Empty worktrees refuse spawn.
// sandbox is the app grok_sandbox setting; empty falls back to strict.
// opts may carry toolchain env, a job-scoped sandbox profile name, and prompt
// notes. onStdout / onStderr receive complete lines (stdout info, stderr warn);
// both streams are still appended to handoff/agent.log. flush drains any
// trailing partial line and closes the log file — call it after Wait.
func StartCodingAgent(handoffAbs, batchID, defectsRoot string, worktrees []WorktreeBinding, sandbox string, onStdout, onStderr func(string), opts *CodingAgentOpts) (*exec.Cmd, func(), error) {
	if len(worktrees) == 0 {
		return nil, nil, fmt.Errorf("refusing to spawn agent batch fix without worktrees")
	}
	adapter := DefaultAdapter()
	if opts != nil && opts.Adapter != nil {
		adapter = opts.Adapter
	}
	sandbox = normalizeSandbox(sandbox)
	prompt := buildBatchFixCLIPrompt(handoffAbs, batchID, defectsRoot, sandbox, worktrees, opts)
	if opts != nil && opts.Prompt != "" {
		prompt = opts.Prompt
	}
	// App Settings wins, then the host override (either env spelling, read
	// by the orchestrator), then the built-in default. A Flutter repo burns
	// turns far faster than a Node one — each pub get, analyze and test is a
	// turn — so this is per app, not per install. The override has always
	// reached grok verbatim.
	task := agentadapter.Task{Role: agentadapter.Implementer, Handoff: handoffAbs, Sandbox: sandbox}
	if opts != nil {
		task.Model = opts.Model
	}
	if opts != nil && opts.MaxTurns > 0 {
		task.MaxTurns = opts.MaxTurns
	} else if opts != nil && opts.MaxTurnsArg != "" {
		task.MaxTurnsArg = opts.MaxTurnsArg
	} else {
		task.MaxTurnsArg = defaultBatchMaxTurns
	}
	if task.MaxTurns == 0 {
		// Grok still gets the env value verbatim (MaxTurnsArg); other
		// adapters need a number, so a non-numeric value means the default.
		if n, err := strconv.Atoi(task.MaxTurnsArg); err == nil && n > 0 {
			task.MaxTurns = n
		} else {
			task.MaxTurns, _ = strconv.Atoi(defaultBatchMaxTurns)
		}
	}
	// Iteration 2+ resumes with the fixed continue prompt. Everything else
	// about the argv is identical, so the sandbox and turn budget cannot drift
	// between iterations.
	task.Prompt = prompt
	cont := ContinuePrompt
	if opts != nil && opts.Browser {
		cont = ContinuePromptBrowser
	}
	if opts != nil && opts.ContinuePrompt != "" {
		cont = opts.ContinuePrompt
	}
	if opts != nil && opts.ResumeSession != "" {
		task.Prompt = cont
		task.ResumeID = opts.ResumeSession
	} else if opts != nil && opts.Continue {
		task.Prompt = cont
		task.Continue = true
	}
	if opts != nil {
		task.SandboxProfile = opts.SandboxProfile
		task.Credential = opts.Credential
		task.StateDir = opts.StateDir
		task.Timeout = opts.Timeout
		task.DenyRead = opts.DenyRead
		task.ReadOnly = opts.ReadOnly
		task.AllowedDomains = opts.AllowedDomains
		task.DenyWrite = opts.DenyWrite
		task.UnixSockets = opts.Browser
		for _, v := range opts.VerifyCommands {
			task.AllowCommands = append(task.AllowCommands, v.Command)
		}
	}
	if opts != nil {
		task.GrokBypassPermissions = opts.GrokBypassPermissions
		task.GrokTools = opts.GrokTools
	}
	if opts != nil && opts.PromptSuffix != "" {
		task.Prompt += "\n\n" + opts.PromptSuffix
	}
	for _, w := range worktrees {
		task.Worktrees = append(task.Worktrees, w.WorktreeAbs)
	}
	// SKILL_FORGE_AGENT_ROLE marks the child as the worker end of the
	// delegation (the adapter sets it). Never set it on the serve process: a
	// leaked value would tell a human session started from the same shell
	// that it is a worker. Inherited DD_TEST_ACCOUNT_PASSWORD and any app
	// env: secret names are removed here. ExtraEnv may set
	// DD_TEST_ACCOUNT_PASSWORD again for a test job; a fix job never does.
	var strip []string
	if opts != nil {
		strip = opts.StripEnv
	}
	task.Env = childBaseEnv(adapter, strip)
	if err := adapter.Prepare(task); err != nil {
		return nil, nil, err
	}
	spec, err := adapter.Command(task)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(spec.Bin, spec.Args...)
	// stdin is /dev/null (os/exec's nil): a CLI that reads a piped stdin,
	// as claude -p does, sees EOF at once instead of waiting for data.
	cmd.Stdin = nil
	cmd.Dir = spec.Dir
	// Own process group so descendants die with the iteration. A background
	// sleep or a swap loop otherwise outlives the exit and can still race
	// the server's review git.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if opts != nil {
		IntoCgroup(cmd.SysProcAttr, opts.CgroupFD)
	}
	// Descendants inherit the stdout pipe. After the leader exits, Wait must
	// not block on them; the caller SIGKILLs the group once Wait returns.
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = spec.Env
	if opts != nil {
		for k, v := range opts.ExtraEnv {
			if k == "" {
				continue
			}
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	// The one DD variable a child sees, set deliberately and non-secret: the
	// grok child has always had it (criterion 1). Other adapters get none.
	if adapter.Name() == agentadapter.Grok {
		cmd.Env = append(cmd.Env, "DEFECT_DRAINER_WORKTREES="+strings.Join(task.Worktrees, ":"))
	}
	keep := map[string]bool{}
	if opts != nil {
		if _, ok := opts.ExtraEnv["DD_TEST_ACCOUNT_PASSWORD"]; ok {
			keep["DD_TEST_ACCOUNT_PASSWORD"] = true
		}
	}
	cmd.Env = dropListedEnv(cmd.Env, strip, keep)
	outFwd := newLineForwarder(onStdout)
	errFwd := newLineForwarder(filterStderr(adapter, onStderr))
	stream := &agentStream{text: outFwd, errs: errFwd, dec: adapter.NewDecoder()}
	if opts != nil && opts.Secret != "" {
		sec := []byte(opts.Secret)
		outFwd.secret = sec
		errFwd.secret = sec
		stream.secret = sec
	}
	if opts != nil {
		stream.onEnd = opts.OnEnd
		stream.onMaxTurns = opts.OnMaxTurns
	}
	logf, err := openAgentLog(handoffAbs)
	// reds are the agent.log redactors, innermost first.
	var reds []*redactWriter
	if err == nil {
		var start int64
		if st, serr := logf.Stat(); serr == nil {
			start = st.Size()
		}
		tee := &cappedWriter{w: logf, n: start, limit: maxAgentLogBytes}
		// stdout is NDJSON and goes through the parser, which owns the raw tee.
		// stderr is plain text and keeps the old path. Each stream redacts on
		// its own so a secret split across two writes of that stream is caught.
		var rawOut io.Writer = tee
		var rawErr io.Writer = tee
		var secrets []string
		if opts != nil {
			secrets = append([]string{opts.Secret}, opts.ExtraSecrets...)
		}
		for _, sec := range secrets {
			if sec == "" {
				continue
			}
			o, e := newRedactWriter(rawOut, sec), newRedactWriter(rawErr, sec)
			reds = append(reds, o, e)
			rawOut, rawErr = o, e
		}
		stream.raw = rawOut
		cmd.Stdout = stream
		cmd.Stderr = io.MultiWriter(rawErr, errFwd)
	} else {
		cmd.Stdout = stream
		cmd.Stderr = errFwd
	}
	// The adapter's credential is redacted before anything else sees a byte:
	// the decoder, agent.log and the job log.
	flushKeys := redactCredential(cmd, task.Credential.Key)
	flush := func() {
		flushKeys()
		// Outermost first, so each held tail still passes the inner ones.
		for i := len(reds) - 1; i >= 0; i-- {
			_ = reds[i].Flush()
		}
		stream.Flush()
		outFwd.Flush()
		errFwd.Flush()
		if logf != nil {
			_ = logf.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		flush()
		return nil, nil, err
	}
	return cmd, flush, nil
}

// reviewTextLimit matches exec.HarvestFileLimit. A reviewer payload larger
// than this is rejected whole; the cap here only keeps the collector from
// growing without bound when the caller does not pass one.
const reviewTextLimit = 26 << 20

// ReviewAgentOpts is a fresh reviewer spawn. It never resumes a session.
type ReviewAgentOpts struct {
	// CgroupFD is the job's cgroup, as CodingAgentOpts.CgroupFD.
	CgroupFD int
	Cwd      string
	Prompt   string
	MaxTurns int
	// TextLimit caps the reassembled assistant text. <= 0 uses reviewTextLimit.
	TextLimit int
	// Wall is how long the child may run before it is killed. <= 0 disables
	// the killer; the jobs orchestrator always passes a positive duration.
	Wall     time.Duration
	StripEnv []string
	OnStdout func(string)
	OnStderr func(string)
	// Adapter is the coding-agent CLI. Nil is grok (DefaultAdapter).
	Adapter agentadapter.Adapter
	// Model is the job's model: reviewers run on it too.
	Model string
	// Worktrees are the job's worktree paths (read-only for the reviewer).
	Worktrees []string
	// ReadOnly are extra paths the reviewer may read (the job's notes).
	ReadOnly   []string
	Credential agentadapter.Credential
	StateDir   string
	DenyRead   []string
	DenyWrite  []string
	// Chat makes this a follow-up answer (agentadapter.Chat): Cwd is the
	// job's handoff and ResumeID the implementer's session, which it
	// resumes read-only. A reviewer leaves both unset.
	Chat     bool
	ResumeID string
	// Register runs after Start and before Wait so the caller can publish the
	// process. The returned func is the single Wait — pass spawnedProc.reap,
	// because Cmd.Wait must not run twice. Nil uses Cmd.Wait.
	Register func(cmd *exec.Cmd) (wait func() error)
}

// ReviewResult is one reviewer process after it has exited. Text is every
// streaming-json text payload concatenated, in order. Deadline is set when
// Wall killed the child.
type ReviewResult struct {
	Text     string
	End      AgentEnd
	Stderr   string
	TooLarge bool
	Deadline bool
	Err      error
	// ToolEvents are the tool_call and tool_call_update payloads, in order.
	ToolEvents [][]byte
}

// StartReviewAgent execs a reviewer that cannot resume the implementer.
// Argv is built here and nowhere else: no -c, no -r, no --resume, sandbox
// always read-only. The caller creates Cwd (handoff/review/<lens>, never the
// handoff itself). Findings come back in ReviewResult.Text; this process
// does not expect the child to write a file.
func StartReviewAgent(opts ReviewAgentOpts) (ReviewResult, error) {
	if opts.Cwd == "" {
		return ReviewResult{}, fmt.Errorf("reviewer cwd is empty")
	}
	adapter := DefaultAdapter()
	if opts.Adapter != nil {
		adapter = opts.Adapter
	}
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 8
	}
	limit := opts.TextLimit
	if limit <= 0 {
		limit = reviewTextLimit
	}
	prompt := opts.Prompt
	if prompt == "" {
		prompt = "Read REVIEW.md and reply with the findings JSON object it describes."
	}
	// Fresh process: the adapter's reviewer argv never resumes, and its
	// sandbox and permissions are read-only.
	role := agentadapter.Reviewer
	if opts.Chat {
		if opts.ResumeID == "" {
			return ReviewResult{}, fmt.Errorf("chat run has no session to resume")
		}
		role = agentadapter.Chat
	}
	task := agentadapter.Task{
		Role:       role,
		ResumeID:   opts.ResumeID,
		Handoff:    opts.Cwd,
		Worktrees:  opts.Worktrees,
		ReadOnly:   opts.ReadOnly,
		Prompt:     prompt,
		Model:      opts.Model,
		MaxTurns:   maxTurns,
		Sandbox:    "read-only",
		Credential: opts.Credential,
		StateDir:   opts.StateDir,
		DenyRead:   opts.DenyRead,
		DenyWrite:  opts.DenyWrite,
		Timeout:    opts.Wall,
		Env:        childBaseEnv(adapter, opts.StripEnv),
	}
	if err := adapter.Prepare(task); err != nil {
		return ReviewResult{}, err
	}
	spec, err := adapter.Command(task)
	if err != nil {
		return ReviewResult{}, err
	}
	cmd := exec.Command(spec.Bin, spec.Args...)
	// stdin is /dev/null (os/exec's nil): a CLI that reads a piped stdin,
	// as claude -p does, sees EOF at once instead of waiting for data.
	cmd.Stdin = nil
	cmd.Dir = spec.Dir
	// Own process group so a wall or Stop kill reaches git/shell grandchildren.
	// The implementer spawn does not do this.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	IntoCgroup(cmd.SysProcAttr, opts.CgroupFD)
	cmd.Env = spec.Env

	var text strings.Builder
	var stderr strings.Builder
	var tools [][]byte
	tooLarge := false
	var end AgentEnd
	outFwd := newLineForwarder(opts.OnStdout)
	errFwd := newLineForwarder(func(line string) {
		if line = adapter.FilterStderr(line); line == "" {
			return
		}
		stderr.WriteString(line)
		stderr.WriteByte('\n')
		if opts.OnStderr != nil {
			opts.OnStderr(line)
		}
	})
	stream := &agentStream{text: outFwd, errs: errFwd, dec: adapter.NewDecoder()}
	stream.onText = func(s string) {
		if tooLarge || s == "" {
			return
		}
		if text.Len()+len(s) > limit {
			tooLarge = true
			return
		}
		text.WriteString(s)
	}
	stream.onEnd = func(e AgentEnd) { end = e }
	stream.onTool = func(raw []byte) { tools = append(tools, raw) }

	logf, logErr := openAgentLog(opts.Cwd)
	if logErr == nil {
		var start int64
		if st, serr := logf.Stat(); serr == nil {
			start = st.Size()
		}
		tee := &cappedWriter{w: logf, n: start, limit: maxAgentLogBytes}
		stream.raw = tee
		cmd.Stdout = stream
		cmd.Stderr = io.MultiWriter(tee, errFwd)
	} else {
		cmd.Stdout = stream
		cmd.Stderr = errFwd
	}
	flushKeys := redactCredential(cmd, task.Credential.Key)
	flush := func() {
		flushKeys()
		stream.Flush()
		outFwd.Flush()
		errFwd.Flush()
		if logf != nil {
			_ = logf.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		flush()
		return ReviewResult{}, err
	}
	trackReviewProc(cmd)
	defer forgetReviewProc(cmd)
	wait := cmd.Wait
	if opts.Register != nil {
		if w := opts.Register(cmd); w != nil {
			wait = w
		}
	}
	var killed atomic.Bool
	var timer *time.Timer
	killedDone := make(chan struct{})
	if opts.Wall > 0 {
		timer = time.AfterFunc(opts.Wall, func() {
			if KillReviewProcess(cmd) {
				killed.Store(true)
			}
			close(killedDone)
		})
	}
	waitErr := wait()
	// The reaped flag is set before any late wall callback. A kill that
	// loses the race with Wait must not signal a pid the kernel has reused.
	markReviewReaped(cmd)
	if reviewAfterReapHook != nil {
		reviewAfterReapHook(cmd)
	}
	if timer != nil && !timer.Stop() {
		<-killedDone
	}
	flush()
	res := ReviewResult{
		Text:       text.String(),
		End:        end,
		Stderr:     stderr.String(),
		TooLarge:   tooLarge,
		Err:        waitErr,
		ToolEvents: tools,
	}
	if killed.Load() && waitErr != nil {
		res.Deadline = true
	}
	return res, nil
}

// reviewProcGate is one reviewer's reap state. Kill paths share it so a
// wall callback that runs after Wait does not signal a reused process group.
type reviewProcGate struct {
	mu     sync.Mutex
	reaped bool
}

var reviewProcs sync.Map // *exec.Cmd -> *reviewProcGate

// reviewAfterReapHook runs after Wait has returned and the reaped flag is
// set. Tests use it to deliver the wall callback late.
var reviewAfterReapHook func(cmd *exec.Cmd)

// signalReviewGroup is the process-group SIGKILL. Tests replace it.
var signalReviewGroup = func(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func trackReviewProc(cmd *exec.Cmd) {
	reviewProcs.Store(cmd, &reviewProcGate{})
}

func forgetReviewProc(cmd *exec.Cmd) {
	reviewProcs.Delete(cmd)
}

func markReviewReaped(cmd *exec.Cmd) {
	v, ok := reviewProcs.Load(cmd)
	if !ok {
		return
	}
	st := v.(*reviewProcGate)
	st.mu.Lock()
	st.reaped = true
	st.mu.Unlock()
}

// KillImplementerGroup SIGKILLs an implementer process group after its leader
// has been reaped. Children keep the group id alive, so the kernel will not
// reuse it while they exist. signal 0 skips a group that is already empty.
// The residual race is the gap between that probe and the SIGKILL: the last
// member can exit and the id can be reused before the kill lands.
func KillImplementerGroup(pid int) {
	if pid <= 0 {
		return
	}
	v, _ := implGroupMu.LoadOrStore(pid, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

var implGroupMu sync.Map // pid -> *sync.Mutex

// KillReviewProcess SIGKILLs the reviewer's process group. It returns false
// once the child has been reaped, and sends nothing. StartReviewAgent puts
// the child in its own group; killing only the leader leaves git and shell
// grandchildren behind. Do not use this on the implementer.
func KillReviewProcess(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	if v, ok := reviewProcs.Load(cmd); ok {
		st := v.(*reviewProcGate)
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.reaped {
			return false
		}
		_ = signalReviewGroup(cmd.Process.Pid)
		return true
	}
	_ = signalReviewGroup(cmd.Process.Pid)
	return true
}
