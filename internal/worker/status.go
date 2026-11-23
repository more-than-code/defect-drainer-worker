package worker

import (
	"encoding/json"
	"os"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Snapshot is {state}/status.json, written by run for dd-worker status. It
// holds no credential, token or secret value.
type Snapshot struct {
	Connected bool                        `json:"connected"`
	Since     string                      `json:"since"`
	Server    string                      `json:"server"`
	WorkerID  string                      `json:"workerId"`
	OrgID     string                      `json:"orgId"`
	Version   string                      `json:"version"`
	Adapters  []workerproto.AdapterStatus `json:"adapters"`
	Policy    workerproto.Policy          `json:"policy"`
	PolicyWhy string                      `json:"policyReason,omitempty"`
	MaxJobs   int                         `json:"maxJobs"`
	Busy      []string                    `json:"busyJobIds"`
	// Tools is the last tools readiness (git, and browser with
	// agent_browser), as hello and status send it.
	Tools []workerproto.ToolStatus `json:"tools,omitempty"`
	// VerifySandbox is the last verify-sandbox self-test: "ready" or
	// "unavailable (<reason>)".
	VerifySandbox string `json:"verifySandbox,omitempty"`
	// JobLimits are the per-job cgroup limits: "memory 3 GiB, 4096 tasks
	// per job" or "off (<reason>)".
	JobLimits string `json:"jobLimits,omitempty"`
}

func snapshotPath(p Paths) string { return p.StatusFile() }

// writeSnapshot records the connection state and the latest readiness.
func (w *Worker) writeSnapshot(connected bool) {
	pol := w.wirePolicy(w.policyNow())
	w.mu.Lock()
	s := Snapshot{
		Connected: connected, Since: time.Now().UTC().Format(time.RFC3339),
		Server: w.cfg.ServerURL, WorkerID: w.cfg.WorkerID, OrgID: w.cfg.OrgID, Version: Version,
		Adapters: append([]workerproto.AdapterStatus(nil), w.statuses...),
		Policy:   pol, PolicyWhy: w.policy.Reason, MaxJobs: w.policy.MaxJobs,
	}
	w.mu.Unlock()
	s.Busy = w.busyIDs()
	s.Tools = w.toolsNow()
	s.VerifySandbox = verifySandboxLine(w.verifyNow().err)
	s.JobLimits = w.limitsLine(w.policyNow())
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = config.WriteFileAtomic(snapshotPath(w.opts.Paths), b, 0o600, 0o700)
}

// ReadSnapshot reads the last snapshot run wrote.
func ReadSnapshot(p Paths) (Snapshot, error) {
	b, err := os.ReadFile(snapshotPath(p))
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	err = json.Unmarshal(b, &s)
	return s, err
}

// policyNow is the current policy.
func (w *Worker) policyNow() Policy {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.policy
}
