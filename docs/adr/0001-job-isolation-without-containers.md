# ADR 0001: Isolate worker jobs with OS sandboxes, not containers

- **Status:** Accepted (records the existing design; written 2026-11-23)
- **Defect:** DEF-20261123-document-why-worker-jobs-are-sandboxed-w-o22o
- **Scope:** how `dd-worker` keeps one coding-agent job away from the host,
  the worker's own state and other jobs

## 1. Context

`dd-worker` runs headless coding agents (Claude Code by default) on a Linux
host the customer owns. The agent reads and edits a repo, runs its build and
tests, and sometimes drives a headless Chromium and a throwaway Postgres for
after-fix screenshots. A project's verify commands then run on the same host.
All of that is code the worker didn't write, so each job has to be fenced off
from:

- the host owner's secrets (`~/.ssh`, cloud credentials, the API key);
- the worker's credential, policy and state;
- other jobs on the same host, and the shipped branch.

The target hosts are small Linux machines. Joe runs workers on Raspberry Pis,
not on the Mac (recorded in the control plane's `docs/claude-code-runner.md`,
Antigravity decision). The install guide lists "a Raspberry Pi 4/5 with
64-bit OS" as a supported host (`docs/host-setup.md`).

Nothing in either repo said why this is done without Docker or another
container runtime. The only recorded decision is D3 in the control plane's
`docs/worker-policy-via-deploybot.md`: the worker "stays a hardened systemd
service (`dd-worker`), not a Deploybot program or container: no nested
bubblewrap, Chromium or toolchains." This ADR writes down the full reasoning
and compares the options.

## 2. What the worker does today

No containers. Six layers, each in code:

| Layer | What it does | Where |
|---|---|---|
| Dedicated user and hardened unit | Runs as the `dd-worker` system user with an empty home. `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome=read-only`, `PrivateTmp`, an empty capability bounding set, `RestrictSUIDSGID`. The API key arrives through `LoadCredential`, never the environment. | `packaging/dd-worker.service` |
| Per-job directories | Each job gets its own handoff and git worktrees. The repo clone is readable but not writable by the agent; the worker commits only on ship. | `exec/`, `git/pinned.go` |
| The agent CLI's own sandbox, `strict` | A per-job `--settings` file turns Claude Code's sandbox on with `failIfUnavailable: true` and `allowUnsandboxedCommands: false`. On Linux that sandbox is bubblewrap; on macOS it is Seatbelt. Reads and edits are scoped to the job's dirs, web fetch and search are denied. | `agentadapter/claude/claude.go` (`Settings`) |
| Hidden secrets | Every path in `Host.DenyRead()` is denied to the shell and the file tools: `~/.ssh`, `~/.config`, `~/.aws`, `~/.gnupg`, `~/.kube`, `~/.docker`, `~/.netrc`, the agent CLIs' config dirs, DD's database, clones, agent state and other jobs. | `exec/host.go` |
| Egress allowlist | The agent's shell has no network unless policy `agent_allowed_domains` lists exact hosts (narrowed further by the project). | `agentadapter/claude/claude.go` (`withNetwork`), `internal/worker/offer.go` |
| Verify sandbox | Verify commands run in a separate bwrap sandbox built by the worker: `--unshare-all --unshare-net --cap-drop ALL --die-with-parent`, host read-only, secrets and state behind empty tmpfs, only the worktrees writable, `.git` read-only, network only through the worker's egress proxy. A start-up self-test gates the `verify_sandbox.v1` capability. | `internal/sandbox`, `internal/egress`, commit `e5c734c` |

These are the same kernel features containers use (namespaces, read-only and
tmpfs mounts, capability dropping), applied per process instead of per image.

## 3. Why not containers: the reasons

Each reason below is checked against the code or a recorded decision. Where it
is an inference, it says so.

1. **The agent needs the host's tools and sign-ins.** The job runs the host's
   `claude`, `git`, Chromium, Postgres binaries and whatever toolchains the
   repo needs. Policy points at them by absolute, root-owned path
   (`agent_browser`, `agent_postgres`; `internal/worker/browser.go`,
   `postgres.go`). In a container, every one of these has to be baked into an
   image and kept at the same version as the host's, or the job sees
   different tools from the ones the operator installed and checked.
2. **Nested sandboxing.** Claude Code's own sandbox is bubblewrap, which needs
   unprivileged user namespaces. Docker's default seccomp and AppArmor
   profiles block those inside a container, so either the agent's sandbox is
   turned off (losing the `strict` guarantee and `failIfUnavailable`) or the
   container is started with a weakened profile. That is D3's "no nested
   bubblewrap". The same applies to the verify sandbox.
3. **Docker's daemon is root-equivalent.** Anyone who can talk to
   `/var/run/docker.sock` can mount the host's root. Giving the `dd-worker`
   user that access would undo the unit's `NoNewPrivileges` and empty
   capability set; DD already hides the Docker socket from agents for this
   reason (`~/.docker` is in `DenyRead`, and the control plane's
   `docs/verification-hardening.md` handles the `docker.sock` symlink).
4. **Small hosts.** A Pi 4 or 5 has 4 to 8 GB of RAM and usually an SD card or
   small SSD. A resident daemon and gigabytes of images compete with the
   agent's Node process and Chromium. Section 4 has the numbers.
5. **Fewer moving parts to install.** The host needs `git`, `bubblewrap`,
   `socat` and `curl` from the distro (`install.sh` checks for them and
   installs nothing itself). There is no registry, image build, or image
   update cycle for the operator to run.
6. **macOS (inference, now secondary).** Claude Code's sandbox works on macOS
   (Seatbelt), so a Mac worker can run agent jobs without a VM. Docker on a
   Mac always means a Linux VM. Since the worker targets Linux hosts this
   matters less today, but it keeps the Mac usable for local DD.

## 4. Resource cost: Docker and the alternatives

Joe's question: does Docker cost more RAM, CPU, disk and time? Short answer:
**on Linux, the per-job runtime overhead of a container is small; the real
costs are a daemon that is always resident, gigabytes of image storage, slower
job starts, and image upkeep. On a Mac, Docker Desktop's VM is the big cost.**

Figures marked *published* come from the projects' own documentation (links
at the end). Figures marked *estimate* are typical values, not measured on a
DD worker; section 7 lists how to measure them on a Pi.

### 4.1 Per host (paid once, all the time)

| | Current (bwrap) | Docker Engine (Linux) | Rootless Podman | gVisor (runsc) | Firecracker microVMs | Docker Desktop (macOS) |
|---|---|---|---|---|---|---|
| Resident daemon | none (bwrap runs only during a job) | `dockerd` + `containerd`, *estimate* tens of MB to ~150 MB RSS, idle CPU near zero | none (daemonless; one small `conmon` per container) | none beyond Docker/containerd if used through them | none per host; a jailer per VM | a Linux VM: memory limit *published* "defaults to 50% of your host's memory", swap 1 GB |
| Packages installed | `bubblewrap`, `socat`: well under 1 MB | engine, containerd, CLI, plugins: *estimate* a few hundred MB | podman, crun, conmon, netavark/pasta: *estimate* ~100 MB | runsc binary: *estimate* tens of MB, plus a container runtime | firecracker + jailer, plus a guest kernel and rootfs per image | the Desktop app plus a VM disk image |
| Images on disk | none: jobs use the host's tools | one or more job images with Node, Claude Code, git, Chromium and toolchains: *estimate* 1.5 to 3 GB each, multiplied by toolchain variants | same as Docker, stored per user | same as Docker | a root filesystem image per variant, same order of size | same as Docker, inside the VM disk |
| Kernel requirements | unprivileged user namespaces | cgroups, overlayfs | user namespaces, `subuid`/`subgid` ranges, cgroup v2 delegation | Linux only (*published*), x86_64 or ARM64 | KVM (`/dev/kvm`), Linux host only, same-arch guests (*published*); Pi support unverified | Apple Virtualization framework |

### 4.2 Per job

| | Current (bwrap) | Docker / Podman | gVisor | Firecracker |
|---|---|---|---|---|
| Start-up | *estimate* a few ms per bwrap launch | *estimate* 0.3 to 1 s per `run` on x86 (more on a Pi); minutes for a cold image pull | *estimate* a few hundred ms | *published* ≤ 125 ms from InstanceStart to guest `/sbin/init` (minimal kernel and rootfs); the job's own boot on top |
| Memory overhead | *estimate* a few MB of bwrap and socat processes | *estimate* a few MB of shim per container; the job's own processes dominate | Sentry and Gofer per sandbox, *estimate* tens of MB | *published* VMM ≤ 5 MiB, **but the guest's RAM is reserved up front**: sized for Node, Chromium and a build, *estimate* 2 to 4 GB per job |
| CPU in steady state | native (same kernel) | near native; overlayfs and bridge networking add a little on file- and network-heavy steps | slower on syscall-heavy work (`git`, `npm install`, `go build`): every syscall goes through the user-space kernel. *published*: directfs cut real workloads' time 12%, Systrap replaced ptrace for speed, and ARM lacks one of Systrap's x86 optimisations | *published* compute > 95% of bare metal; I/O through virtio |
| Disk per job | worktrees only (clone shared per repo) | worktrees (bind-mounted) plus a writable layer | same as Docker | a writable disk image per VM, or a shared clone over virtio-fs |

What this means on a 4 GB Pi: a Claude Code job (one Node process, a
reviewer later) plus Chromium already uses a large share of RAM. Docker's
daemon and shims fit, but images eat the SD card, and every image update is a
multi-GB pull. A microVM per job reserves its guest RAM whether used or not,
so one job at a time is the realistic ceiling.

On a Mac, Docker Desktop's VM takes half the RAM by default (configurable) and
file sharing goes through VirtioFS, which is much faster than it was but still
slower than native disk for `node_modules`-sized trees.

## 5. Comparison

| Criterion | Current (OS sandbox) | Docker (rootful) | Rootless Podman | gVisor | Firecracker / Kata microVM |
|---|---|---|---|---|---|
| Isolation boundary | Namespaces, read-only and tmpfs mounts, dropped capabilities; shares the host kernel | Same class: namespaces, cgroups, default seccomp and AppArmor; shares the host kernel | Same class, and the runtime itself is unprivileged | User-space kernel: most host-kernel bugs unreachable | Hardware virtualisation: separate guest kernel. Strongest |
| Kernel attack surface | full syscall set (verify sandbox has no seccomp filter) | reduced by default seccomp | reduced by default seccomp | small | small (KVM and virtio devices) |
| Per-job CPU and memory caps | none per job: limits apply to the whole unit, and the unit sets none | cgroup limits per container | cgroup limits, if v2 delegation is set up | cgroup limits | fixed vCPUs and RAM per VM |
| Host tools and sign-ins | used directly | baked into images | baked into images | baked into images | baked into guest images |
| Agent's own sandbox (bwrap) inside | yes, it is the sandbox | needs weakened seccomp/AppArmor | works with care (nested user namespaces) | unverified | yes (own kernel), at the cost of a full guest |
| Privilege needed by the worker | none (unprivileged user namespaces) | root-equivalent daemon socket | none, but `subuid` and cgroup set-up | a runtime install by root | `/dev/kvm` access |
| Runs on macOS | yes (Seatbelt; no verify sandbox) | only through a VM | only through a VM (`podman machine`) | no | no |
| Runs on a Pi 4/5 | yes (shipped) | yes | yes | likely (ARM64 supported); not tested | unverified (needs KVM support from the Pi's kernel and interrupt controller) |
| Operator upkeep | distro packages | engine, images, registry, pruning | images, pruning, `subuid` | runtime plus images | kernels, rootfs images, networking (tap devices) |

## 6. Decision

**Keep the OS-sandbox design.** It gives container-class isolation (the same
kernel features) without a privileged daemon, without images, with the host's
own tools, and at near-zero resource cost on small hosts. Containers would
add cost and moving parts without a stronger boundary: Docker and Podman still
share the host kernel, exactly as bwrap does.

Two gaps are real, and both can be closed without containers:

1. **No per-job memory or CPU cap.** One runaway build can starve the host
   and the other jobs. The fix is cgroup v2: `Delegate=yes` on the unit plus
   a per-job sub-cgroup (or `systemd-run --user --scope`) with `MemoryMax`,
   `CPUQuota` and `TasksMax`. A unit-wide `MemoryMax` is a smaller first step.
2. **No seccomp filter in the verify sandbox.** bwrap takes one through
   `--seccomp <fd>`; a default-deny-dangerous list like Docker's would bring
   its kernel surface in line with a container's.

**When to revisit:** if DD ever runs jobs from mutually untrusted orgs on the
same host (DD-hosted shared workers), the shared kernel becomes the risk that
matters, and a microVM per job (Firecracker or Kata) is the right step up,
on x86 or server ARM hosts with KVM, not on Pis. Containers alone would not
fix that.

**Status, 2026-11-25:** both gaps are closed in worker PR #16 (0.2.0-rc10):
per-job cgroup v2 limits under a delegated unit cgroup, unit-wide
`MemoryMax`/`TasksMax`, a deny-list seccomp filter in the verify sandbox and a
looser `SystemCallFilter` on the whole unit (the agent included). See the
host setup guide, Resource limits and Verify sandbox.

## 7. Follow-ups

- File the two gaps above as defects (per-job cgroup limits; seccomp in the
  verify sandbox).
- Measure the estimates on a Pi 4 and a Pi 5: bwrap start time and RSS
  against `docker run --rm` and `podman run --rm` with a Debian slim image,
  `dockerd`/`containerd` idle RSS, and a job image's size with Node, Claude
  Code, git and Chromium. Replace the *estimate* marks with those numbers.

## Sources

- Firecracker specification (boot time, VMM memory, CPU and I/O figures):
  <https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md>
- Firecracker FAQ (supported processors, Linux hosts, same-arch guests):
  <https://github.com/firecracker-microvm/firecracker/blob/main/FAQ.md>
- Docker Desktop settings (memory default, swap, VirtioFS):
  <https://docs.docker.com/desktop/settings-and-maintenance/settings/>
- gVisor Systrap release: <https://gvisor.dev/blog/2023/04/28/systrap-release/>
- gVisor directfs: <https://opensource.googleblog.com/2023/06/optimizing-gvisor-filesystems-with-directfs.html>
- gVisor FAQ (Linux only, x86_64 and ARM64): <https://gvisor.dev/docs/user_guide/faq>
