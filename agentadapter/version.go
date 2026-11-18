package agentadapter

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// VersionState is what the startup probe found.
type VersionState string

const (
	VersionOK      VersionState = "ok"
	VersionMissing VersionState = "missing"
	VersionTooOld  VersionState = "too-old"
)

// Probe is one adapter's install check.
type Probe struct {
	Adapter Name
	State   VersionState
	// Found is the parsed version, "" when missing or unparsable.
	Found string
	Min   string
}

// Marker is the startup-line value: claude@2.1.292, claude@missing,
// claude@too-old.
func (p Probe) Marker() string {
	if p.State == VersionOK {
		return string(p.Adapter) + "@" + p.Found
	}
	return string(p.Adapter) + "@" + string(p.State)
}

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// ParseVersion takes the first x.y.z in a --version output.
func ParseVersion(out string) string {
	return semverRe.FindString(out)
}

// AtLeast compares dotted numeric versions. An unparsable found is false.
func AtLeast(found, min string) bool {
	f, okF := splitVersion(found)
	m, okM := splitVersion(min)
	if !okF || !okM {
		return false
	}
	for i := 0; i < 3; i++ {
		if f[i] != m[i] {
			return f[i] > m[i]
		}
	}
	return true
}

func splitVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ProbeVersion runs `<bin> <VersionArgs>` with env (the caller's child env)
// and classifies the result. It never fails: a missing or broken CLI is a
// state, because serve starts without an agent.
func ProbeVersion(ctx context.Context, a Adapter, env []string) Probe {
	p := Probe{Adapter: a.Name(), Min: a.MinVersion(), State: VersionMissing}
	bin := a.Bin()
	if bin == "" {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, a.VersionArgs()...)
	// A nil Env would inherit serve's whole environment.
	if env == nil {
		env = []string{}
	}
	cmd.Env = env
	// Some CLIs print their version to stderr.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return p
	}
	p.Found = ParseVersion(string(out))
	if p.Found == "" {
		return p
	}
	if AtLeast(p.Found, p.Min) {
		p.State = VersionOK
	} else {
		p.State = VersionTooOld
	}
	return p
}

// LookBin returns override when set, else the first existing candidate, else
// a PATH lookup of name. Adapters call it with the override the caller read.
func LookBin(override, name string, candidates ...string) string {
	if override != "" {
		return override
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}
