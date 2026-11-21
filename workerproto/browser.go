package workerproto

// Worker browser (docs/worker-browser.md in the control plane, decisions
// B1–B8): after-fix screenshots on a worker. Additive to v1.

// CapBrowserV1: the worker understands JobSpec.Project.browser, previewCmd,
// previewUrl and agentHosts. Whether its browser is ready is the "browser"
// tool's readiness, not this capability.
const CapBrowserV1 = "browser.v1"

// ToolBrowser is the tools[] entry for the host's headless browser (policy
// agent_browser). A worker sends it only when the policy sets agent_browser:
// a server before browser.v1 refuses the name (ValidateTools).
const ToolBrowser = "browser"

// Browser not-ready reasons, besides missing and unsupported_os.
const (
	// ReasonUnsafe: agent_browser is no longer a root-owned regular file
	// that only root can write.
	ReasonUnsafe = "unsafe"
	// ReasonScanIncomplete: the host socket scan hit a limit, so the masked
	// list may miss a socket. No browser job runs on the host.
	ReasonScanIncomplete = "scan_incomplete"
)

// MaxMaskedSockets bounds policy.masked_sockets on the wire.
const MaxMaskedSockets = 128
