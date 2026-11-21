package worker

// Health reasons (dd-worker health --wait), most basic first.
const (
	HealthNotRunning    = "not_running"
	HealthPolicyInvalid = "policy_invalid"
	HealthNotConnected  = "not_connected"
)

// HealthReason reads the status file run writes and returns "" when the
// running worker has loaded the policy.toml now on disk, that policy is
// valid and enabled, and the worker is connected; otherwise the first
// failing reason. A missing status file, or one whose policy hash differs
// from the file on disk (a worker that has not loaded it, or one that
// stopped without saying so), is not_running. It reads and never writes.
func HealthReason(p Paths) string {
	snap, err := ReadSnapshot(p)
	if err != nil {
		return HealthNotRunning
	}
	if snap.Policy.Hash != LoadPolicy(p.Policy(), true, nil).Hash {
		return HealthNotRunning
	}
	if !snap.Policy.Valid || !snap.Policy.Enabled {
		return HealthPolicyInvalid
	}
	if !snap.Connected {
		return HealthNotConnected
	}
	return ""
}

// Busy reports whether the status file lists a running job (dd-worker
// health --idle). No status file means no worker, so nothing is running.
func Busy(p Paths) bool {
	snap, err := ReadSnapshot(p)
	return err == nil && len(snap.Busy) > 0
}
