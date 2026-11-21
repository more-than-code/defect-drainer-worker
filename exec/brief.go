package exec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// CONTRACT_FILES are repo-owned rules the brief tells the agent to read first.
// Port of backend/src/batches.ts CONTRACT_FILES.
var contractFiles = []string{"AGENTS.md", "CLAUDE.md", "CONTRIBUTING.md"}

var (
	acceptanceHeadingRe = regexp.MustCompile(`(?i)^#{1,6}\s+acceptance\b`)
	nextHeadingRe       = regexp.MustCompile(`^#{1,6}\s`)
	bulletPrefixRe      = regexp.MustCompile(`^[-*+]\s+`)
	numberedPrefixRe    = regexp.MustCompile(`^\d+[.)]\s+`)
	checkboxPrefixRe    = regexp.MustCompile(`^\[[ xX]\]\s*`)
)

// BatchSQLitePath is the TS writeBatchManifest path: sqlite:batches/<id>.
func BatchSQLitePath(id string) string {
	return "sqlite:batches/" + id
}

func contractFilesIn(worktreeAbs string) []string {
	var out []string
	for _, f := range contractFiles {
		if _, err := os.Stat(filepath.Join(worktreeAbs, f)); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// inventoryEvidencePaths returns a cleaned slash-rel (from defectsRoot) and
// the absolute path only when rel is a well-formed path under
// {defectsRoot}/evidence. Values containing NUL, newline, CR, or backtick
// are rejected so they cannot break the markdown fence or forge an abs line.
func inventoryEvidencePaths(defectsRoot, rel string) (cleanRel, abs string) {
	if rel == "" || strings.ContainsAny(rel, "\x00\n\r`") {
		return "", ""
	}
	norm := strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	joined := filepath.Join(defectsRoot, filepath.FromSlash(norm))
	// Confine to {defectsRoot}/evidence, not the whole inventory root — a
	// crafted rel can otherwise name job.json / the sqlite file. Same Rel /
	// ".." shape as underRoot, but on cleaned paths so a missing evidence
	// file is still listed. Do not switch this site to underRoot: EvalSymlinks
	// the root on macOS /var/folders rejects every not-yet-created path.
	cleanRoot := filepath.Clean(filepath.Join(defectsRoot, "evidence"))
	cleanJoin := filepath.Clean(joined)
	r, err := filepath.Rel(cleanRoot, cleanJoin)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", ""
	}
	fromDef, err := filepath.Rel(filepath.Clean(defectsRoot), cleanJoin)
	if err != nil || fromDef == ".." || strings.HasPrefix(fromDef, ".."+string(filepath.Separator)) {
		return "", ""
	}
	return filepath.ToSlash(fromDef), cleanJoin
}

func evidenceBriefItems(defectsRoot string, rels []string) []string {
	var out []string
	for _, e := range rels {
		rel, abs := inventoryEvidencePaths(defectsRoot, e)
		if abs == "" {
			continue
		}
		out = append(out, "  - rel: `"+rel+"`", "  - abs: `"+abs+"`")
	}
	return out
}

// AcceptanceCriteria pulls the ## Acceptance checklist out of a defect body.
// Port of backend/src/batches.ts acceptanceCriteria — same heading, strip, and
// "next heading ends the block" rules.
func AcceptanceCriteria(body string) []string {
	if body == "" {
		return nil
	}
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if acceptanceHeadingRe.MatchString(strings.TrimSpace(l)) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []string
	for _, raw := range lines[start+1:] {
		line := strings.TrimSpace(raw)
		if nextHeadingRe.MatchString(line) {
			break
		}
		if line == "" {
			continue
		}
		item := bulletPrefixRe.ReplaceAllString(line, "")
		item = numberedPrefixRe.ReplaceAllString(item, "")
		item = checkboxPrefixRe.ReplaceAllString(item, "")
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// BatchFixBriefInput is the TS buildBatchFixBrief argument.
type BatchFixBriefInput struct {
	Batch       Batch
	Defects     []Defect
	DefectsRoot string
	GrokSandbox string
	Worktrees   []git.WorktreeBinding
	// AllowedDomains are the hosts the agent's sandbox may reach; nil
	// writes no network section (grok, local serve without a list).
	AllowedDomains []string
	// NoNetwork says the agent's sandbox has no network at all (a
	// non-grok adapter with no allowed hosts).
	NoNetwork bool
	// Browser, when set, adds the Screenshots section and narrows
	// NO-SCREENSHOT (worker-browser B3, B6). Nil writes rc2's brief.
	Browser *BrowserBrief
}

// BrowserBrief is the app's preview as the brief states it; both empty means
// the app has no preview.
type BrowserBrief struct {
	PreviewCmd string
	PreviewURL string
	// Stack, when set, adds the Stack section (job stack S9) and
	// the Screenshots section says dd-shot runs the preview through it.
	Stack *StackBrief
}

// StackBrief is the app's job stack as the brief states it: names,
// repos, ports and URLs only, never an env value.
type StackBrief struct {
	Services   []JobStackService
	Postgres   bool
	TimeoutSec int
	SignIn     *StackSignIn // the email shows; the password never does
}

// BuildBatchFixBrief ports backend/src/batches.ts buildBatchFixBrief.
func BuildBatchFixBrief(input BatchFixBriefInput) string {
	sandbox := "strict"
	if input.GrokSandbox == "workspace" {
		sandbox = "workspace"
	}
	var lines []string
	lines = append(lines,
		"# Batch agent fix — "+input.Batch.ID,
		"",
		"## Role",
		"You are the **worker** for this job. This brief and `PROCESS.md` (same",
		"directory) outrank any per-turn instruction that contradicts them.",
		"- Read `PROCESS.md` before your first product edit. It is the worker contract.",
		"- Read `SKILLS.md` for the skills and contract files of each worktree.",
		"- Do not delegate onward. Do not spawn another coding-agent CLI.",
		"",
		"## Facts",
		"- Defect Drainer is the harness; this brief is the ask.",
		"- Inventory SSOT is SQLite + evidence files under `"+input.DefectsRoot+"/evidence`; you cannot write it.",
		"- The worktrees listed below are the only product paths you may write.",
		"- cwd is this handoff directory, not a product repo — skill discovery by directory will not fire.",
		"",
		"## Decisions already made (do not relitigate)",
		"",
		"| Decision | Value |",
		"|---|---|",
		"| Spec approval | the operator started this batch — do not wait to be approved |",
		"| Defects in scope | exactly the ids listed below |",
		"| Sandbox profile | **"+sandbox+"** (App Settings) |",
		"| Branch | the worktree branch already checked out — do not create another |",
		"| Commits | none: leave changes uncommitted; DD commits them when the operator ships |",
		"| Verification | operator-authored commands, re-run by DD after you exit; you may not add or edit them |",
		"| Worker ledger | `NOTES.md` + `fix-notes/`, never `tasks/todo.md` |",
		"",
		"## Environment",
		"- Any toolchain this job needs is already provisioned; the spawn prompt carries the specifics.",
		"- Do not re-scaffold Flutter and do not clone a second SDK.",
		"- Package installs: if the sandbox blocks a cache write, stop and record it in `NOTES.md`.",
		"  Do not overlay-symlink `~/fvm` or any other toolchain root to work around it.",
	)
	switch {
	case len(input.AllowedDomains) > 0:
		lines = append(lines,
			"- Network: only "+strings.Join(input.AllowedDomains, ", ")+" is reachable from the sandbox.",
			"  Use `--ignore-scripts` (or the package manager's equivalent) where it allows it.",
		)
	case input.NoNetwork:
		lines = append(lines, "- Network: none. A step that needs it fails: record it as `BLOCKED:` (below).")
	}
	lines = append(lines,
		"",
		"## Stopping early",
		"If something outside your control blocks the work (a refused path, no network, a missing",
		"tool), write a full line in `NOTES.md`:",
		"",
		"    BLOCKED: <reason>",
		"",
		"Defect Drainer stops the loop after that iteration and shows the reason to the operator.",
		"Do not retry around the blocker. Delete the line once it is gone.",
		"",
		"## Deliverables",
		"- `NOTES.md` — plan, decisions, departures, anything you could not verify.",
		"- `fix-notes/<DEF-id>.md` for each defect you claim fixed, answering every acceptance line.",
		"- `fix-evidence/<DEF-id>/fix-01.png` for each defect you claim fixed.",
		"",
		"## Honesty constraints",
		"- Never report a test result you did not observe. DD re-runs verification itself;",
		"  a claimed pass that fails on re-run is worse than an admitted failure.",
		"- Never reply DONE without the files. Prose is ignored when the files are missing.",
		"- Do not invent acceptance criteria, and do not quietly drop one you could not meet — say so.",
		"",
		"## NOTES.md (required)",
		"Record the per-defect plan and its blast-radius tier, decisions and any departure from",
		"this brief, what you could not verify, and what the operator must do next.",
		"",
		"## Goal",
		input.Batch.Goal,
		"",
		"## App",
		"`"+input.Batch.AppID+"`",
		"",
		"## Agent sandbox (App Settings)",
	)
	if sandbox == "workspace" {
		lines = append(lines,
			"- Profile: **workspace** — host reads allowed (iOS Simulator / simctl).",
			"- Writes only under handoff cwd + temp. Put post-fix screenshots in `fix-evidence/<DEF-id>/fix-01.png`.",
		)
	} else {
		lines = append(lines,
			"- Profile: **strict** (restrict) — no reliable Simulator access.",
			"- Code-only in worktrees; operator attaches fix_evidence in console if needed.",
		)
	}
	lines = append(lines,
		"",
		"## HARD isolation (enforced by operator — do not violate)",
		"- Product code edits are **only** allowed inside the **worktree paths** listed below.",
		"- An app may have **multiple repos**; decide which worktree(s) need changes for each defect.",
		"- **Never** edit primary checkouts (the \"primary\" paths). Other agents may own those trees.",
		"- Branch name for this batch: use the worktree branch already checked out (do not switch primary).",
		"- Inventory SSOT is SQLite + evidence files under `"+input.DefectsRoot+"/evidence`.",
		"- **Read** report + historical fix evidence from inventory (paths listed per defect below).",
		"- **Write** new fix proof under handoff `fix-evidence/` (this job only); backend harvests onto the defect after the run.",
		"",
	)
	if len(input.Worktrees) > 0 {
		lines = append(lines, "## Worktrees (edit ONLY these product paths)", "")
		for _, w := range input.Worktrees {
			lines = append(lines,
				"### "+w.Repo,
				"- **worktree (EDIT HERE):** `"+w.WorktreeAbs+"`",
				"- **branch:** `"+w.Branch+"`",
				"- **primary (DO NOT EDIT):** `"+w.PrimaryAbs+"`",
			)
			contracts := contractFilesIn(w.WorktreeAbs)
			if len(contracts) > 0 {
				quoted := make([]string, 0, len(contracts))
				for _, f := range contracts {
					quoted = append(quoted, "`"+filepath.Join(w.WorktreeAbs, f)+"`")
				}
				lines = append(lines,
					"- **read first — this repo's own rules:** "+strings.Join(quoted, ", "),
					"  They define that repo's mandatory gates, skills and conventions.",
					"  They bind you as much as this brief; where stricter, they win.",
				)
			}
			lines = append(lines, "")
		}
	} else {
		lines = append(lines,
			"## Worktrees",
			"",
			"**NONE — refuse to edit product code.** Worktree setup failed or was skipped.",
			"",
		)
	}
	lines = append(lines,
		"## Rules",
		"- Fix **only** the listed defects.",
		"- Prefer minimal, verified fixes with tests inside the worktree.",
		"- Use **historical fix evidence** (if listed) as context: prior proof of what “fixed” looked like; do not ignore regressions.",
		"",
		"## New fix evidence this run (REQUIRED — handoff paths, sandbox-writable)",
		"Backend imports these after the run onto the **defect** SSOT. Do **not** rely on writing inventory directly.",
		"",
		"For **each** defect id you claim fixed:",
		"1. Screenshots (png/jpg/webp): `fix-evidence/<DEF-id>/fix-01.png`, `fix-02.png`, …",
		"2. Short resolution text: `fix-notes/<DEF-id>.md` (what changed + how verified).",
		"",
		"Without **new** images under this job's `fix-evidence/<DEF-id>/`, harvest will not update the defect for this run.",
		"",
		"## Defects ("+strconv.Itoa(len(input.Defects))+")",
		"",
	)
	for _, d := range input.Defects {
		lines = append(lines,
			"### "+d.ID,
			"- **title:** "+d.Title,
			"- **severity:** "+d.Severity,
			"- **status:** "+d.Status,
			"- **summary:** "+d.Summary,
		)
		if d.Surface != "" {
			lines = append(lines, "- **surface:** "+d.Surface)
		}
		if len(d.Repos) > 0 {
			lines = append(lines, "- **related repos:** "+strings.Join(d.Repos, ", "))
		}
		if d.Resolution != "" {
			lines = append(lines, "- **prior resolution note (historical):** "+d.Resolution)
		}
		acceptance := AcceptanceCriteria(d.Body)
		if len(acceptance) > 0 {
			lines = append(lines, "- **acceptance criteria (THE BAR — address each one in fix-notes):**")
			for _, a := range acceptance {
				lines = append(lines, "  - [ ] "+a)
			}
		}
		if ev := evidenceBriefItems(input.DefectsRoot, d.Evidence); len(ev) > 0 {
			lines = append(lines, "- **report evidence (intake — read for bug context):**")
			lines = append(lines, ev...)
		}
		if fx := evidenceBriefItems(input.DefectsRoot, d.FixEvidence); len(fx) > 0 {
			lines = append(lines, "- **historical fix evidence (owned by defect SSOT — read for prior proof / regressions):**")
			lines = append(lines, fx...)
		} else {
			lines = append(lines, "- **historical fix evidence:** (none yet — first fix run or never harvested)")
		}
		lines = append(lines,
			"- **deliver this run:** `fix-evidence/"+d.ID+"/fix-01.png` + `fix-notes/"+d.ID+".md`",
			"",
		)
	}
	noShot := []string{
		"   - If the sandbox is **strict** and you cannot screenshot, put a line reading exactly",
		"     `NO-SCREENSHOT: <DEF-id>` in `NOTES.md`. That releases the run only — the defect",
		"     still will not resolve without a new image.",
	}
	if input.Browser != nil {
		if input.Browser.Stack != nil {
			lines = append(lines, stackBriefSection(*input.Browser.Stack)...)
		}
		lines = append(lines, browserBriefSection(*input.Browser)...)
		noShot = []string{
			"   - Only if `dd-shot` failed, or this brief says the app has no preview, put a line",
			"     reading exactly `NO-SCREENSHOT: <DEF-id>` in `NOTES.md`, with the `DD-SHOT FAIL`",
			"     line quoted under it. That releases the run only — the defect still will not",
			"     resolve without a new image.",
		}
	}
	lines = append(lines,
		"## Done",
		"Completion is measured in **files**, not prose.",
		"",
		"1. Write `NOTES.md`.",
		"2. For each defect you claim fixed:",
		"   - `fix-notes/<DEF-id>.md` answering every acceptance criterion, and",
		"   - `fix-evidence/<DEF-id>/fix-01.png`.",
	)
	lines = append(lines, noShot...)
	lines = append(lines,
		"3. To decline a defect, put a line reading exactly `UNFIXED: <DEF-id>` in `NOTES.md`.",
		"   The job may complete; that defect will not resolve.",
		"4. Only once those files exist, reply `DONE "+input.Batch.ID+"`.",
		"",
		"Defect Drainer ignores the prose when the files are missing.",
		"",
	)
	return strings.Join(lines, "\n")
}

// WriteHandoffBrief writes BRIEF.md, the DD-owned PROCESS.md worker contract,
// the generated SKILLS.md path listing, and the sandbox-writable harvest dirs.
func WriteHandoffBrief(handoff string, input BatchFixBriefInput) error {
	if err := os.MkdirAll(filepath.Join(handoff, "fix-evidence"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(handoff, "PROCESS.md"), []byte(processMarkdown), 0o644); err != nil {
		return err
	}
	skills := BuildSkillsMarkdown(input.Worktrees)
	if err := os.WriteFile(filepath.Join(handoff, "SKILLS.md"), []byte(skills), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(handoff, "BRIEF.md"), []byte(BuildBatchFixBrief(input)), 0o644)
}

// browserBriefSection is the brief's Screenshots section for a worker
// browser job (worker-browser B3, B6).
func browserBriefSection(b BrowserBrief) []string {
	if b.PreviewCmd == "" || b.PreviewURL == "" {
		return []string{
			"## Screenshots",
			"This host has a headless browser, but the app has **no preview**: no preview command",
			"and URL are set, so `dd-shot` cannot serve it. Put `NO-SCREENSHOT: <DEF-id>` in",
			"`NOTES.md` for each defect you fix, and say in its fix notes how you verified it.",
			"",
		}
	}
	if b.Stack != nil {
		return stackScreenshotsSection(b)
	}
	return []string{
		"## Screenshots",
		"This host has a headless browser. Take each after-fix screenshot with `dd-shot`, from",
		"this directory, in **one** Bash call:",
		"",
		"```sh",
		"sh dd-shot <DEF-id> <path> [<path>...]",
		"```",
		"",
		"- It runs the app's preview (`" + briefSafe(b.PreviewCmd, 200) + "`) in the first worktree, waits up",
		"  to 90 s for " + briefSafe(b.PreviewURL, 100) + ", screenshots each path (for example `/` or",
		"  `/settings`) into `fix-evidence/<DEF-id>/fix-NN.png`, then stops the preview.",
		"- It prints one `DD-SHOT OK` or `DD-SHOT FAIL` line per step; the preview's output is in",
		"  `.dd-shot/server.log`.",
		"- A server you start yourself does not survive into your next Bash call, so don't serve",
		"  the app in one call and screenshot it in another.",
		"- The preview runs with no app secrets. If a page needs its backend, screenshot what",
		"  renders and say so in the fix notes.",
		"",
	}
}

// stackBriefSection is the brief's Stack section (job stack S9):
// what the stack holds and how the agent runs a command against it.
func stackBriefSection(st StackBrief) []string {
	lines := []string{
		"## Stack",
		"This app has a job stack. `dd-stack`, in this directory, boots it inside your sandbox:",
	}
	if st.Postgres {
		lines = append(lines, "- a throwaway Postgres, fresh on every call (no data survives it);")
	}
	for _, s := range st.Services {
		steps := []string{}
		if s.Install != "" {
			steps = append(steps, "install")
		}
		if s.Migrate != "" {
			steps = append(steps, "migrate")
		}
		if len(s.Seed) > 0 {
			steps = append(steps, fmt.Sprintf("%d seed(s)", len(s.Seed)))
		}
		steps = append(steps, "start")
		lines = append(lines, fmt.Sprintf("- `%s` (repo `%s`): %s, then serves %s, health `%s`;",
			s.Name, briefSafe(s.Repo, 100), strings.Join(steps, ", "), ServiceURL(s.Port), briefSafe(s.Health, 100)))
	}
	lines = append(lines,
		"",
		"To try the app against it, run your command through it in **one** Bash call, from this",
		"directory (the stack stops when the call ends):",
		"",
		"```sh",
		"sh dd-stack run -- curl -s '"+ServiceURL(st.Services[0].Port)+briefSafe(st.Services[0].Health, 100)+"'",
		"```",
		"",
		fmt.Sprintf("- The stack must be up within %d s. It prints `DD-STACK` status lines only; a", st.TimeoutSec),
		"  `DD-STACK FAIL <service> <step>` line names what failed, with its log's last lines.",
		"  Every step's log is in `.dd-stack/`.",
		"- Your command runs with the app's preview env. The env holds dev-only values; never",
		"  copy one into the code, notes or evidence.",
	)
	if st.SignIn != nil {
		lines = append(lines,
			"- Stack sign-in: the stack's test user is `"+briefSafe(st.SignIn.Email, 100)+"` (its password is in",
			"  `$DD_SIGNIN_PASSWORD`; never write it down). Inside `dd-stack run`, `$DD_SIGNIN_URL`,",
			"  `$DD_SIGNIN_EMAIL` and `$DD_SIGNIN_PASSWORD` let your command sign in as that user.",
		)
	}
	lines = append(lines,
		"- Never put setup files (env files, seed data, scripts) in a worktree to make the stack",
		"  run: the stack is defined in Defect Drainer, and the diff must hold only the fix.",
		"- The after-fix screenshot comes from `dd-shot`, which runs the preview through this",
		"  same stack.",
		"",
	)
	return lines
}

// stackScreenshotsSection is the Screenshots section when the preview runs
// through dd-stack.
func stackScreenshotsSection(b BrowserBrief) []string {
	return []string{
		"## Screenshots",
		"This host has a headless browser. Take each after-fix screenshot with `dd-shot`, from",
		"this directory, in **one** Bash call:",
		"",
		"```sh",
		"sh dd-shot <DEF-id> <path> [<path>...]",
		"```",
		"",
		"- It boots the stack with `dd-stack`, runs the app's preview (`" + briefSafe(b.PreviewCmd, 200) + "`)",
		"  in the first worktree, waits up to " + fmt.Sprint(b.Stack.TimeoutSec) + " s for " + briefSafe(b.PreviewURL, 100) + ", screenshots each path",
		"  (for example `/` or `/settings`) into `fix-evidence/<DEF-id>/fix-NN.png`, then stops it all.",
		signInLine(b.Stack.SignIn),
		"- It prints one `DD-SHOT OK` or `DD-SHOT FAIL` line per step; a failed stack shows as",
		"  `DD-SHOT FAIL <DEF-id> stack <service> <step>`. The preview's and the stack's output",
		"  is in `.dd-shot/server.log`.",
		"- A server you start yourself does not survive into your next Bash call, so don't serve",
		"  the app in one call and screenshot it in another.",
		"- Screenshot only pages that render the fix. An error page is not evidence.",
		"",
	}
}

// signInLine is the Screenshots section's line on the Stack sign-in.
func signInLine(si *StackSignIn) string {
	if si == nil {
		return "- It takes the screenshots signed out."
	}
	return "- It signs in as the stack's test user first (`DD-SHOT SIGNIN OK`). If that fails, it prints\n" +
		"  `DD-SHOT FAIL <DEF-id> signin <reason>` and takes no screenshot: fix the sign-in, not the page."
}
