package workerproto

// Sandboxed verify (worker protocol v1 §19). Additive to v1.

// CapVerifySandboxV1: the worker runs a project's verify commands in its
// verify sandbox, and its startup self-test passed. A server offers a job
// whose project has verify commands only to a worker that lists it and
// whose policy verify_commands isn't "off".
const CapVerifySandboxV1 = "verify_sandbox.v1"

// VerifySkippedSandboxUnavailable is JobSpec.project.verifySkipped and
// VerificationRun.skippedReason when the verify sandbox cannot start. The
// checks are then never run, sandboxed or not.
const VerifySkippedSandboxUnavailable = "verify_sandbox_unavailable"

// VerifySandboxStatus is hello.verify_sandbox: the last self-test, for
// display. Ready is true exactly when the hello lists CapVerifySandboxV1;
// otherwise Reason is "<token>: <detail>" (for example "bwrap: not found").
// A top-level hello field, not a tools entry: servers before 0.2.0-rc9
// refuse unknown tool names but ignore unknown fields.
type VerifySandboxStatus struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}
