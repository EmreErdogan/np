package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emre/np/internal/clock"
)

func open(t *testing.T, node string) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), node)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScanCommitsExternalEdits(t *testing.T) {
	s := open(t, "a")
	p, _ := s.Path("todo")
	os.WriteFile(p, []byte("one"), 0o644)
	changed, err := s.Scan()
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	m := s.Get("todo")
	if m.Clock.String() != "a:1" || len(m.History) != 1 {
		t.Fatalf("meta %+v", m)
	}
	os.WriteFile(p, []byte("two"), 0o644)
	s.Scan()
	if s.Get("todo").Clock.String() != "a:2" {
		t.Fatal("expected bump")
	}
	os.Remove(p)
	s.Scan()
	if !s.Get("todo").Deleted || s.Get("todo").Clock.String() != "a:3" {
		t.Fatal("expected tombstone")
	}
	// Reopen persists.
	s2, _ := Open(s.Dir, "a")
	if !s2.Get("todo").Deleted {
		t.Fatal("index not persisted")
	}
}

func transfer(t *testing.T, from, to *Store, name string) ApplyResult {
	t.Helper()
	m := from.Get(name)
	var data []byte
	if !m.Deleted {
		data, _ = from.Read(name)
	}
	r, err := to.Apply(*m, data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestApplyAcceptsNewerAndRejectsOlder(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("v1"))
	if r := transfer(t, a, b, "n"); r != Accepted {
		t.Fatal(r)
	}
	got, _ := b.Read("n")
	if string(got) != "v1" {
		t.Fatal(string(got))
	}
	if r := transfer(t, a, b, "n"); r != Unchanged {
		t.Fatal(r)
	}
	b.Write("n", []byte("v2"))
	if r := transfer(t, a, b, "n"); r != Rejected {
		t.Fatal(r)
	}
	if r := transfer(t, b, a, "n"); r != Accepted {
		t.Fatal(r)
	}
	if a.Get("n").Clock.String() != "a:1,b:1" {
		t.Fatal(a.Get("n").Clock)
	}
}

func TestApplyConflictKeepsLoser(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("base"))
	transfer(t, a, b, "n")
	a.Write("n", []byte("from a"))
	time.Sleep(10 * time.Millisecond)
	b.Write("n", []byte("from b")) // newer, wins
	if r := transfer(t, b, a, "n"); r != Conflicted {
		t.Fatal(r)
	}
	got, _ := a.Read("n")
	if string(got) != "from b" {
		t.Fatalf("winner=%q", got)
	}
	var conflicts []string
	for _, m := range a.List(false) {
		if m.Name != "n" {
			conflicts = append(conflicts, m.Name)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts=%v", conflicts)
	}
	loser, _ := a.Read(conflicts[0])
	if string(loser) != "from a" {
		t.Fatalf("loser=%q", loser)
	}
	// Merged version must dominate b's so it flows back.
	if clock.Compare(a.Get("n").Clock, b.Get("n").Clock) != clock.Dominates {
		t.Fatal("merged clock should dominate")
	}
	if r := transfer(t, a, b, "n"); r != Accepted {
		t.Fatal(r)
	}
	if r := transfer(t, a, b, conflicts[0]); r != Accepted {
		t.Fatal(r)
	}
}

func TestApplyDeleteTombstone(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("x"))
	transfer(t, a, b, "n")
	a.Delete("n")
	if r := transfer(t, a, b, "n"); r != Accepted {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "notes", "n.md")); err == nil {
		t.Fatal("file should be gone")
	}
}

func TestValidName(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/../b", ".hidden", "x.md", "/abs"} {
		if ValidName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	for _, ok := range []string{"a", "work/todo", "a.conflict-b-1"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
