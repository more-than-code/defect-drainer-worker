package worker

import (
	"path/filepath"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

func TestPolicyAutoUpdateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.toml")
	base := "enabled = true\norgs = [\"org_a\"]\n"
	writeFile(t, path, base, 0o644)
	if p := LoadPolicy(path, false, rootUID); !p.Valid || p.AutoUpdate != workerproto.SelfUpdateOff || p.AutoUpdateWindow.Set || p.AutoUpdateProxy != "" {
		t.Fatalf("defaults %+v", p)
	}
	writeFile(t, path, base+"auto_update = \"server\"\nauto_update_window = \"02:00-05:00\"\nauto_update_https_proxy = \"http://proxy.example:3128\"\n", 0o644)
	p := LoadPolicy(path, false, rootUID)
	if !p.Valid || p.AutoUpdate != workerproto.SelfUpdateServer || !p.AutoUpdateWindow.Set || p.AutoUpdateWindow.Start != 120 || p.AutoUpdateProxy != "http://proxy.example:3128" {
		t.Fatalf("set %+v", p)
	}
	for body, reason := range map[string]string{
		base + "auto_update = \"always\"\n":                     "bad_auto_update",
		base + "auto_update = true\n":                           "bad_type",
		base + "auto_update_window = \"2-5\"\n":                 "bad_auto_update_window",
		base + "auto_update_https_proxy = \"socks5://p:1\"\n":   "bad_auto_update_https_proxy",
		base + "auto_update_https_proxy = \"http://u:p@p:1\"\n": "bad_auto_update_https_proxy",
	} {
		writeFile(t, path, body, 0o644)
		if p := LoadPolicy(path, false, rootUID); p.Valid || (reason != "bad_type" && p.Reason != reason) {
			t.Errorf("%q: valid=%v reason=%q, want %q", body, p.Valid, p.Reason, reason)
		}
	}
}
