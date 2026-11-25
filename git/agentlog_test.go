package git

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent can plant agent.log as a link before a Resume. The worker's
// appends must not reach the link's target.
func TestOpenAgentLogDoesNotFollowLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			target := filepath.Join(outside, "victim")
			if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "agent.log")
			var err error
			if kind == "symlink" {
				err = os.Symlink(target, link)
			} else {
				err = os.Link(target, link)
			}
			if err != nil {
				t.Fatal(err)
			}
			f, err := openAgentLog(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("agent output\n"); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if b, _ := os.ReadFile(target); string(b) != "keep" {
				t.Fatalf("target written through the %s: %q", kind, b)
			}
			if b, _ := os.ReadFile(link); string(b) != "agent output\n" {
				t.Fatalf("agent.log = %q", b)
			}
		})
	}
}

func TestOpenAgentLogAppends(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.log"), []byte("run 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openAgentLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("run 2\n")
	f.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "agent.log")); string(b) != "run 1\nrun 2\n" {
		t.Fatalf("agent.log = %q", b)
	}
}

func TestOpenAgentLogRefusesASymlinkedDir(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "handoff")
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if f, err := openAgentLog(dir); err == nil {
		f.Close()
		t.Fatal("opened agent.log under a symlinked directory")
	}
}
