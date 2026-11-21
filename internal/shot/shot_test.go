package shot

import (
	"strings"
	"testing"
)

func TestParseActivePort(t *testing.T) {
	if u, ok := parseActivePort("41231\n/devtools/browser/abc-123\n"); !ok || u != "ws://127.0.0.1:41231/devtools/browser/abc-123" {
		t.Fatalf("%q %v", u, ok)
	}
	for _, bad := range []string{"", "41231", "x\n/devtools/browser/a", "0\n/devtools/browser/a", "70000\n/devtools/browser/a",
		"41231\n/devtools/page/a", "41231\n/devtools/browser/a\nextra"} {
		if _, ok := parseActivePort(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClean(t *testing.T) {
	if got := clean("a\nb\x00c\x7f"); got != "a b c" {
		t.Fatalf("%q", got)
	}
	if got := clean(strings.Repeat("x", 200)); len(got) != 120 {
		t.Fatalf("len %d", len(got))
	}
}

func TestJS(t *testing.T) {
	if got := js(`a"b</script>`); got != `"a\"b\u003c/script\u003e"` {
		t.Fatalf("%s", got)
	}
}
