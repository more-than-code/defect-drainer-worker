// Package workerproto holds the wire types of worker protocol v1
// (docs/contracts/worker-protocol-v1.md, frozen 2026-11-15): frames, codes,
// limits and the validators both the worker (cmd/dd-worker) and the server
// hub use. It is plain data: it imports exec for the embedded job types and
// nothing from store, db, api, jobs, analytics or feedback (boundary_test.go).
//
// Decoding is lenient on unknown keys (encoding/json's default) and strict on
// required keys: every frame type has Validate, which a receiver calls after
// Unmarshal and answers with Error{Code: CodeBadFrame} when it fails (§2).
package workerproto

import (
	"encoding/json"

	"github.com/more-than-code/defect-drainer-worker/exec"
)

// Version is the envelope's "v" (§1). Protocol is the highest protocol
// number this build speaks (§4.1).
const (
	Version  = 1
	Protocol = 1
)

// Frame types (§3–§6).
const (
	TypeEnroll        = "enroll"
	TypeEnrolled      = "enrolled"
	TypeHello         = "hello"
	TypeHelloOK       = "hello_ok"
	TypePing          = "ping"
	TypePong          = "pong"
	TypeStatus        = "status"
	TypeRotated       = "rotated"
	TypeRotateAck     = "rotate_ack"
	TypeOffer         = "offer"
	TypeAccept        = "accept"
	TypeDecline       = "decline"
	TypeLease         = "lease"
	TypeProgress      = "progress"
	TypeAck           = "ack"
	TypeControl       = "control"
	TypeControlResult = "control_result"
	TypeControlAck    = "control_ack"
	TypeError         = "error"
)

// Envelope is decoded first to dispatch on Type; the receiver then decodes
// the same bytes into the frame struct.
type Envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// Header is embedded in every frame.
type Header struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// NewHeader stamps v and type.
func NewHeader(t string) Header { return Header{V: Version, Type: t} }

// --- Enrolment (§3) ---

type Enroll struct {
	Header
	Hostname      string `json:"hostname"`
	MachineIDHash string `json:"machine_id_hash"`
	Version       string `json:"version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
}

// Enrolled carries the credential once. Never log it (§11).
type Enrolled struct {
	Header
	WorkerID   string `json:"worker_id"`
	OrgID      string `json:"org_id"`
	Credential string `json:"credential"`
}

// --- Session (§4) ---

// AdapterStatus is one coding-agent CLI's readiness on the host, checked
// under the exact child env a job would get.
type AdapterStatus struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	CredentialMode string `json:"credential_mode"`
	Ready          bool   `json:"ready"`
	MinOK          bool   `json:"min_ok"`
	Reason         string `json:"reason,omitempty"`
}

// Adapter not-ready reasons (§4.1).
const (
	ReasonMissing       = "missing"
	ReasonTooOld        = "too_old"
	ReasonSignedOut     = "signed_out"
	ReasonNoKey         = "no_key"
	ReasonUnsupportedOS = "unsupported_os"
)

// ToolStatus is one repo tool's readiness on the host (git, gh), checked
// under the env the worker's own clone and push get. Display only: the
// server does not schedule on it (§4.1).
type ToolStatus struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"`
}

// Policy mirrors /etc/dd-worker/policy.toml for scheduling and display. The
// worker enforces its own copy regardless (§6.5).
type Policy struct {
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
	// Additive in 0.2.0-rc3 (worker browser, B4): the host's ceiling for
	// display. AllowedDomains is policy agent_allowed_domains, MaxCostUSD
	// policy max_cost_usd (0: unset), Browser policy agent_browser ("": off)
	// and MaskedSockets the host sockets a browser job's sandbox hides.
	AllowedDomains []string `json:"allowed_domains"`
	MaxCostUSD     int      `json:"max_cost_usd"`
	Browser        string   `json:"browser"`
	MaskedSockets  []string `json:"masked_sockets"`
	// Additive in 0.2.0-rc4: policy allowed_models, the models a job may
	// run on (at most MaxAllowedModels, exact names). Absent: any model.
	AllowedModels []string `json:"allowed_models,omitempty"`
	// Additive in 0.2.0-rc6 (job stack, S8): policy agent_postgres,
	// the directory of the host's Postgres binaries ("": off).
	Postgres string `json:"postgres,omitempty"`
	// Additive in 0.2.0-rc6: Hash is the sha256 (hex) of the policy.toml
	// bytes the worker loaded, valid or not ("": no file); ManagedURL is
	// policy managed_url, where the host's manager edits the file, sent
	// only with a valid policy.
	Hash       string `json:"hash,omitempty"`
	ManagedURL string `json:"managed_url,omitempty"`
	// Additive in 0.2.0-rc9 (sandboxed verify): policy verify_commands,
	// "sandbox" or "off" ("" with an invalid policy). AllowVerifyCommands
	// is then verify_commands == "sandbox", for servers before it.
	VerifyCommands string `json:"verify_commands,omitempty"`
}

type Hello struct {
	Header
	WorkerID     string          `json:"worker_id"`
	Version      string          `json:"version"`
	Protocol     int             `json:"protocol"`
	Capabilities []string        `json:"capabilities"`
	MaxJobs      int             `json:"max_jobs"`
	BusyJobIDs   []string        `json:"busy_job_ids"`
	Adapters     []AdapterStatus `json:"adapters"`
	Tools        []ToolStatus    `json:"tools,omitempty"`
	Policy       Policy          `json:"policy"`
	// Additive in 0.2.0-rc9 (sandboxed verify, §19).
	VerifySandbox *VerifySandboxStatus `json:"verify_sandbox,omitempty"`
	// Additive in 0.2.0-rc10 (per-job limits, §20).
	JobLimits *JobLimitsStatus `json:"job_limits,omitempty"`
}

type HelloOK struct {
	Header
	Protocol     int      `json:"protocol"`
	HeartbeatSec int      `json:"heartbeat_sec"`
	ServerTime   string   `json:"server_time"`
	Capabilities []string `json:"capabilities"`
}

// Ping renews the leases of JobIDs held by this worker (§4.2, §5.3).
type Ping struct {
	Header
	JobIDs []string `json:"job_ids,omitempty"`
}

type Pong struct{ Header }

type Status struct {
	Header
	Adapters   []AdapterStatus `json:"adapters"`
	Tools      []ToolStatus    `json:"tools,omitempty"`
	Policy     Policy          `json:"policy"`
	MaxJobs    int             `json:"max_jobs"`
	BusyJobIDs []string        `json:"busy_job_ids"`
}

// Rotated carries a new pending credential. Never log it (§11).
type Rotated struct {
	Header
	Credential string `json:"credential"`
}

type RotateAck struct{ Header }

// --- Jobs (§5) ---

// Offer carries a job token. Never log it (§11).
type Offer struct {
	Header
	JobID      string `json:"job_id"`
	LeaseID    string `json:"lease_id"`
	LeaseUntil string `json:"lease_until"`
	Adapter    string `json:"adapter"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	SpecURL    string `json:"spec_url"`
	JobToken   string `json:"job_token"`
}

type Accept struct {
	Header
	JobID   string `json:"job_id"`
	LeaseID string `json:"lease_id"`
}

// Decline reasons (§5.2).
const (
	DeclineBusy               = "busy"
	DeclineAdapterUnavailable = "adapter_unavailable"
	DeclinePolicyDenied       = "policy_denied"
	DeclineSecretMissing      = "secret_missing"
	DeclineBadSpec            = "bad_spec"
	DeclineInternal           = "internal"
	// DeclineModelNotAllowed: the job's model is not in the host's policy
	// allowed_models. Additive in 0.2.0-rc4.
	DeclineModelNotAllowed = "model_not_allowed"
)

type Decline struct {
	Header
	JobID   string `json:"job_id"`
	LeaseID string `json:"lease_id"`
	Reason  string `json:"reason"`
}

// Lease grants or re-attaches a lease, with a fresh job token (§5.3).
type Lease struct {
	Header
	JobID      string `json:"job_id"`
	LeaseID    string `json:"lease_id"`
	LeaseUntil string `json:"lease_until"`
	JobToken   string `json:"job_token"`
}

// --- Streaming and control (§6) ---

type Progress struct {
	Header
	JobID   string       `json:"job_id"`
	LeaseID string       `json:"lease_id"`
	Seq     int64        `json:"seq"`
	Events  []exec.Event `json:"events"`
}

// Ack is cumulative: everything up to and including Seq.
type Ack struct {
	Header
	JobID string `json:"job_id"`
	Seq   int64  `json:"seq"`
}

// Control carries an exec.Control and, for a resume, a fresh job token.
type Control struct {
	Header
	JobID     string       `json:"job_id"`
	LeaseID   string       `json:"lease_id"`
	RequestID string       `json:"request_id"`
	Control   exec.Control `json:"control"`
	JobToken  string       `json:"job_token,omitempty"`
}

type ControlResult struct {
	Header
	JobID     string          `json:"job_id"`
	RequestID string          `json:"request_id"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	Code      string          `json:"code,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

type ControlAck struct {
	Header
	JobID     string `json:"job_id"`
	RequestID string `json:"request_id"`
}

// Results of the remote-only controls (§6.3).
type ShipPreviewResult struct {
	Repos []ShipPreviewRepo `json:"repos"`
}

type ShipPreviewRepo struct {
	Repo    string   `json:"repo"`
	Files   []string `json:"files"`
	Message string   `json:"message"`
}

type DiffResult struct {
	Repo      string `json:"repo"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

// ShipResult's PRs use job.prs[]'s shape (kind "pr" on a worker).
type ShipResult struct {
	PRs []PR `json:"prs"`
}

type PR struct {
	Kind     string `json:"kind"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	Status   string `json:"status"`
	URL      string `json:"url,omitempty"`
	GHNumber int    `json:"ghNumber,omitempty"`
	GHState  string `json:"ghState,omitempty"`
	Error    string `json:"error,omitempty"`
}

// --- Errors (§9) ---

type Error struct {
	Header
	Code     string `json:"code"`
	RefType  string `json:"ref_type,omitempty"`
	Detail   string `json:"detail,omitempty"`
	JobID    string `json:"job_id,omitempty"`
	LeaseID  string `json:"lease_id,omitempty"`
	Expected int64  `json:"expected,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}
