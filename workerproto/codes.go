package workerproto

import (
	"time"

	"github.com/coder/websocket"
)

// Frame error codes (§9).
const (
	CodeBadFrame      = "bad_frame"
	CodeUnknownType   = "unknown_type"
	CodeFrameTooLarge = "frame_too_large"
	CodeLeaseLost     = "lease_lost"
	CodeSeqGap        = "seq_gap"
	CodeBlobMissing   = "blob_missing"
	CodeNotOwner      = "not_owner"
	CodeInternal      = "internal"
)

// Job reasons: the normative "<code>: " prefix on exec.Done.Line and
// exec.Status.Error, and the queued reason (§9).
const (
	ReasonWorkerUnavailable  = "worker_unavailable" // API 409
	ReasonNoWorker           = "no_worker"          // queued_reason
	ReasonWorkerLost         = "worker_lost"
	ReasonWorkerRestarted    = "worker_restarted"
	ReasonSecretMissing      = "secret_missing"
	ReasonPolicyDenied       = "policy_denied"
	ReasonAdapterUnavailable = "adapter_unavailable"
	ReasonBaseMissing        = "base_missing"
	ReasonLFSMissing         = "lfs_missing" // W3: an LFS repo on a host without git-lfs
	ReasonSpoolFull          = "spool_full"
	ReasonAgentAuth          = "agent_auth"
)

// WebSocket close codes (§1), typed for github.com/coder/websocket, the one
// WebSocket library both dd-worker and serve use (the maintainers approved it 2026-11-15).
const (
	CloseNormal                                   = websocket.StatusNormalClosure
	ClosePolicy                                   = websocket.StatusPolicyViolation
	CloseTooBig                                   = websocket.StatusMessageTooBig
	CloseReplaced            websocket.StatusCode = 4000
	CloseRevoked             websocket.StatusCode = 4001
	CloseProtocolUnsupported websocket.StatusCode = 4002
	CloseOrgMismatch         websocket.StatusCode = 4003
)

// Capabilities (§10). Every v1 worker advertises CapBlobsV1.
const CapBlobsV1 = "blobs.v1"

// Token prefixes (§1, §3, §5.4). Only SHA-256 hashes are stored.
const (
	PrefixJoinToken  = "dd_wjn_"
	PrefixCredential = "dd_wrk_"
	PrefixJobToken   = "dd_wjt_"
)

// Paths (§1, §7).
const (
	PathSocket = "/worker/v1/ws"
	PathPrefix = "/worker/v1/"
)

// Limits (§1, §4–§7, §11).
const (
	MaxFrameBytes        = 1 << 20
	MaxEventBytes        = 768 << 10
	MaxStringFieldBytes  = 256 << 10
	MaxDiffBytes         = 512 << 10
	MaxBatchEvents       = 64
	BatchInterval        = time.Second
	FirstFrameDeadline   = 10 * time.Second
	HeartbeatSec         = 20
	OfflineAfter         = 60 * time.Second
	OfferAnswerTimeout   = 30 * time.Second
	LateAcceptGrace      = 5 * time.Second
	LeaseDuration        = 120 * time.Second
	JobTokenAfterDone    = 10 * time.Minute
	ReadinessRecheck     = 5 * time.Minute
	CancelGrace          = 3 * time.Second
	CancelDeadline       = 5 * time.Second
	RotationInterval     = 30 * 24 * time.Hour
	PendingCredentialTTL = 7 * 24 * time.Hour
	MaxPendingCreds      = 2
	ReplacedBackoffMin   = 60 * time.Second
	ReplacedMaxPer10Min  = 3
	SpoolMaxBytes        = 200 << 20
	SpoolEndedJobTTL     = 24 * time.Hour
	MaxAdapters          = 8
	MaxTools             = 8
	MaxBlobsPerJob       = 200
	MaxBlobBytesPerJob   = 100 << 20
	NoteBlobAbove        = 64 << 10
	BadFramesBeforeClose = 3
	BadFrameWindow       = 60 * time.Second
	FramesPerMinute      = 1000
	UnauthSocketsPerIP   = 3
	EnrolPerIPPerHour    = 10
	UpgradesPerCredMin   = 30
	BlobPutsPerTokenHour = 600
)

// Tool names a hello or status may report (§4.1).
const (
	ToolGit = "git"
	ToolGh  = "gh"
)

// ReasonNoGitHelper: git has no credential route to github.com (no gh
// credential helper and no ssh-agent). The other tool reasons are the
// adapter ones: missing, too_old, signed_out (§4.1).
const ReasonNoGitHelper = "no_git_helper"

// Minimum tool versions; older reports too_old (§4.1). git 2.32 is the
// first to honour GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM, which the pinned
// worktree git needs (W3, docs/worker-w3.md §3.5).
const (
	MinGitVersion = "2.32.0"
	MinGhVersion  = "2.0.0"
)

// DefaultAdapterOrder applies when the app's agent setting is empty and the
// host policy sets no order (§5.1, decision at freeze).
var DefaultAdapterOrder = []string{"claude", "grok", "codex", "antigravity"}

// AllowedBlobTypes are the content types a blob PUT may carry; the server
// sniffs magic bytes and refuses a mismatch or SVG with 415 (§7).
var AllowedBlobTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif", "text/plain"}
