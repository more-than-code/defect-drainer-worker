package cgroup

import (
	"slices"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"1048576": 1 << 20, "512M": 512 << 20, "3G": 3 << 30, "2GiB": 2 << 30, "1t": 1 << 40, " 64K ": 64 << 10,
	} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1G", "1.5G", "G", "3X", "99999999999T"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestLimitsFiles(t *testing.T) {
	l := Limits{MemoryBytes: 3 << 30, Tasks: 4096, CPUs: 1.5}
	want := [][2]string{{"memory.max", "3221225472"}, {"pids.max", "4096"}, {"cpu.max", "150000 100000"}}
	if got := l.files(); !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if got := l.String(); got != "memory 3 GiB, 1.5 CPUs, 4096 tasks" {
		t.Fatalf("%q", got)
	}
	if got := (Limits{}).String(); got != "none" {
		t.Fatalf("%q", got)
	}
	if got := (Limits{CPUs: 0.001}).files(); got[0][1] != "1000 100000" {
		t.Fatalf("cpu floor: %v", got)
	}
	if got := FormatBytes(1536 << 20); got != "1536 MiB" {
		t.Fatalf("%q", got)
	}
}
