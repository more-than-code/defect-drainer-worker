package workerproto

// Job stack (docs/job-stack.md in the control plane, decisions
// S1–S9): an app's job stack (a throwaway Postgres and the app's
// services) booted inside the agent's sandbox by the dd-stack helper.
// Additive to v1.

// CapJobStackV1: the worker understands JobSpec.Project.jobStack,
// previewEnv and previewTimeoutSec, and declines a job whose services need
// Postgres when its policy has no agent_postgres. Whether Postgres is ready
// is the "postgres" tool's readiness, not this capability.
const CapJobStackV1 = "job_stack.v1"

// ToolPostgres is the tools[] entry for the host's Postgres binaries (policy
// agent_postgres). A worker sends it only when the policy sets
// agent_postgres: a server before job_stack.v1 refuses the name
// (ValidateTools). Not-ready reasons: missing, unsafe.
const ToolPostgres = "postgres"

// DetailNoPostgres is the policy_denied decline detail for a job whose
// services need Postgres on a worker whose policy has no agent_postgres
// (S8). A Postgres that is set but not ready declines with
// "postgres not ready: <reason>".
const DetailNoPostgres = "no Postgres (policy)"
