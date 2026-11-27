//go:build release

package release

import "testing"

// TestCompiledKeys runs in the release workflow (-tags release): a release
// ships only with real, distinct keys.
func TestCompiledKeys(t *testing.T) {
	if len(PublicKeys) == 0 {
		t.Fatal("PublicKeys is empty")
	}
	seen := map[string]bool{}
	for i, k := range PublicKeys {
		if len(k) != 32 {
			t.Errorf("key %d is %d bytes", i, len(k))
		}
		if IsPlaceholder(k) {
			t.Errorf("key %d is the placeholder", i)
		}
		if seen[string(k)] {
			t.Errorf("key %d is a duplicate", i)
		}
		seen[string(k)] = true
	}
}
