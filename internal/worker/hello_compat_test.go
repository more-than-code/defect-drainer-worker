package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// The hello and policy shapes of workerproto at v0.2.0-rc8, frozen: what a
// server before rc9 decodes a hello into. workerproto's Validate is
// unchanged since rc8 (rc9 only adds fields).
type rc8Policy struct {
	Valid               bool     `json:"valid"`
	Enabled             bool     `json:"enabled"`
	Orgs                []string `json:"orgs"`
	Adapters            []string `json:"adapters"`
	AllowVerifyCommands bool     `json:"allow_verify_commands"`
	AllowShip           bool     `json:"allow_ship"`
	Repos               []string `json:"repos"`
	MinSandbox          string   `json:"min_sandbox"`
	MaxTurns            int      `json:"max_turns"`
	MaxWallSec          int      `json:"max_wall_sec"`
	AllowedDomains      []string `json:"allowed_domains"`
	MaxCostUSD          int      `json:"max_cost_usd"`
	Browser             string   `json:"browser"`
	MaskedSockets       []string `json:"masked_sockets"`
	AllowedModels       []string `json:"allowed_models,omitempty"`
	Postgres            string   `json:"postgres,omitempty"`
	Hash                string   `json:"hash,omitempty"`
	ManagedURL          string   `json:"managed_url,omitempty"`
}

type rc8Hello struct {
	workerproto.Header
	WorkerID     string                      `json:"worker_id"`
	Version      string                      `json:"version"`
	Protocol     int                         `json:"protocol"`
	Capabilities []string                    `json:"capabilities"`
	MaxJobs      int                         `json:"max_jobs"`
	BusyJobIDs   []string                    `json:"busy_job_ids"`
	Adapters     []workerproto.AdapterStatus `json:"adapters"`
	Tools        []workerproto.ToolStatus    `json:"tools,omitempty"`
	Policy       rc8Policy                   `json:"policy"`
}

// An rc9 hello, ready or not, still passes an rc8-era server: its new
// fields (verify_sandbox, policy.verify_commands) are ignored on decode,
// tools[] keeps the old names, and the rest validates.
func TestHelloRC9AcceptedByRC8Server(t *testing.T) {
	for _, c := range []struct {
		name string
		find func(context.Context) (sandbox.Launcher, error)
	}{
		{"ready", nil},
		{"unavailable", func(context.Context) (sandbox.Launcher, error) {
			return sandbox.Launcher{}, fmt.Errorf("%w: bwrap: not found", sandbox.ErrUnavailable)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{verify: c.find})
			frames := h.fs.frames(workerproto.TypeHello)
			raw := frames[len(frames)-1].raw
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(raw, &probe); err != nil || probe["verify_sandbox"] == nil {
				t.Fatalf("rc9 hello lacks verify_sandbox: %s", raw)
			}
			var old rc8Hello
			if err := json.Unmarshal(raw, &old); err != nil {
				t.Fatalf("rc8 decode: %v", err)
			}
			// What the rc8 server validates: the same fields, re-encoded
			// from its own struct, so nothing rc9 added survives.
			b, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			var f workerproto.Hello
			if err := json.Unmarshal(b, &f); err != nil {
				t.Fatal(err)
			}
			if f.VerifySandbox != nil || f.Policy.VerifyCommands != "" {
				t.Fatalf("rc9 fields leaked through the rc8 shape: %s", b)
			}
			if err := f.Validate(); err != nil {
				t.Fatalf("an rc8 server would refuse this hello: %v\n%s", err, raw)
			}
			if err := workerproto.ValidateTools(old.Tools); err != nil {
				t.Fatalf("tools: %v", err)
			}
		})
	}
}
