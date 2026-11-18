package exec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/git"
)

// A test-run job: the agent executes a test run's steps and records results,
// screenshots and RUN.md in the handoff. The executor writes the brief,
// decides when the loop may stop, and reads the results back; the
// orchestrator applies them to the run.

// stepKeyRe is store.ValidStepKey's rule. TestValidStepKeyMatchesStore in
// internal/jobs keeps the two equal.
var stepKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidStepKey reports whether k is a plan or run step key.
func ValidStepKey(k string) bool {
	return stepKeyRe.MatchString(k) && !strings.Contains(k, "..")
}

// TestBriefInput is what a test job's BRIEF.md is built from.
type TestBriefInput struct {
	TestRunID string
	AppID     string
	Worktrees []git.WorktreeBinding
	Run       TestRun
	// SecretEnv is set when the run's account password is injected.
	SecretEnv string
	// Failure is set when spawn did not start.
	Failure string
}

// WriteTestHandoff writes a test job's handoff: results/, evidence/,
// PROCESS.md, SKILLS.md and BRIEF.md.
func WriteTestHandoff(handoff string, in TestBriefInput) error {
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(handoff, "evidence"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(handoff, "PROCESS.md"), []byte(testProcessMarkdown), 0o644); err != nil {
		return err
	}
	skills := BuildSkillsMarkdown(in.Worktrees)
	if err := os.WriteFile(filepath.Join(handoff, "SKILLS.md"), []byte(skills), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(handoff, "BRIEF.md"), []byte(BuildTestBrief(in)), 0o644)
}

// testArtifactsComplete is the test job's loop stop: RUN.md, and a result
// file or a NOT-RUN line for every snapshot step. A run with no snapshot
// steps is done once RUN.md exists.
func testArtifactsComplete(handoff string, steps []RunStep, cutoff time.Time, secret string) (bool, []string) {
	var missing []string
	if _, _, ok := ReadRegularFile(handoff, filepath.Join(handoff, "RUN.md"), cutoff); !ok {
		missing = append(missing, "RUN.md")
	}
	notRun := NotRunKeys(handoff, cutoff, secret)
	snapshot := 0
	for _, step := range steps {
		if step.Exploratory {
			continue
		}
		snapshot++
		if _, noted := notRun[step.Key]; ValidResultFile(handoff, step.Key, cutoff) || noted {
			continue
		}
		missing = append(missing, "results/"+step.Key+".md")
	}
	if snapshot == 0 && len(missing) == 0 {
		return true, nil
	}
	return len(missing) == 0, missing
}

// CollectTestResults reads RUN.md, results/ and evidence/ for the run's
// steps. Everything the agent wrote is redacted with secret. imgCutoff is
// the stricter cutoff for screenshots (this run's start).
func CollectTestResults(handoff string, steps []RunStep, cutoff, imgCutoff time.Time, secret string) TestHarvest {
	ignored := 0
	var warns []string
	notRun := NotRunKeys(handoff, cutoff, secret)
	byKey := map[string]RunStep{}
	for _, step := range steps {
		byKey[step.Key] = step
	}
	updates := map[string]TestStepResult{}
	for key, reason := range notRun {
		step, ok := byKey[key]
		if !ok {
			ignored++
			warns = append(warns, "harvest: ignore NOT-RUN "+key+" (unknown step)")
			continue
		}
		actual := "Not run"
		if reason != "" {
			actual = "Not run: " + reason
		}
		updates[key] = TestStepResult{
			Key: key, Status: "blocked", Actual: RedactCut(actual, secret),
			Exploratory: step.Exploratory, Action: step.Action, Expected: step.Expected, HasExpected: true,
		}
	}

	resultsDir := filepath.Join(handoff, "results")
	if IsRealDir(resultsDir) {
		ents, err := os.ReadDir(resultsDir)
		if err == nil {
			names := make([]string, 0, len(ents))
			for _, e := range ents {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				if !strings.HasSuffix(name, ".md") {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" (unknown step)")
					continue
				}
				key := strings.TrimSuffix(name, ".md")
				path := filepath.Join(resultsDir, name)
				if !ValidStepKey(key) {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" (unknown step)")
					continue
				}
				data, tooLarge, ok := ReadRegularFile(handoff, path, cutoff)
				if tooLarge {
					ignored++
					warns = append(warns, fmt.Sprintf("harvest: ignore results/%s (exceeds %d bytes)", name, HarvestFileLimit))
					continue
				}
				if !ok {
					continue
				}
				step, exists := byKey[key]
				exploratory := strings.HasPrefix(key, "x-") && !exists
				if exists {
					exploratory = step.Exploratory
				}
				if !exists && !strings.HasPrefix(key, "x-") {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" (unknown step)")
					continue
				}
				parsed, perr := ParseHarvestResult(RedactRaw(string(data), secret), exploratory)
				if perr != "" {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" ("+perr+")")
					continue
				}
				parsed.Key = key
				parsed.Action = RedactCut(parsed.Action, secret)
				parsed.Expected = RedactCut(parsed.Expected, secret)
				parsed.Actual = RedactCut(parsed.Actual, secret)
				if exists && !parsed.Exploratory {
					parsed.Exploratory = false
				}
				updates[key] = parsed
			}
		}
	}

	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := TestHarvest{Steps: make([]TestStepResult, 0, len(keys))}
	for _, key := range keys {
		step := updates[key]
		imgs, extra, imgWarns := HarvestStepImages(handoff, key, imgCutoff)
		ignored += extra
		warns = append(warns, imgWarns...)
		step.Images = imgs
		out.Steps = append(out.Steps, step)
	}
	out.Warns = warns
	out.Ignored = ignored
	return out
}

// HygieneChanged reports whether the diff hygiene saw any change.

// WorktreeDirty reports whether any worktree has uncommitted changes.

const testContinuePrompt = `Continue executing BRIEF.md and PROCESS.md. Resume where you left off.
Do not stop early. Required artifacts (exact; Defect Drainer matches these, not prose):
- RUN.md
- results/<key>.md
- results/x-<slug>.md
- evidence/<key>/<file>
A full line in RUN.md, NOT-RUN: <key>, accounts for a snapshot step with no result file.
Do not invent other marker spellings. Do not delegate onward.
Never type, echo, log or screenshot the password.`

const testProcessMarkdown = `# Test worker contract

You execute the test run in BRIEF.md. You are not fixing defects.

## Build and launch

Use the worktree checkouts BRIEF.md lists. When the prompt names a toolchain, use that to build and launch the product from the worktree. Do not install tools outside this handoff.

## Record

For each step you check, write ` + "`results/<key>.md`" + ` and put one fresh screenshot in ` + "`evidence/<key>/`" + `. The screenshot has to be created during this run: save it under a new file name (for example with a timestamp), never over an existing file. An overwritten file keeps its old creation time and does not count.

The result file's first line is exactly ` + "`STATUS: pass`" + `, ` + "`STATUS: fail`" + `, ` + "`STATUS: blocked`" + ` or ` + "`STATUS: skipped`" + `. The rest is what happened.

To add a step that was not on the plan, write ` + "`results/x-<slug>.md`" + `. ` + "`x-<slug>`" + ` must be a valid step key. Line 1 is the STATUS line, line 2 is ` + "`ACTION: …`" + ` (required), line 3 may be ` + "`EXPECTED: …`" + `, and the rest is the result.

Write ` + "`RUN.md`" + ` before you stop. It is a short summary. For a step you did not run, add a line ` + "`NOT-RUN: <key>`" + ` and, when you know why, a reason after the key.

## Stop

Stop when ` + "`RUN.md`" + ` exists and every step in BRIEF.md has a result file or a ` + "`NOT-RUN`" + ` line. A run with no steps is done once ` + "`RUN.md`" + ` exists.

Do not edit product code to make a step pass. Do not commit, push, or open a pull request. Do not call the Defect Drainer API or start jobs. Never type, echo, log or screenshot the password.
`

const (
	testEvidenceMax   = 25 << 20
	testEvidenceLimit = 12
	testActualMax     = 4000
)

var (
	statusLineRe = regexp.MustCompile(`^STATUS: (pass|fail|blocked|skipped)$`)
	notRunRe     = regexp.MustCompile(`^NOT-RUN: ([a-z0-9][a-z0-9._-]{0,63})(?:[ \t]+(.*))?$`)
)

func HygieneChanged(h DiffHygieneReport) bool {
	for _, r := range h.Repos {
		if r.FilesChanged > 0 || r.ChangedLines > 0 {
			return true
		}
	}
	return false
}

func WorktreeDirty(wts []git.WorktreeBinding) bool {
	for _, w := range wts {
		out, err := WorktreeGit(w, "status", "--porcelain")
		if err == nil && strings.TrimSpace(out) != "" {
			return true
		}
	}
	return false
}

func BuildTestBrief(in TestBriefInput) string {
	run, failure := in.Run, in.Failure
	planID := "none"
	planVersion := "none"
	if run.PlanID != nil && *run.PlanID != "" {
		planID = *run.PlanID
	}
	if run.PlanVersion != nil {
		planVersion = fmt.Sprintf("%d", *run.PlanVersion)
	}
	lines := []string{
		"# Test run " + in.TestRunID,
		"",
		"## Role",
		"",
		"You are a test executor for Defect Drainer run " + in.TestRunID + ", not a fixer.",
		"Read this brief, then `PROCESS.md`, then `SKILLS.md`.",
		"",
		"## Run",
		"",
		"- Run: " + in.TestRunID,
		"- Title: " + run.Title,
		"- App: " + in.AppID,
		"- Plan: " + planID,
		"- Plan version: " + planVersion,
		"",
		"## Environment",
		"",
	}
	lines = append(lines, envBriefLines(run.Env)...)
	lines = append(lines, "", "## Account", "")
	lines = append(lines, accountBriefLines(run.Env, in.SecretEnv != "")...)
	lines = append(lines, "", "## Worktrees", "")
	if len(in.Worktrees) == 0 {
		lines = append(lines, "- (none — do not edit product code)")
	} else {
		refByRepo := map[string]RunRef{}
		for _, r := range run.Refs {
			refByRepo[r.Repo] = r
		}
		for _, w := range in.Worktrees {
			ref := refByRepo[w.Repo].Ref
			sha := refByRepo[w.Repo].SHA
			if ref == "" {
				ref = w.BaseBranch
			}
			if sha == "" {
				sha = WorktreeHead(w)
			}
			lines = append(lines, fmt.Sprintf("- %s: `%s` ref `%s` sha `%s`", w.Repo, w.WorktreeAbs, ref, sha))
		}
	}
	lines = append(lines, "", "## Steps", "")
	if len(run.Steps) == 0 {
		lines = append(lines, "- (none — write `RUN.md` and stop)")
	} else {
		for _, s := range run.Steps {
			lines = append(lines, fmt.Sprintf("- %d · %s · %s · %s · %s", s.Position, s.Key, s.Section, s.Action, s.Expected))
		}
	}
	lines = append(lines,
		"",
		"## Output contract",
		"",
		"Write these under this handoff:",
		"",
		"- `RUN.md` — a summary. It may contain lines `NOT-RUN: <key>` optionally followed by a reason.",
		"- `results/<key>.md` — first line exactly `STATUS: pass|fail|blocked|skipped`; the rest is the actual result.",
		"- `results/x-<slug>.md` — an exploratory step (`x-<slug>` must be a valid step key): line 1 `STATUS: …`, line 2 `ACTION: …` (required), optional line 3 `EXPECTED: …`, then the body.",
		"- `evidence/<key>/<file>` — screenshots, one of png, jpg, jpeg, webp, gif, heic.",
		"",
		"## Rules",
		"",
		"- Do not call the Defect Drainer API or start jobs.",
		"- Do not commit, push or open PRs.",
		"- Do not edit product code to make a step pass. Changes are discarded and flagged.",
		"- Never type, echo, log or screenshot the password.",
		"- Take one fresh screenshot per checked step, saved under a new file name (never overwrite an existing file).",
		"",
	)
	if failure != "" {
		lines = append(lines, "## Spawn", "", "Spawn did not start: "+failure, "")
	}
	return strings.Join(lines, "\n")
}

func envBriefLines(env map[string]any) []string {
	if len(env) == 0 {
		return []string{"- (none)"}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "account" || k == "secret_ref" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return []string{"- (none)"}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, "- "+k+": "+envBriefValue(env[k]))
	}
	return out
}

func envBriefValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return strings.TrimSpace(fmt.Sprint(StripSecretRef(t)))
	default:
		return strings.TrimSpace(fmt.Sprint(StripSecretRef(v)))
	}
}

func StripSecretRef(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range t {
			if k == "secret_ref" {
				continue
			}
			out[k] = StripSecretRef(child)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = StripSecretRef(child)
		}
		return out
	default:
		return v
	}
}

func accountBriefLines(env map[string]any, inject bool) []string {
	acc, _ := env["account"].(map[string]any)
	if acc == nil {
		return []string{"- no password is available to you; mark steps that need one blocked"}
	}
	label, _ := acc["label"].(string)
	username, _ := acc["username"].(string)
	lines := []string{"- Label: " + label, "- Username: " + username}
	if inject {
		lines = append(lines, "- the password is in $DD_TEST_ACCOUNT_PASSWORD")
	} else {
		lines = append(lines, "- no password is available to you; mark steps that need one blocked")
	}
	return lines
}

func BuildTestPrompt(label, handoff, defectsRoot, sandbox string, wts []git.WorktreeBinding, base *git.CodingAgentOpts) string {
	var wt []string
	if len(wts) == 0 {
		wt = []string{"- (none — do not edit product code)"}
	} else {
		for _, w := range wts {
			wt = append(wt, "- "+w.Repo+": run the product from "+w.WorktreeAbs+" (detached; do not edit it to force a pass)")
		}
	}
	var adapter agentadapter.Adapter
	if base != nil {
		adapter = base.Adapter
	}
	sandboxNote := git.SandboxHeadline(adapter, sandbox)
	parts := []string{
		"Test run session for " + label + ".",
		"Handoff directory (cwd): " + handoff,
		"Inventory root (readable, do not call its API): " + defectsRoot,
		"",
		sandboxNote,
		"You are a test executor, not a fixer. Do not delegate onward.",
		"Read BRIEF.md, PROCESS.md and SKILLS.md before the first step.",
		"The password, when one is provided, is only in $DD_TEST_ACCOUNT_PASSWORD. Never print it.",
		"",
		"WORKTREES:",
	}
	parts = append(parts, wt...)
	if base != nil && len(base.ToolchainNotes) > 0 {
		parts = append(parts, "")
		parts = append(parts, base.ToolchainNotes...)
	}
	parts = append(parts, "",
		"Write RUN.md, results/<key>.md, results/x-<slug>.md and evidence/<key>/<file> as BRIEF.md describes.",
		"Do not commit, push or open PRs. Do not edit product code to make a step pass.",
	)
	return strings.Join(parts, "\n")
}

func NotRunKeys(handoff string, cutoff time.Time, secret string) map[string]string {
	out := map[string]string{}
	data, _, ok := ReadRegularFile(handoff, filepath.Join(handoff, "RUN.md"), cutoff)
	if !ok {
		return out
	}
	raw := RedactRaw(string(data), secret)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		m := notRunRe.FindStringSubmatch(line)
		if m == nil || strings.Contains(m[1], "..") || !ValidStepKey(m[1]) {
			continue
		}
		reason := ""
		if len(m) > 2 {
			reason = strings.TrimSpace(m[2])
		}
		out[m[1]] = reason
	}
	return out
}

func ValidResultFile(handoff, key string, cutoff time.Time) bool {
	data, _, ok := ReadRegularFile(handoff, filepath.Join(handoff, "results", key+".md"), cutoff)
	if !ok {
		return false
	}
	_, _, good := ParseResultStatus(string(data))
	return good
}

func ParseResultStatus(raw string) (status, body string, ok bool) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return "", "", false
	}
	lines[0] = strings.TrimRight(lines[0], "\r")
	m := statusLineRe.FindStringSubmatch(lines[0])
	if m == nil {
		return "", "", false
	}
	rest := ""
	if len(lines) > 1 {
		rest = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	return m[1], rest, true
}

func ParseHarvestResult(raw string, exploratory bool) (TestStepResult, string) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return TestStepResult{}, "invalid STATUS line"
	}
	lines[0] = strings.TrimRight(lines[0], "\r")
	m := statusLineRe.FindStringSubmatch(lines[0])
	if m == nil {
		return TestStepResult{}, "invalid STATUS line"
	}
	step := TestStepResult{Status: m[1]}
	rest := lines[1:]
	if exploratory {
		step.Exploratory = true
		if len(rest) == 0 || !strings.HasPrefix(strings.TrimRight(rest[0], "\r"), "ACTION:") {
			return TestStepResult{}, "invalid STATUS line"
		}
		action := strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(rest[0], "\r"), "ACTION:"))
		if action == "" {
			return TestStepResult{}, "invalid STATUS line"
		}
		step.Action = action
		rest = rest[1:]
		if len(rest) > 0 && strings.HasPrefix(strings.TrimRight(rest[0], "\r"), "EXPECTED:") {
			step.HasExpected = true
			step.Expected = strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(rest[0], "\r"), "EXPECTED:"))
			rest = rest[1:]
		}
	}
	if len(rest) > 0 {
		step.Actual = strings.TrimSpace(strings.Join(rest, "\n"))
	}
	return step, ""
}

func HarvestStepImages(handoff, key string, cutoff time.Time) ([]HarvestFile, int, []string) {
	dir := filepath.Join(handoff, "evidence", key)
	if !IsRealDir(dir) {
		return nil, 0, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, nil
	}
	var names []string
	for _, e := range ents {
		if HarvestableImageName(e.Name(), e.IsDir()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var fresh []HarvestFile
	ignored := 0
	var warns []string
	for _, name := range names {
		data, tooLarge, ok := ReadRegularFile(handoff, filepath.Join(dir, name), cutoff)
		if tooLarge || (ok && len(data) > testEvidenceMax) {
			ignored++
			warns = append(warns, fmt.Sprintf("harvest %s: skip %s (exceeds 25 MiB)", key, name))
			continue
		}
		if !ok {
			continue
		}
		fresh = append(fresh, HarvestFile{Filename: name, Data: data})
	}
	if len(fresh) > testEvidenceLimit {
		extra := len(fresh) - testEvidenceLimit
		ignored += extra
		warns = append(warns, fmt.Sprintf("harvest %s: skip %d image(s) past the 12 image cap", key, extra))
		fresh = fresh[:testEvidenceLimit]
	}
	return fresh, ignored, warns
}

// RedactRaw replaces the secret before newline normalization, so a value
// that itself contains CR LF is one match rather than two lines.
func RedactRaw(s, secret string) string {
	if secret == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

func RedactCut(s, secret string) string {
	s = strings.TrimSpace(s)
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	if utf8.RuneCountInString(s) <= testActualMax {
		return s
	}
	i := 0
	for n := 0; n < testActualMax; n++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}
