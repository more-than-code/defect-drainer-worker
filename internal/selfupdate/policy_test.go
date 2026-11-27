package selfupdate

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 27, h, m, 0, 0, time.UTC) }
	w, err := ParseWindow("02:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		h, m int
		want bool
	}{{1, 59, false}, {2, 0, true}, {4, 59, true}, {5, 0, false}} {
		if got := w.Open(at(c.h, c.m)); got != c.want {
			t.Errorf("02:00-05:00 at %02d:%02d = %v", c.h, c.m, got)
		}
	}
	wrap, err := ParseWindow("23:30-01:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		h, m int
		want bool
	}{{23, 29, false}, {23, 30, true}, {0, 30, true}, {1, 0, false}, {12, 0, false}} {
		if got := wrap.Open(at(c.h, c.m)); got != c.want {
			t.Errorf("23:30-01:00 at %02d:%02d = %v", c.h, c.m, got)
		}
	}
	if !(Window{}).Open(at(12, 0)) {
		t.Error("unset window is closed")
	}
	for _, bad := range []string{"", "2:00-5:00", "02:00", "02:00-02:00", "24:00-01:00", "02:60-03:00", "02:00-05:00-06:00", "aa:bb-cc:dd"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) accepted", bad)
		}
	}
}

func TestValidProxy(t *testing.T) {
	for _, ok := range []string{"http://proxy.internal:3128", "https://10.0.0.1:8443", "http://[::1]:3128"} {
		if !ValidProxy(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "proxy:3128", "socks5://p:1080", "http://u:p@p:3128", "http://p", "http://p:3128/x", "http://p:3128?a", "http://p:0", "http://:3128"} {
		if ValidProxy(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
