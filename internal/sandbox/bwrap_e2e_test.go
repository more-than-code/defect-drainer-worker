package sandbox

import (
	"context"
	"os"
	"testing"
)

// TestSelfTestBwrap runs the V9 self-test against the host's real bwrap and
// socat. packaging/test/sandbox_test.sh runs it in a bookworm container.
func TestSelfTestBwrap(t *testing.T) {
	if os.Getenv("DD_SANDBOX_E2E") != "1" {
		t.Skip("DD_SANDBOX_E2E=1 (packaging/test/sandbox_test.sh)")
	}
	l, err := Find()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SelfTest(context.Background()); err != nil {
		t.Fatalf("self-test: %v", err)
	}
	t.Logf("self-test passed with %s and %s (--size %v, --disable-userns %v)", l.Bwrap, l.Socat, l.Size, l.DisableUserns)
}
