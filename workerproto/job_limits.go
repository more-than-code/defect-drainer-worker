package workerproto

// Per-job resource limits (worker protocol v1 §20). Additive to v1.

// CapJobLimitsV1: the worker runs each job in its own cgroup with the
// limits in hello.job_limits. Listed only while they are enforced.
const CapJobLimitsV1 = "job_limits.v1"

// JobLimitsStatus is hello.job_limits, for display. Enforced is true
// exactly when the hello lists CapJobLimitsV1; the limits are then the ones
// each job's cgroup gets (0: not limited). Otherwise Reason says why, for
// example "not running as a systemd service".
type JobLimitsStatus struct {
	Enforced    bool   `json:"enforced"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
	Tasks       int64  `json:"tasks,omitempty"`
	CPUPercent  int    `json:"cpu_percent,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
