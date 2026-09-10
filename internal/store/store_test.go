package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
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
	// The merge is one history entry (base, a's edit, merged), not two.
	if h := a.Get("n").History; len(h) != 3 || h[2].Clock.String() != "a:3,b:1" {
		t.Fatalf("history=%+v", h)
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

func TestExtensions(t *testing.T) {
	cases := map[string]string{
		"todo": "todo.md", "cfg.json": "cfg.json", "deploy.sh": "deploy.sh",
		"a.conflict-laptop-20260908-202227": "a.conflict-laptop-20260908-202227.md",
		"work/notes.v2":                     "work/notes.v2", "dir.name/todo": "dir.name/todo.md",
	}
	for name, want := range cases {
		if got := FileName(name); got != want {
			t.Errorf("FileName(%q)=%q want %q", name, got, want)
		}
	}
	if Ext("cfg.JSON") != "json" || Ext("todo") != "" || Canon("todo.md") != "todo" {
		t.Fatal("ext/canon")
	}
	s := open(t, "a")
	os.WriteFile(filepath.Join(s.Dir, "notes", "cfg.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(s.Dir, "notes", "plain.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(s.Dir, "notes", "README"), []byte("ignored"), 0o644)
	changed, _ := s.Scan()
	if len(changed) != 2 || changed[0] != "cfg.json" || changed[1] != "plain" {
		t.Fatalf("changed=%v", changed)
	}
	if got, _ := s.Read("cfg.json"); string(got) != "{}" {
		t.Fatal(string(got))
	}
}

func TestJoin(t *testing.T) {
	cases := []struct{ existing, text, want string }{
		{"", "hi", "hi\n"},
		{"# T\n\npara\n", "next", "# T\n\npara\n\nnext\n"},
		{"para", "next\n\n", "para\n\nnext\n"},
		{"para\n\n", "next", "para\n\nnext\n"},
		{"- a\n- b\n", "- c", "- a\n- b\n- c\n"},
		{"- a\n- [ ] b\n", "- [x] c", "- a\n- [ ] b\n- [x] c\n"},
		{"1. a\n", "2. b", "1. a\n2. b\n"},
		{"- a\n", "plain", "- a\n\nplain\n"},
		{"para\n", "- item", "para\n\n- item\n"},
	}
	for _, c := range cases {
		if got := string(Join(c.existing, c.text)); got != c.want {
			t.Errorf("Join(%q,%q)=%q want %q", c.existing, c.text, got, c.want)
		}
	}
	s := open(t, "a")
	if err := s.Append("log", "first"); err != nil {
		t.Fatal(err)
	}
	s.Append("log", "second")
	got, _ := s.Read("log")
	if string(got) != "first\n\nsecond\n" || s.Get("log").Clock.String() != "a:2" {
		t.Fatalf("%q %s", got, s.Get("log").Clock)
	}
	if err := s.Append("log", "  "); err == nil {
		t.Fatal("empty append should fail")
	}
}

func TestApplyMergesDisjointConcurrentEdits(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("one\ntwo\nthree\n"))
	transfer(t, a, b, "n")
	a.Write("n", []byte("ONE\ntwo\nthree\n"))
	b.Write("n", []byte("one\ntwo\nthree\nfour\n"))
	if r := transfer(t, b, a, "n"); r != Merged {
		t.Fatal(r)
	}
	got, _ := a.Read("n")
	if string(got) != "ONE\ntwo\nthree\nfour\n" {
		t.Fatalf("merged=%q", got)
	}
	if n := len(a.List(false)); n != 1 {
		t.Fatalf("expected no conflict copy, have %d notes", n)
	}
	m := a.Get("n")
	if m.ModBy != "a" || len(m.History) != 3 || m.History[2].Hash != m.Hash {
		t.Fatalf("meta=%+v", m)
	}
	if clock.Compare(m.Clock, b.Get("n").Clock) != clock.Dominates {
		t.Fatal("merged clock should dominate b")
	}
	// The merge flows back to b as a plain accept.
	if r := transfer(t, a, b, "n"); r != Accepted {
		t.Fatal(r)
	}
	got, _ = b.Read("n")
	if string(got) != "ONE\ntwo\nthree\nfour\n" {
		t.Fatalf("b=%q", got)
	}
	// Scan must not see the merged file as a new local edit.
	if ch, _ := a.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
}

func TestApplyConcurrentIdenticalContent(t *testing.T) {
	// a and b sync directly and each merges the other's edit; the results
	// are identical but their clocks are concurrent. No conflict copy.
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("one\ntwo\n"))
	transfer(t, a, b, "n")
	a.Write("n", []byte("ONE\ntwo\n"))
	b.Write("n", []byte("one\ntwo\nthree\n"))
	ma, da := *a.Get("n"), mustRead(t, a, "n")
	mb, db := *b.Get("n"), mustRead(t, b, "n")
	if r, _ := a.Apply(mb, db); r != Merged {
		t.Fatal(r)
	}
	if r, _ := b.Apply(ma, da); r != Merged {
		t.Fatal(r)
	}
	if clock.Compare(a.Get("n").Clock, b.Get("n").Clock) != clock.Concurrent {
		t.Fatal("expected concurrent merge results")
	}
	if r := transfer(t, b, a, "n"); r != Unchanged {
		t.Fatal(r)
	}
	if n := len(a.List(false)); n != 1 {
		t.Fatalf("have %d notes", n)
	}
	if r := transfer(t, a, b, "n"); r != Accepted {
		t.Fatal(r)
	}
	if clock.Compare(a.Get("n").Clock, b.Get("n").Clock) != clock.Equal {
		t.Fatal("clocks should converge")
	}
}

func TestApplyOverlappingEditsStillConflict(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("one\ntwo\n"))
	transfer(t, a, b, "n")
	a.Write("n", []byte("uno\ntwo\n"))
	time.Sleep(10 * time.Millisecond)
	b.Write("n", []byte("bir\ntwo\n"))
	if r := transfer(t, b, a, "n"); r != Conflicted {
		t.Fatal(r)
	}
	if n := len(a.List(false)); n != 2 {
		t.Fatalf("have %d notes", n)
	}
}

func mustRead(t *testing.T, s *Store, name string) []byte {
	t.Helper()
	d, err := s.Read(name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestScanReloadsIndexWrittenByAnotherProcess(t *testing.T) {
	// A daemon (d) and a CLI command (c) share one directory. c pulls a note
	// and a file from a peer; d's next Scan must see the new index, not
	// re-commit them as its own local changes.
	dir := t.TempDir()
	d, err := Open(dir, "joy")
	if err != nil {
		t.Fatal(err)
	}
	d.Write("mine", []byte("x"))
	c, err := Open(dir, "joy")
	if err != nil {
		t.Fatal(err)
	}
	peer := open(t, "m1")
	peer.Write("n", []byte("from m1"))
	put(t, peer, "f.png", "img")
	time.Sleep(5 * time.Millisecond) // distinct index mtimes on coarse filesystems
	transfer(t, peer, c, "n")
	send(t, peer, c, "f.png", true)

	ch, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatalf("daemon re-committed %v", ch)
	}
	if m := d.Get("n"); m == nil || m.Clock.String() != "m1:1" || m.ModBy != "m1" {
		t.Fatalf("note=%+v", m)
	}
	if m := d.File("f.png"); m == nil || m.Clock.String() != "m1:1" || m.ModBy != "m1" {
		t.Fatalf("file=%+v", m)
	}
	if m := d.Get("mine"); m == nil {
		t.Fatal("daemon's own note lost")
	}
}

func TestAddVersionsUnionAndOrder(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("v1"))
	time.Sleep(2 * time.Millisecond)
	a.Write("n", []byte("v2"))
	time.Sleep(2 * time.Millisecond)
	a.Write("n", []byte("v3"))
	transfer(t, a, b, "n") // b has only v3
	if len(b.Get("n").History) != 1 {
		t.Fatal("b should have one version")
	}
	if HistoryDigest(a.Get("n")) == HistoryDigest(b.Get("n")) {
		t.Fatal("digests should differ")
	}
	here, there := b.MissingVersions("n", a.Get("n").History)
	if len(here) != 2 || len(there) != 0 {
		t.Fatalf("missing here=%d there=%d", len(here), len(there))
	}
	// Without snapshots nothing is added.
	if n, _ := b.AddVersions("n", here); n != 0 {
		t.Fatal("should skip versions without content")
	}
	for _, v := range here {
		data, _ := a.Snapshot("n", v.Hash)
		if err := b.PutSnapshot("n", v.Hash, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.PutSnapshot("n", here[0].Hash, []byte("wrong")); err == nil {
		t.Fatal("wrong content should be rejected")
	}
	n, err := b.AddVersions("n", here)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	h := b.Get("n").History
	if len(h) != 3 || h[0].Seq != 1 || h[2].Seq != 3 || h[2].Clock.String() != b.Get("n").Clock.String() {
		t.Fatalf("history=%+v", h)
	}
	if got, _ := b.Snapshot("n", h[0].Hash); string(got) != "v1" {
		t.Fatalf("oldest=%q", got)
	}
	if HistoryDigest(a.Get("n")) != HistoryDigest(b.Get("n")) {
		t.Fatal("digests should now match")
	}
	// Idempotent.
	if n, _ := b.AddVersions("n", a.Get("n").History); n != 0 {
		t.Fatal("re-adding should add nothing")
	}
}

func TestAddVersionsKeepsCurrentLast(t *testing.T) {
	// A pulled older version with a later mtime (clock skew) must not
	// displace the current version from the end of the ledger.
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("cur"))
	transfer(t, a, b, "n")
	skewed := Version{Hash: hashOf([]byte("old")), ModTime: time.Now().Add(time.Hour), ModBy: "x", Clock: clock.Clock{"x": 1}}
	b.PutSnapshot("n", skewed.Hash, []byte("old"))
	if n, _ := b.AddVersions("n", []Version{skewed}); n != 1 {
		t.Fatal("should add")
	}
	h := b.Get("n").History
	if h[len(h)-1].Hash != b.Get("n").Hash {
		t.Fatalf("current not last: %+v", h)
	}
}
