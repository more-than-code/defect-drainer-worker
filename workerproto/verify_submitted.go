package workerproto

// Verify a submitted ref (worker protocol v1 §16): a batch fix whose code
// came from outside Defect Drainer. The worker fetches the ref, measures the
// baseline at the base, and runs the checks, diff hygiene and the reviewer
// with no coding agent. It never pushes. Additive to v1.

// CapVerifySubmittedV1: the worker understands JobSpec.mode
// "verify_submitted", JobSpec.submittedRefs and Status.submittedHeads.
const CapVerifySubmittedV1 = "verify_submitted.v1"

// Job reasons (§9) a verify_submitted run can fail with before any check.
const (
	// ReasonSubmittedRefMissing: the remote has no such ref, or a SHA ref
	// did not resolve to that commit. Line: "submitted_ref_missing: <repo> <ref>".
	ReasonSubmittedRefMissing = "submitted_ref_missing"
	// ReasonNoMergeBase: the submitted commit shares no history with the
	// base. Line: "no_merge_base: <repo> <ref>".
	ReasonNoMergeBase = "no_merge_base"
)
