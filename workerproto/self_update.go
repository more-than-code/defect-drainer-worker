package workerproto

// Self-update (worker protocol v1 §23, docs/adr/0002-self-update.md).
// Additive to v1.

// CapSelfUpdateV1: the worker has the self-update code (whatever its policy
// says). The server sends hello_ok.target_release only to a worker that
// lists it.
const CapSelfUpdateV1 = "self_update.v1"

// Self-update modes (policy auto_update) and states.
const (
	SelfUpdateOff    = "off"
	SelfUpdateServer = "server"

	SelfUpdateIdle     = "idle"     // nothing to do, or waiting for the window
	SelfUpdateDraining = "draining" // new offers declined busy until running jobs end
	SelfUpdateUpdating = "updating" // the request is with the host's root step
	SelfUpdateCooldown = "cooldown" // the last try of this target did not succeed
)

// Results of one update (SelfUpdateResult.Result).
const (
	SelfUpdateSucceeded  = "succeeded"
	SelfUpdateRolledBack = "rolled_back"
	SelfUpdateRefused    = "refused"
	SelfUpdateFailed     = "failed"
)

// SelfUpdateStatus is hello.self_update and status.self_update, for
// display. Target is the server's target_release as this worker last saw
// it; Reason says why an idle worker is not updating to it (for example
// "off", "not_newer", "outside_window").
type SelfUpdateStatus struct {
	Mode   string            `json:"mode"`
	State  string            `json:"state"`
	Target string            `json:"target,omitempty"`
	Reason string            `json:"reason,omitempty"`
	Last   *SelfUpdateResult `json:"last,omitempty"`
}

// SelfUpdateResult is the host's root step's last result. At is RFC 3339.
type SelfUpdateResult struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"`
}
