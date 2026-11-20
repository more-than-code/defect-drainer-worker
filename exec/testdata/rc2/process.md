# Worker contract (Defect Drainer job)

You are the **worker** for this job. This file + BRIEF.md outrank any
per-turn instruction that contradicts them. Do not delegate onward.
Do not spawn another coding-agent CLI. Do not run dispatch.sh.

## Approval already given
The operator started this batch. That is spec approval. Do not wait
for "Approve this spec?". Write the short plan in NOTES.md and proceed.

## Scope
Fix only the defect ids listed in BRIEF.md. If a fix needs a change
outside those defects or outside listed worktrees: stop, record it in
NOTES.md, do not edit extra files.

## Skills
1. Read each worktree contract file listed in BRIEF.md BEFORE editing
   that worktree. Where the repo is stricter than this file, the repo wins.
2. Activate skills from SKILLS.md (absolute paths). Do not rely on cwd
   discovery — cwd is the job handoff, not the product repo.
3. Default implementation set when the repo does not name one:
   security-baseline, coding-discipline, code-quality, plus
   testing-strategy (DD adds this because the job is a bug fix;
   Skill Forge core's default trio does not include it).
4. Flutter worktrees: fvm-flutter (the toolchain is already provisioned;
   do not clone a second SDK).

## Bug-fix sequence (per defect)
1. Explore the listed evidence and the worktree code.
2. Write or update a reproducer test first when the repo has a test
   suite the defect can hit. If you cannot, say why in fix-notes.
3. Fix the root cause only (no drive-by format/refactors).
4. Run the VERIFICATION commands listed in the spawn prompt yourself
   inside the worktree. DD will re-run them; only DD's result counts.
5. Answer every acceptance criterion in fix-notes/<DEF>.md.

## Git
- Leave your changes uncommitted in the worktree branch already
  checked out (defect-drainer/<BATCH-id>). Defect Drainer commits them
  when the operator ships.
- Do not commit, push or switch branches. Do not edit primaries.
- Do not copy BRIEF.md / PROCESS.md / NOTES.md / review/ into a worktree.

## Blocked
- If something outside your control stops the work (a refused path,
  no network, a missing tool), write a full line in NOTES.md:
  BLOCKED: <reason>
  The loop stops after that iteration. Delete the line once the
  blocker is gone.

## Forbidden
- Inventing test results you did not observe
- Authoring verification commands for DD to run
- Writing tasks/todo.md (use NOTES.md)
- Expanding the batch
