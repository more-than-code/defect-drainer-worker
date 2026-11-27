package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/release"
	"github.com/more-than-code/defect-drainer-worker/internal/selfupdate"
	"github.com/more-than-code/defect-drainer-worker/internal/worker"
)

// The installed layout install.sh keeps (packaging/install.sh).
const (
	installedLink = "/usr/local/bin/dd-worker"
	installedLib  = "/usr/local/lib/dd-worker"
	// rootPath is the only PATH the root step's commands get.
	rootPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	// nobody runs a downloaded binary's `version` before it is installed.
	nobodyID = 65534
	// updateBudget bounds one run (the unit's RuntimeMaxSec is 900 s).
	updateBudget = 14 * time.Minute
)

// cmdUpdateRun is `dd-worker update-run`: the host's root step of
// self-update (docs/adr/0002-self-update.md §6.2), started by
// dd-worker-update.path when the worker writes its request. It always
// exits 0 once it has written a result: the result is in result.json, and
// a failing unit would only hit systemd's start limit.
func cmdUpdateRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("update-run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := pathFlags(fs)
	rootDir := fs.String("root-dir", selfupdate.DefaultRootDir, "root's self-update dir (result.json, staging)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	if geteuid() != 0 {
		fmt.Fprintln(stderr, "update-run runs as root, from dd-worker-update.service")
		return exitFail
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		fmt.Fprintln(stderr, "update-run: dd-worker hosts are linux arm64 or amd64")
		return exitFail
	}
	if err := os.MkdirAll(*rootDir, 0o755); err != nil {
		fmt.Fprintln(stderr, "update-run:", err)
		return exitFail
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateBudget)
	defer cancel()
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	pol := worker.LoadPolicy(p.Policy(), false, nil)
	env := selfupdate.Env{
		StateDir: p.StateDir, RootDir: *rootDir, LibDir: installedLib, LockPath: selfupdate.DefaultLock,
		MinInterval: time.Minute,
		Arch:        runtime.GOARCH, Keys: release.PublicKeys,
		Installed: installedVersion,
		Policy: func() selfupdate.HostPolicy {
			return selfupdate.HostPolicy{Valid: pol.Valid, Mode: pol.AutoUpdate, Window: pol.AutoUpdateWindow, Proxy: pol.AutoUpdateProxy}
		},
		Busy:      func(context.Context) bool { return worker.Busy(*p) },
		Fetch:     selfupdate.HTTPFetcher{Proxy: pol.AutoUpdateProxy},
		VersionOf: versionAsNobody,
		Install: func(ctx context.Context, script, version, base string) (int, string, error) {
			return runRoot(ctx, []string{"DD_WORKER_RELEASE_BASE=" + base}, "/bin/sh", script, "upgrade", version)
		},
		Healthy: func(ctx context.Context) error {
			code, out, err := runRoot(ctx, nil, installedLink, "health", "--wait", "60s",
				"--config-dir", p.ConfigDir, "--state-dir", p.StateDir)
			if err != nil || code != 0 {
				return fmt.Errorf("health exit %d: %s %v", code, strings.TrimSpace(out), err)
			}
			return nil
		},
		Rollback: func(ctx context.Context) error {
			cur, err := installedVersion()
			if err != nil {
				return err
			}
			code, out, err := runRoot(ctx, nil, "/bin/sh", filepath.Join(installedLib, cur, "install.sh"), "rollback")
			if err != nil || code != 0 {
				return fmt.Errorf("install.sh rollback exit %d: %v\n%s", code, err, out)
			}
			return nil
		},
		Logf: logf,
	}
	if _, err := selfupdate.Run(ctx, env); err != nil {
		logf("update-run: %v", err)
		return exitFail
	}
	return exitOK
}

// installedVersion reads the version /usr/local/bin/dd-worker points at.
func installedVersion() (string, error) {
	link, err := os.Readlink(installedLink)
	if err != nil {
		return "", err
	}
	v, ok := strings.CutPrefix(link, installedLib+"/")
	if !ok {
		return "", fmt.Errorf("%s points outside %s", installedLink, installedLib)
	}
	v, ok = strings.CutSuffix(v, "/dd-worker")
	if !ok || strings.Contains(v, "/") {
		return "", fmt.Errorf("%s points at %s", installedLink, link)
	}
	return v, nil
}

// versionAsNobody runs `<bin> version` as nobody with an empty environment.
func versionAsNobody(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	cmd.Env = []string{"PATH=" + rootPath}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobodyID, Gid: nobodyID}}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// runRoot runs a command as root with a fixed environment and returns its
// exit code and combined output.
func runRoot(ctx context.Context, extraEnv []string, name string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append([]string{"PATH=" + rootPath, "LANG=C.UTF-8", "HOME=/root"}, extraEnv...)
	cmd.Dir = "/"
	cmd.WaitDelay = 2 * time.Minute
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String(), nil
	}
	if err != nil {
		return -1, out.String(), err
	}
	return 0, out.String(), nil
}
