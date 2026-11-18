// Command dd-worker runs Defect Drainer fix and test jobs on the host
// owner's machine, dialling out to the server over worker protocol v1
// (docs/contracts/worker-protocol-v1.md).
//
//	dd-worker init --server <origin> --join-token-stdin
//	dd-worker run
//	dd-worker status
//	dd-worker policy check [--syntax-only]
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
	"strings"
	"syscall"

	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
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
		return cmdPolicyCheck(args[2:], stdout, stderr)
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
  dd-worker policy check [--syntax-only]
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
	server := fs.String("server", "", "server origin, e.g. https://defectdrainer.com")
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
	scrubOwnEnv()
	w, err := worker.New(worker.Options{Paths: *p, Adapters: a, Log: stderr})
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

// scrubOwnEnv drops DD_WORKER_* from this process before anything starts:
// the credential is read from its file, and no child should inherit it.
func scrubOwnEnv() {
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "DD_WORKER_") {
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
		fmt.Fprintf(stdout, "policy:     valid, enabled=%v, orgs=%s, adapters=%s, repos=%d pattern(s), min_sandbox=%s, max_turns=%d, max_wall_sec=%d, max_jobs=%d, allow_verify_commands=%v, allow_ship=%v\n",
			pol.Enabled, strings.Join(pol.Orgs, ","), strings.Join(pol.Adapters, ","), len(pol.Repos), pol.MinSandbox,
			pol.MaxTurns, pol.MaxWallSec, pol.MaxJobs, pol.AllowVerifyCommands, pol.AllowShip)
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
	return code
}

func cmdPolicyCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	file := fs.String("file", "", "policy file (default <config-dir>/policy.toml)")
	syntaxOnly := fs.Bool("syntax-only", false, "skip the root-owner and mode checks (a file not yet installed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	path := *file
	if path == "" {
		path = p.Policy()
	}
	pol := worker.LoadPolicy(path, *syntaxOnly, nil)
	if !pol.Valid {
		fmt.Fprintf(stdout, "%s: invalid (%s)\n", path, pol.Reason)
		return exitFail
	}
	fmt.Fprintf(stdout, "%s: valid (enabled=%v, sha256 %s)\n", path, pol.Enabled, pol.Hash)
	return exitOK
}
