package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Adapted from bot-agent's spool tests: ids are protocol ids, a full spool
// refuses instead of dropping, and age is enforced only by DropOlderThan.

func TestOrderAndMode(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLimited(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("p%020d", i+1)
		ids = append(ids, id)
		if err := s.Put(id, []byte(`{"n":`+string(rune('1'+i))+`}`), time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "spool", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
	}
	items, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("len %d", len(items))
	}
	for i, it := range items {
		if it.ID != ids[i] {
			t.Fatalf("order %v want %v", it.ID, ids[i])
		}
		if i > 0 && it.Seq <= items[i-1].Seq {
			t.Fatal("seq")
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %o", fi.Mode().Perm())
	}
	if err := s.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	items, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != ids[0] || items[1].ID != ids[2] {
		t.Fatalf("%v", items)
	}
	for _, bad := range []string{"../x", "a.b", "", "-x"} {
		if err := s.Put(bad, []byte(`{"a":1}`), time.Now()); err == nil {
			t.Fatalf("accepted bad id %q", bad)
		}
	}
}

func TestFullRefusesAndKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLimited(dir, 400)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte(`{"k":"` + strings.Repeat("a", 80) + `"}`)
	stored := 0
	for i := 0; i < 6; i++ {
		err := s.Put(fmt.Sprintf("p%d", i), frame, time.Now())
		if errors.Is(err, ErrFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		stored++
	}
	if stored == 0 || stored >= 6 {
		t.Fatalf("stored %d", stored)
	}
	items, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != stored || items[0].ID != "p0" {
		t.Fatalf("an entry was dropped: %v", items)
	}
	n, err := s.Bytes()
	if err != nil || n == 0 || n > 400 {
		t.Fatalf("bytes %d %v", n, err)
	}
}

func TestDropOlderThan(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("old", []byte(`{"a":1}`), time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("new", []byte(`{"a":2}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.List(); len(items) != 2 {
		t.Fatalf("List dropped by age: %v", items)
	}
	if err := s.DropOlderThan(24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	items, _ := s.List()
	if len(items) != 1 || items[0].ID != "new" {
		t.Fatalf("%v", items)
	}
}

func TestReopenKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	_ = s.Put("b", []byte(`{"a":1}`), time.Now())
	_ = s.Put("a", []byte(`{"a":2}`), time.Now())
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Put("c", []byte(`{"a":3}`), time.Now())
	items, _ := s2.List()
	if len(items) != 3 || items[0].ID != "b" || items[1].ID != "a" || items[2].ID != "c" {
		t.Fatalf("%v", items)
	}
}

func TestDropsCorrupt(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "spool")
	if err := os.WriteFile(filepath.Join(sp, "not.an.id.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp, "p1.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("%v", items)
	}
	if _, err := os.Stat(filepath.Join(sp, "not.an.id.json")); !os.IsNotExist(err) {
		t.Fatal("junk kept")
	}
}
