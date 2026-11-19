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
}

func snapshotPath(p Paths) string { return p.StatusFile() }

// writeSnapshot records the connection state and the latest readiness.
func (w *Worker) writeSnapshot(connected bool) {
	w.mu.Lock()
	s := Snapshot{
		Connected: connected, Since: time.Now().UTC().Format(time.RFC3339),
		Server: w.cfg.ServerURL, WorkerID: w.cfg.WorkerID, OrgID: w.cfg.OrgID, Version: Version,
		Adapters: append([]workerproto.AdapterStatus(nil), w.statuses...),
		Policy:   w.policy.Wire(), PolicyWhy: w.policy.Reason, MaxJobs: w.policy.MaxJobs,
	}
	w.mu.Unlock()
	s.Busy = w.busyIDs()
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
