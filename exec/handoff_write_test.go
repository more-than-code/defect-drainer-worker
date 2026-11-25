package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// An agent can leave a symlink where DD writes next. DD's write must replace
// the link and leave its target alone.
func TestVerifyJSONDoesNotFollowAPlantedSymlink(t *testing.T) {
	handoff, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "credential")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(handoff, "verify.json")); err != nil {
		t.Fatal(err)
	}
	tryWriteVerifyJSON(handoff, VerificationRun{Ok: true})

	if b, _ := os.ReadFile(target); string(b) != "secret" {
		t.Fatalf("symlink target was overwritten: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(handoff, "verify.json"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("verify.json is not a fresh regular file: %v %v", fi, err)
	}
}

func TestHandoffBriefDoesNotFollowPlantedLinks(t *testing.T) {
	handoff, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "config")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(handoff, "BRIEF.md")); err != nil {
		t.Fatal(err)
	}
	// A hardlink passes O_NOFOLLOW; it must be unlinked, not written through.
	if err := os.Link(target, filepath.Join(handoff, "PROCESS.md")); err != nil {
		t.Fatal(err)
	}
	if err := WriteHandoffBrief(handoff, BatchFixBriefInput{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("link target was overwritten: %q", b)
	}
}

func TestEnsureHandoffDirRefusesASymlinkedDir(t *testing.T) {
	handoff, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(handoff, "review")); err != nil {
		t.Fatal(err)
	}
	if err := WriteHandoffFile(handoff, "review/x/REVIEW.md", []byte("x"), 0o644); err == nil {
		t.Fatal("write through a symlinked directory succeeded")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote outside the handoff: %v", ents)
	}
}

func TestWriteHandoffFileRejectsEscapes(t *testing.T) {
	handoff := t.TempDir()
	for _, rel := range []string{"", ".", "..", "../x", "/etc/x"} {
		if err := WriteHandoffFile(handoff, rel, []byte("x"), 0o644); err == nil {
			t.Errorf("%q: accepted", rel)
		}
	}
}

func TestWriteHandoffFileMode(t *testing.T) {
	handoff := t.TempDir()
	if err := WriteHandoffFile(handoff, "a/b/c.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(handoff, "a/b/c.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", fi, err)
	}
}

// On Resume the handoff is reused. Whatever the agent left under .tooling must
// not run as the worker: provisioning starts again from the host SDK and
// probes the host SDK.
func TestFlutterProvisionNeverRunsTheHandoffCopy(t *testing.T) {
	fvm := t.TempDir()
	t.Setenv("FVM_HOME", fvm)
	marks := t.TempDir()
	sdkBin := filepath.Join(fvm, "versions", "3.1.0", "bin")
	if err := os.MkdirAll(sdkBin, 0o755); err != nil {
		t.Fatal(err)
	}
	hostFlutter := "#!/bin/sh\necho host >> " + filepath.Join(marks, "host") + "\n"
	if err := os.WriteFile(filepath.Join(sdkBin, "flutter"), []byte(hostFlutter), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".fvmrc"), []byte(`{"flutter":"3.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wts := []git.WorktreeBinding{{Repo: "app", WorktreeAbs: wt}}
	handoff := t.TempDir()

	if p := ProvisionFlutterToolchain(handoff, wts, nil, nil); !p.Ok {
		t.Fatal("first provision failed")
	}
	// The agent plants its own binary in the copy and in the shim dir.
	planted := "#!/bin/sh\necho planted >> " + filepath.Join(marks, "planted") + "\n"
	for _, p := range []string{".tooling/flutter-sdk/bin/flutter", ".tooling/bin/flutter"} {
		if err := os.WriteFile(filepath.Join(handoff, p), []byte(planted), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if p := ProvisionFlutterToolchain(handoff, wts, nil, nil); !p.Ok {
		t.Fatal("second provision failed")
	}
	if _, err := os.Stat(filepath.Join(marks, "planted")); err == nil {
		t.Fatal("the worker ran the agent's planted flutter")
	}
	if b, _ := os.ReadFile(filepath.Join(marks, "host")); strings.Count(string(b), "host") != 2 {
		t.Fatalf("host SDK probed %q, want twice", b)
	}
	b, err := os.ReadFile(filepath.Join(handoff, ".tooling/flutter-sdk/bin/flutter"))
	if err != nil || string(b) != hostFlutter {
		t.Fatalf("handoff SDK was not replaced by a fresh copy: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(handoff, ".tooling/flutter-sdk/3.1.0")); err == nil {
		t.Fatal("the second copy nested inside the first")
	}
}

// A repo-committed SKILL.md symlink must not make the worker read a host file
// into SKILLS.md.
func TestSkillListingDoesNotFollowSymlinks(t *testing.T) {
	wt, outside := t.TempDir(), t.TempDir()
	host := filepath.Join(outside, "SKILL.md")
	if err := os.WriteFile(host, []byte("---\nname: host-secret-name\ndescription: host-secret-desc\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(wt, ".agents", "skills", "evil")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wt, ".agents", "skills", "evil2")); err != nil {
		t.Fatal(err)
	}
	md := BuildSkillsMarkdown([]git.WorktreeBinding{{Repo: "app", WorktreeAbs: wt}})
	if strings.Contains(md, "host-secret") {
		t.Fatalf("SKILLS.md carries a host file's frontmatter:\n%s", md)
	}
}
