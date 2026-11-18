package agentadapter

import (
	"slices"
	"testing"
)

func TestChildEnvDropsDDNamesAndForeignCredentials(t *testing.T) {
	in := []string{
		"PATH=/bin", "HOME=/h",
		"DEFECT_DRAINER_DATABASE_URL=postgres://secret", "DEFECT_DRAINER_CLAUDE_API_KEY=k",
		"DEFECT_CHANNEL_TOKEN=t", "DEFECTS_ROOT=/d", "DD_TEST_ACCOUNT_PASSWORD=pw",
		"ANTHROPIC_API_KEY=a", "CLAUDE_CODE_OAUTH_TOKEN=o", "XAI_API_KEY=x", "GEMINI_API_KEY=g",
		"APP_SECRET=s", "NOEQUALS",
	}
	got := ChildEnv(in, []string{"XAI_API_KEY"}, []string{"APP_SECRET"})
	want := []string{"PATH=/bin", "HOME=/h", "XAI_API_KEY=x"}
	if !slices.Equal(got, want) {
		t.Fatalf("ChildEnv = %v, want %v", got, want)
	}
	if got := ChildEnv(in, nil, nil); slices.Contains(got, "XAI_API_KEY=x") {
		t.Fatalf("no keep list must drop every credential: %v", got)
	}
}

func TestVersionGate(t *testing.T) {
	cases := []struct {
		found, min string
		ok         bool
	}{
		{"2.1.292", "2.1.292", true},
		{"2.1.323", "2.1.292", true},
		{"2.2.0", "2.1.292", true},
		{"2.1.291", "2.1.292", false},
		{"1.9.999", "2.1.292", false},
		{"", "2.1.292", false},
		{"x.y.z", "1.0.0", false},
	}
	for _, c := range cases {
		if got := AtLeast(c.found, c.min); got != c.ok {
			t.Errorf("AtLeast(%q,%q)=%v", c.found, c.min, got)
		}
	}
	if v := ParseVersion("2.1.292 (Claude Code)\n"); v != "2.1.292" {
		t.Errorf("ParseVersion = %q", v)
	}
	p := Probe{Adapter: Claude, State: VersionTooOld, Found: "2.0.1"}
	if p.Marker() != "claude@too-old" {
		t.Errorf("Marker = %q", p.Marker())
	}
	p.State, p.Found = VersionOK, "2.1.292"
	if p.Marker() != "claude@2.1.292" {
		t.Errorf("Marker = %q", p.Marker())
	}
}
