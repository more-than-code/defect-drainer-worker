// Command dd-worker runs Defect Drainer fix and test jobs on the host
// owner's machine, dialling out to the server over worker protocol v1
// (docs/contracts/worker-protocol-v1.md).
//
//	dd-worker init --server <origin> --join-token-stdin
//	dd-worker run
//	dd-worker status
//	dd-worker policy check [--syntax-only] [--file <path|->]
//	dd-worker secrets check --file <path|->
//	dd-worker health --wait <dur> | --idle
//
// Every command takes --config-dir (default /etc/dd-worker) and --state-dir
// (default /var/lib/dd-worker).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/cgroup"
	"github.com/more-than-code/defect-drainer-worker/internal/shot"
	"github.com/more-than-code/defect-drainer-worker/internal/worker"
)

// version is set via -ldflags "-X main.version=…".
var version = ""

// Exit codes.
const (
	exitOK      = 0
	exitFail    = 1
	exitUsage   = 2
	exitRevoked = 3
	exitFatal   = 4 // replaced, protocol or org mismatch: the owner must act
)

// Exit codes of the candidate checks (policy check --file, secrets check)
// and of health, a contract a host manager such as Deploybot relies on
// (docs/host-setup.md): usage and I/O errors are exitFail.
const (
	exitInvalid   = 2 // one bad_* line on stderr
	exitUnhealthy = 3 // health --wait timed out; one reason line on stderr
	exitBusy      = 4 // health --idle: a job is running
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if version != "" {
		worker.Version = version
	}
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:], stdin, stdout, stderr)
	case "run":
		return cmdRun(args[1:], stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "policy":
		if len(args) < 2 || args[1] != "check" {
			usage(stderr)
			return exitUsage
		}
		return cmdPolicyCheck(args[2:], stdin, stdout, stderr)
	case "secrets":
		if len(args) < 2 || args[1] != "check" {
			usage(stderr)
			return exitFail
		}
		return cmdSecretsCheck(args[2:], stdin, stdout, stderr)
	case "health":
		return cmdHealth(args[1:], stderr)
	case "shot":
		return cmdShot(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, worker.Version)
		return exitOK
	default:
		usage(stderr)
		return exitUsage
	}
}

// geteuid is os.Geteuid; tests replace it.
var geteuid = os.Geteuid

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  dd-worker init --server <https origin> --join-token-stdin [--insecure-dev]
  dd-worker run [--claude-bin P] [--claude-config-dir D] [--grok-bin P] [--grok-home D] [--agy-bin P] [--agy-home D]
  dd-worker status
  dd-worker policy check [--syntax-only] [--file <path|->]
  dd-worker secrets check --file <path|->
  dd-worker health --wait <duration> | --idle
common flags: --config-dir (default /etc/dd-worker) --state-dir (default /var/lib/dd-worker)
init runs as the worker user, never root (sudo -u dd-worker dd-worker init ...)
`)
}

func pathFlags(fs *flag.FlagSet) *worker.Paths {
	p := &worker.Paths{}
	fs.StringVar(&p.ConfigDir, "config-dir", config.DefaultConfigDir, "config dir, never written by the worker (policy.toml, secrets.env)")
	fs.StringVar(&p.StateDir, "state-dir", config.DefaultStateDir, "state dir (credential/, repos, jobs, spool)")
	return p
}

func cmdInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	server := fs.String("server", "", "server origin, e.g. https://api.defectdrainer.com")
	fromStdin := fs.Bool("join-token-stdin", false, "read the dd_wjn_ join token from stdin (required)")
	insecure := fs.Bool("insecure-dev", false, "allow http://127.0.0.1 (the Mac dev loop with AUTH=none)")
	machineID := fs.String("machine-id-file", "", "machine id file (default /etc/machine-id)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *server == "" || !*fromStdin || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "init needs --server and --join-token-stdin (the token is never an argument)")
		return exitUsage
	}
	if geteuid() == 0 {
		// Root would write a credential the worker user cannot read.
		fmt.Fprintf(stderr, "init: do not run init as root; run it as the worker user:\n  sudo -u dd-worker dd-worker init --server %s --join-token-stdin\n", *server)
		return exitFail
	}
	token, err := config.ReadJoinToken(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := worker.Enroll(ctx, worker.EnrollOptions{
		Paths: *p, Server: *server, JoinToken: token, InsecureDev: *insecure, MachineIDFile: *machineID,
	})
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return exitFail
	}
	fmt.Fprintf(stdout, "enrolled: worker %s in org %s; credential written to %s (mode 0600)\n", cfg.WorkerID, cfg.OrgID, cfg.Path)
	return exitOK
}

func cmdRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	var a worker.AdapterConfig
	fs.StringVar(&a.ClaudeBin, "claude-bin", "", "claude binary (default: PATH)")
	fs.StringVar(&a.ClaudeConfigDir, "claude-config-dir", "", "claude's logged-in config dir (subscription mode)")
	fs.StringVar(&a.GrokBin, "grok-bin", "", "grok binary (default: PATH)")
	fs.StringVar(&a.GrokHome, "grok-home", "", "grok's state dir (default ~/.grok)")
	fs.StringVar(&a.AgyBin, "agy-bin", "", "agy binary (default: PATH)")
	fs.StringVar(&a.AgyHome, "agy-home", "", "the HOME agy is signed in under")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// systemd's LoadCredential= sets CREDENTIALS_DIRECTORY (a path, never
	// the key) when the unit hands the worker its secrets.env copy.
	// scrubOwnEnv then drops it, so no child learns where the copy is.
	p.CredentialsDir = os.Getenv("CREDENTIALS_DIRECTORY")
	scrubOwnEnv()
	w, err := worker.New(worker.Options{Paths: *p, Adapters: a, Log: stderr, Cgroups: cgroup.Open})
	if err != nil {
		fmt.Fprintln(stderr, "run:", err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = w.Run(ctx)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, worker.ErrRevoked):
		fmt.Fprintln(stderr, "run:", err)
		return exitRevoked
	default:
		fmt.Fprintln(stderr, "run:", err)
		return exitFatal
	}
}

// scrubOwnEnv drops DD_WORKER_* and CREDENTIALS_DIRECTORY from this
// process before anything starts: the credential is read from its file,
// and no child should inherit it or the path to the secrets.env copy.
func scrubOwnEnv() {
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && (strings.HasPrefix(k, "DD_WORKER_") || k == "CREDENTIALS_DIRECTORY") {
			_ = os.Unsetenv(k)
		}
	}
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	code := exitOK
	cfg, err := config.Load(p.Env())
	if err != nil {
		fmt.Fprintf(stdout, "enrolment:  not enrolled (%v)\n", err)
		if _, lerr := os.Lstat(p.LegacyEnv()); lerr == nil {
			fmt.Fprintf(stdout, "            a credential is still at %s: dd-worker run moves it to %s\n", p.LegacyEnv(), p.CredentialDir())
		}
		code = exitFail
	} else {
		org := cfg.OrgID
		if org == "" {
			org = "?"
		}
		fmt.Fprintf(stdout, "enrolment:  worker %s, org %s, server %s\n", cfg.WorkerID, org, cfg.ServerURL)
		if _, err := os.Stat(p.EnvPrev()); err == nil {
			fmt.Fprintln(stdout, "            a rotation is pending (env.prev present)")
		}
	}
	pol := worker.LoadPolicy(p.Policy(), false, nil)
	if pol.Valid {
		fmt.Fprintf(stdout, "policy:     valid, enabled=%v, orgs=%s, adapters=%s, repos=%d pattern(s), min_sandbox=%s, max_turns=%d, max_wall_sec=%d, max_jobs=%d, verify=%s, allow_ship=%v\n",
			pol.Enabled, strings.Join(pol.Orgs, ","), strings.Join(pol.Adapters, ","), len(pol.Repos), pol.MinSandbox,
			pol.MaxTurns, pol.MaxWallSec, pol.MaxJobs, pol.VerifyCommands, pol.AllowShip)
		printPolicyDeprecations(stdout, pol, "            ")
	} else {
		fmt.Fprintf(stdout, "policy:     NOT VALID (%s): every offer is declined\n", pol.Reason)
	}
	snap, err := worker.ReadSnapshot(*p)
	if err != nil {
		fmt.Fprintln(stdout, "connection: unknown (dd-worker run has not written a status yet)")
		return code
	}
	state := "disconnected"
	if snap.Connected {
		state = "connected"
	}
	fmt.Fprintf(stdout, "connection: %s since %s (version %s)\n", state, snap.Since, snap.Version)
	for _, a := range snap.Adapters {
		ready := "ready"
		if !a.Ready {
			ready = "not ready: " + a.Reason
		}
		v := a.Version
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(stdout, "adapter:    %-12s %-10s %-13s %s\n", a.Name, v, a.CredentialMode, ready)
	}
	busy := "none"
	if len(snap.Busy) > 0 {
		busy = strings.Join(snap.Busy, ", ")
	}
	fmt.Fprintf(stdout, "busy jobs:  %s\n", busy)
	if snap.VerifySandbox != "" {
		fmt.Fprintf(stdout, "verify sandbox: %s\n", snap.VerifySandbox)
	}
	if snap.JobLimits != "" {
		fmt.Fprintf(stdout, "job limits: %s\n", snap.JobLimits)
	}
	return code
}

// printPolicyLines are policy check's lines after "valid": the verify
// setting, then any deprecated key the file still sets.
func printPolicyLines(stdout io.Writer, pol worker.Policy) {
	fmt.Fprintf(stdout, "verify=%s\n", pol.VerifyCommands)
	printPolicyDeprecations(stdout, pol, "")
}

func printPolicyDeprecations(stdout io.Writer, pol worker.Policy, indent string) {
	if pol.AllowVerifySet {
		fmt.Fprintf(stdout, "%sdeprecated: allow_verify_commands (use verify_commands)\n", indent)
	}
}

func cmdPolicyCheck(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	file := fs.String("file", "", "candidate policy file, or - for stdin (exit 0 valid, 2 one bad_* line on stderr, 1 usage or I/O)")
	syntaxOnly := fs.Bool("syntax-only", false, "skip the root-owner and mode checks (a file not yet installed)")
	if err := fs.Parse(args); err != nil {
		if namesFileFlag(args) {
			return exitFail
		}
		return exitUsage
	}
	if *file == "" {
		// Without --file: today's check of the installed policy.
		path := p.Policy()
		pol := worker.LoadPolicy(path, *syntaxOnly, nil)
		if !pol.Valid {
			fmt.Fprintf(stdout, "%s: invalid (%s)\n", path, pol.Reason)
			return exitFail
		}
		fmt.Fprintf(stdout, "%s: valid (enabled=%v, sha256 %s)\n", path, pol.Enabled, pol.Hash)
		printPolicyLines(stdout, pol)
		return exitOK
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "policy check: unexpected arguments")
		return exitFail
	}
	var pol worker.Policy
	name := *file
	if name == "-" {
		data, err := readCandidate(stdin, policyMaxBytes)
		if err != nil {
			fmt.Fprintln(stderr, "policy check: cannot read stdin")
			return exitFail
		}
		name = "stdin"
		pol = worker.CheckPolicyData(data)
	} else {
		pol = worker.LoadPolicy(name, *syntaxOnly, nil)
		if !pol.Exists || pol.Reason == "unreadable" {
			fmt.Fprintf(stderr, "policy check: cannot read %s\n", name)
			return exitFail
		}
	}
	if !pol.Valid {
		fmt.Fprintln(stderr, badReason(pol.Reason))
		return exitInvalid
	}
	fmt.Fprintf(stdout, "%s: valid (enabled=%v, sha256 %s)\n", name, pol.Enabled, pol.Hash)
	printPolicyLines(stdout, pol)
	return exitOK
}

// cmdShot is dd-shot's browser driver for a Stack sign-in (job stack S10),
// run by dd-shot inside the agent's sandbox. Not listed in usage: nothing
// else calls it.
//
//	dd-worker shot --browser B --config F --def D --dir OUT --after N --url APP --timeout S -- PATH...
func cmdShot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	browser := fs.String("browser", "", "the host browser")
	cfg := fs.String("config", "", "the Stack sign-in file ("+exec.ShotSignInName+")")
	def := fs.String("def", "", "the defect id")
	dir := fs.String("dir", "", "where the screenshots go")
	after := fs.Int("after", 0, "the highest fix-NN already there")
	appURL := fs.String("url", "", "the app's URL")
	timeout := fs.Int("timeout", 60, "seconds per page")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *browser == "" || *cfg == "" || *def == "" || *dir == "" || *appURL == "" || fs.NArg() == 0 || *after < 0 || *timeout < 1 {
		fmt.Fprintln(stderr, "shot needs --browser, --config, --def, --dir, --url and at least one path")
		return exitUsage
	}
	si, err := exec.ReadShotSignIn(*cfg)
	if err != nil {
		fmt.Fprintf(stdout, "DD-SHOT FAIL %s signin no-config\n", *def)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return shot.Run(ctx, shot.Options{Browser: *browser, SignIn: si, Def: *def, Dir: *dir, After: *after,
		AppURL: strings.TrimSuffix(*appURL, "/"), Paths: fs.Args(), Timeout: time.Duration(*timeout) * time.Second, Out: stdout})
}

// policyMaxBytes is one more than a policy may hold, so an oversized
// candidate reaches the parser and is refused there.
const policyMaxBytes = 64*1024 + 1

func cmdSecretsCheck(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("secrets check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "candidate secrets.env, or - for stdin (exit 0 valid, 2 one bad_* line on stderr, 1 usage or I/O)")
	if err := fs.Parse(args); err != nil {
		return exitFail
	}
	if *file == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "secrets check needs --file <path|->")
		return exitFail
	}
	var reason string
	if *file == "-" {
		data, err := readCandidate(stdin, worker.MaxSecretsBytes+1)
		if err != nil {
			fmt.Fprintln(stderr, "secrets check: cannot read stdin")
			return exitFail
		}
		reason = worker.CheckSecretsData(data)
	} else {
		r, err := worker.CheckSecretsFile(*file, nil)
		if err != nil {
			fmt.Fprintf(stderr, "secrets check: cannot read %s\n", *file)
			return exitFail
		}
		reason = r
	}
	if reason != "" {
		fmt.Fprintln(stderr, badReason(reason))
		return exitInvalid
	}
	fmt.Fprintln(stdout, "secrets: valid")
	return exitOK
}

// healthPoll is how often health --wait re-reads the status file.
var healthPoll = 500 * time.Millisecond

// maxHealthWait bounds health --wait.
const maxHealthWait = 10 * time.Minute

func cmdHealth(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	wait := fs.Duration("wait", 0, "wait up to this long for a running, connected worker with a valid, enabled policy (exit 0, or 3 and one reason line)")
	idle := fs.Bool("idle", false, "exit 4 (busy) while any job is running, else 0")
	if err := fs.Parse(args); err != nil {
		return exitFail
	}
	if fs.NArg() != 0 || (*wait != 0) == *idle || *wait < 0 || *wait > maxHealthWait {
		fmt.Fprintln(stderr, "health needs exactly one of --wait <duration> (up to 10m) or --idle")
		return exitFail
	}
	if *idle {
		if worker.Busy(*p) {
			fmt.Fprintln(stderr, "busy")
			return exitBusy
		}
		return exitOK
	}
	deadline := time.Now().Add(*wait)
	for {
		reason := worker.HealthReason(*p)
		if reason == "" {
			return exitOK
		}
		if !time.Now().Add(healthPoll).Before(deadline) {
			fmt.Fprintln(stderr, reason)
			return exitUnhealthy
		}
		time.Sleep(healthPoll)
	}
}

// readCandidate reads at most limit bytes of a candidate from r.
func readCandidate(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// badReasonRe is the reason line contract of the candidate checks.
var badReasonRe = regexp.MustCompile(`^bad_[a-z0-9_]{1,64}$`)

// badReason is a check reason as its bad_* line: reasons the worker reports
// without the prefix (parse, unknown_key, orgs_empty, not_root, ...) gain it.
func badReason(reason string) string {
	if !strings.HasPrefix(reason, "bad_") {
		reason = "bad_" + reason
	}
	if !badReasonRe.MatchString(reason) {
		return "bad_unknown"
	}
	return reason
}

// namesFileFlag reports whether args use --file, so a usage error follows
// the candidate-check contract (exit 1) instead of policy check's own.
func namesFileFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if n := strings.TrimLeft(a, "-"); a != n && (n == "file" || strings.HasPrefix(n, "file=")) {
			return true
		}
	}
	return false
}
