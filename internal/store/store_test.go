package store

import (
	"os"
	"path/filepath"
	"strings"
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

func put(t *testing.T, s *Store, name, content string) {
	t.Helper()
	if err := s.Put(name, strings.NewReader(content), s.Node); err != nil {
		t.Fatal(err)
	}
}

// send applies from's version of name to to, with content when withContent is set.
func send(t *testing.T, from, to *Store, name string, withContent bool) ApplyResult {
	t.Helper()
	m := from.Get(name)
	var r ApplyResult
	var err error
	if withContent && m.Have && !m.Deleted {
		f, oerr := from.OpenFile(name)
		if oerr != nil {
			t.Fatal(oerr)
		}
		defer f.Close()
		r, err = to.Apply(m.Wire(), f)
	} else {
		r, err = to.Apply(m.Wire(), nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func transfer(t *testing.T, from, to *Store, name string) ApplyResult {
	t.Helper()
	return send(t, from, to, name, true)
}

func mustRead(t *testing.T, s *Store, name string) string {
	t.Helper()
	d, err := s.Read(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

const bin = "\x00\x01binary "

func TestScanCommitsExternalEdits(t *testing.T) {
	s := open(t, "a")
	p, _ := s.Path("todo.md")
	os.WriteFile(p, []byte("one"), 0o644)
	changed, err := s.Scan()
	if err != nil || len(changed) != 1 || changed[0] != "todo.md" {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	m := s.Get("todo.md")
	if m.Clock.String() != "a:1" || len(m.History) != 1 || m.Blob || !m.Have || m.Size != 3 {
		t.Fatalf("meta %+v", m)
	}
	// Same size, written within the same clock tick: still noticed.
	os.WriteFile(p, []byte("two"), 0o644)
	s.Scan()
	if s.Get("todo.md").Clock.String() != "a:2" {
		t.Fatal("expected bump")
	}
	if got, _ := s.Snapshot("todo.md", s.Get("todo.md").History[0].Hash); string(got) != "one" {
		t.Fatalf("snapshot=%q", got)
	}
	os.Remove(p)
	s.Scan()
	if !s.Get("todo.md").Deleted || s.Get("todo.md").Clock.String() != "a:3" {
		t.Fatal("expected tombstone")
	}
	// Reopen persists.
	s2, _ := Open(s.Dir, "a")
	if !s2.Get("todo.md").Deleted {
		t.Fatal("index not persisted")
	}
	// Temp and dot files are ignored.
	os.WriteFile(filepath.Join(s.Dir, "files", "x.tmp"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(s.Dir, "files", ".hidden"), []byte("x"), 0o644)
	if ch, _ := s.Scan(); len(ch) != 0 {
		t.Fatalf("changed=%v", ch)
	}
}

func TestScanClassifiesTextAndBlob(t *testing.T) {
	s := open(t, "a")
	put(t, s, "pics/cat.png", "PNG\x00data")
	put(t, s, "cfg.json", "{}")
	put(t, s, "README", "plain text without an extension")
	p, _ := s.Path("huge.txt")
	os.WriteFile(p, []byte(strings.Repeat("a", TextMax+1)), 0o644)
	s.Scan()
	for name, blob := range map[string]bool{"pics/cat.png": true, "cfg.json": false, "README": false, "huge.txt": true} {
		m := s.Get(name)
		if m == nil || m.Blob != blob || len(m.History) != 1 || m.History[0].Blob != blob {
			t.Fatalf("%s: %+v", name, m)
		}
		if s.HasSnapshot(name, m.Hash) == blob {
			t.Fatalf("%s: snapshot presence should be %v", name, !blob)
		}
	}
	if _, ok := s.ReadText("pics/cat.png"); ok {
		t.Fatal("binary is not text")
	}
	if d, ok := s.ReadText("README"); !ok || !strings.HasPrefix(string(d), "plain") {
		t.Fatal("README should read as text")
	}
	// A blob that becomes text gains history from then on.
	put(t, s, "pics/cat.png", "now a caption")
	if m := s.Get("pics/cat.png"); m.Blob || len(m.History) != 2 || !s.HasSnapshot(m.Name, m.Hash) {
		t.Fatalf("meta=%+v", m)
	}
}

func TestScanSkipsUnchangedFilesByStamp(t *testing.T) {
	s := open(t, "a")
	put(t, s, "n.md", "v1")
	if s.Get("n.md").Stamp != "" {
		t.Fatal("a file changed just now must not be trusted by size and mtime yet")
	}
	p := s.LocalPath("n.md")
	past := time.Now().Add(-time.Minute)
	os.Chtimes(p, past, past)
	s.Scan()
	if s.Get("n.md").Stamp == "" || s.Get("n.md").Clock.String() != "a:1" {
		t.Fatalf("meta=%+v", s.Get("n.md"))
	}
	// The stamp is local and never travels.
	if w := s.Get("n.md").Wire(); w.Stamp != "" || w.History != nil {
		t.Fatalf("wire=%+v", w)
	}
	os.WriteFile(p, []byte("v2"), 0o644)
	if ch, _ := s.Scan(); len(ch) != 1 {
		t.Fatalf("changed=%v", ch)
	}
}

func TestApplyAcceptsNewerAndRejectsOlder(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("v1"))
	if r := transfer(t, a, b, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if got := mustRead(t, b, "n.md"); got != "v1" {
		t.Fatal(got)
	}
	if r := transfer(t, a, b, "n.md"); r != Unchanged {
		t.Fatal(r)
	}
	b.Write("n.md", []byte("v2"))
	if r := transfer(t, a, b, "n.md"); r != Rejected {
		t.Fatal(r)
	}
	if r := transfer(t, b, a, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if a.Get("n.md").Clock.String() != "a:1,b:1" {
		t.Fatal(a.Get("n.md").Clock)
	}
	// A received version is not a local edit.
	if ch, _ := a.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
}

func TestApplyRejectsWrongContent(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("v1"))
	transfer(t, a, b, "n.md")
	a.Write("n.md", []byte("v2"))
	if _, err := b.Apply(a.Get("n.md").Wire(), strings.NewReader("tampered")); err == nil {
		t.Fatal("expected hash mismatch")
	}
	// The previous version is untouched, in the index and on disk.
	if m := b.Get("n.md"); m.Clock.String() != "a:1" || len(m.History) != 1 || mustRead(t, b, "n.md") != "v1" {
		t.Fatalf("b=%+v", m)
	}
}

func TestApplyConflictKeepsLoser(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("base"))
	transfer(t, a, b, "n.md")
	a.Write("n.md", []byte("from a"))
	time.Sleep(10 * time.Millisecond)
	b.Write("n.md", []byte("from b")) // newer, wins
	if r := transfer(t, b, a, "n.md"); r != Conflicted {
		t.Fatal(r)
	}
	if got := mustRead(t, a, "n.md"); got != "from b" {
		t.Fatalf("winner=%q", got)
	}
	var conflicts []string
	for _, m := range a.List(false) {
		if m.Name != "n.md" {
			conflicts = append(conflicts, m.Name)
		}
	}
	if len(conflicts) != 1 || !strings.HasPrefix(conflicts[0], "n.conflict-a-") || !strings.HasSuffix(conflicts[0], ".md") {
		t.Fatalf("conflicts=%v", conflicts)
	}
	if loser := mustRead(t, a, conflicts[0]); loser != "from a" {
		t.Fatalf("loser=%q", loser)
	}
	// The merge is one history entry (base, a's edit, merged), not two.
	if h := a.Get("n.md").History; len(h) != 3 || h[2].Clock.String() != "a:3,b:1" {
		t.Fatalf("history=%+v", h)
	}
	// Merged version must dominate b's so it flows back.
	if clock.Compare(a.Get("n.md").Clock, b.Get("n.md").Clock) != clock.Dominates {
		t.Fatal("merged clock should dominate")
	}
	if r := transfer(t, a, b, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if r := transfer(t, a, b, conflicts[0]); r != Accepted {
		t.Fatal(r)
	}
}

func TestApplyDeleteTombstone(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("x"))
	transfer(t, a, b, "n.md")
	if err := a.Delete("n.md"); err != nil {
		t.Fatal(err)
	}
	if r := transfer(t, a, b, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if _, err := os.Stat(b.LocalPath("n.md")); err == nil {
		t.Fatal("file should be gone")
	}
	if len(b.List(false)) != 0 || len(b.List(true)) != 1 {
		t.Fatal("tombstone expected")
	}
	if err := a.Delete("n.md"); err == nil {
		t.Fatal("deleting a tombstone should fail")
	}
}

func TestNames(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/../b", ".hidden", "x.tmp", "/abs", "a//b", `a\b`} {
		if ValidName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	for _, ok := range []string{"a", "work/todo.md", "a.conflict-b-1.md", "a.png", "README"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for arg, want := range map[string]string{
		"todo": "todo.md", "cfg.json": "cfg.json", "deploy.sh": "deploy.sh", "todo.md": "todo.md",
		"work/notes.v2": "work/notes.v2", "dir.name/todo": "dir.name/todo.md",
		"a.conflict-laptop-20260908-202227": "a.conflict-laptop-20260908-202227.md",
	} {
		if got := DefaultName(arg); got != want {
			t.Errorf("DefaultName(%q)=%q want %q", arg, got, want)
		}
	}
	if Ext("cfg.JSON") != "json" || Ext("todo") != "" || !IsMarkdown("a/b.md") || IsMarkdown("b.json") {
		t.Fatal("ext")
	}
	at := time.Date(2026, 9, 8, 20, 22, 27, 0, time.UTC)
	for name, want := range map[string]string{
		"todo.md": "todo.conflict-desk-20260908-202227.md", "pics/cat.jpg": "pics/cat.conflict-desk-20260908-202227.jpg",
		"README": "README.conflict-desk-20260908-202227",
	} {
		if got := conflictName(name, "desk", at); got != want {
			t.Errorf("conflictName(%q)=%q want %q", name, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	s := open(t, "a")
	put(t, s, "plan.md", "the markdown")
	put(t, s, "plan", "a file without extension")
	put(t, s, "todo.md", "x")
	put(t, s, "pic.jpg", bin)
	for arg, want := range map[string]string{
		"plan": "plan", "plan.md": "plan.md", "todo": "todo.md", "todo.md": "todo.md", "pic.jpg": "pic.jpg", "nope": "",
	} {
		if got := s.Resolve(arg); got != want {
			t.Errorf("Resolve(%q)=%q want %q", arg, got, want)
		}
	}
	// A tombstone is found when nothing live matches.
	s.Delete("todo.md")
	if got := s.Resolve("todo"); got != "todo.md" {
		t.Fatalf("tombstone: %q", got)
	}
}

func TestJoinAndAppend(t *testing.T) {
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
	if err := s.Append("log.md", "first"); err != nil {
		t.Fatal(err)
	}
	s.Append("log.md", "second")
	if got := mustRead(t, s, "log.md"); got != "first\n\nsecond\n" || s.Get("log.md").Clock.String() != "a:2" {
		t.Fatalf("%q %s", got, s.Get("log.md").Clock)
	}
	if err := s.Append("log.md", "  "); err == nil {
		t.Fatal("empty append should fail")
	}
	put(t, s, "pic.jpg", bin)
	if err := s.Append("pic.jpg", "caption"); err == nil {
		t.Fatal("appending to a binary file should fail")
	}
}

func TestApplyMergesDisjointConcurrentEdits(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("one\ntwo\nthree\n"))
	transfer(t, a, b, "n.md")
	a.Write("n.md", []byte("ONE\ntwo\nthree\n"))
	b.Write("n.md", []byte("one\ntwo\nthree\nfour\n"))
	if r := transfer(t, b, a, "n.md"); r != Merged {
		t.Fatal(r)
	}
	if got := mustRead(t, a, "n.md"); got != "ONE\ntwo\nthree\nfour\n" {
		t.Fatalf("merged=%q", got)
	}
	if n := len(a.List(false)); n != 1 {
		t.Fatalf("expected no conflict copy, have %d files", n)
	}
	m := a.Get("n.md")
	if m.ModBy != "a" || len(m.History) != 3 || m.History[2].Hash != m.Hash || m.Size != 19 {
		t.Fatalf("meta=%+v", m)
	}
	if clock.Compare(m.Clock, b.Get("n.md").Clock) != clock.Dominates {
		t.Fatal("merged clock should dominate b")
	}
	// The merge flows back to b as a plain accept.
	if r := transfer(t, a, b, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if got := mustRead(t, b, "n.md"); got != "ONE\ntwo\nthree\nfour\n" {
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
	a.Write("n.md", []byte("one\ntwo\n"))
	transfer(t, a, b, "n.md")
	a.Write("n.md", []byte("ONE\ntwo\n"))
	b.Write("n.md", []byte("one\ntwo\nthree\n"))
	ma, da := a.Get("n.md").Wire(), mustRead(t, a, "n.md")
	mb, db := b.Get("n.md").Wire(), mustRead(t, b, "n.md")
	if r, _ := a.Apply(mb, strings.NewReader(db)); r != Merged {
		t.Fatal(r)
	}
	if r, _ := b.Apply(ma, strings.NewReader(da)); r != Merged {
		t.Fatal(r)
	}
	if clock.Compare(a.Get("n.md").Clock, b.Get("n.md").Clock) != clock.Concurrent {
		t.Fatal("expected concurrent merge results")
	}
	if r := transfer(t, b, a, "n.md"); r != Unchanged {
		t.Fatal(r)
	}
	if n := len(a.List(false)); n != 1 {
		t.Fatalf("have %d files", n)
	}
	if r := transfer(t, a, b, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if clock.Compare(a.Get("n.md").Clock, b.Get("n.md").Clock) != clock.Equal {
		t.Fatal("clocks should converge")
	}
}

func TestApplyOverlappingEditsStillConflict(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("one\ntwo\n"))
	transfer(t, a, b, "n.md")
	a.Write("n.md", []byte("uno\ntwo\n"))
	time.Sleep(10 * time.Millisecond)
	b.Write("n.md", []byte("bir\ntwo\n"))
	if r := transfer(t, b, a, "n.md"); r != Conflicted {
		t.Fatal(r)
	}
	if n := len(a.List(false)); n != 2 {
		t.Fatalf("have %d files", n)
	}
}

func TestScanReloadsIndexWrittenByAnotherProcess(t *testing.T) {
	// A daemon (d) and a CLI command (c) share one directory. c pulls two
	// files from a peer; d's next Scan must see the new index, not
	// re-commit them as its own local changes.
	dir := t.TempDir()
	d, err := Open(dir, "desk")
	if err != nil {
		t.Fatal(err)
	}
	d.Write("mine.md", []byte("x"))
	c, err := Open(dir, "desk")
	if err != nil {
		t.Fatal(err)
	}
	peer := open(t, "lap")
	peer.Write("n.md", []byte("from lap"))
	put(t, peer, "f.png", bin+"img")
	time.Sleep(5 * time.Millisecond) // distinct index mtimes on coarse filesystems
	transfer(t, peer, c, "n.md")
	transfer(t, peer, c, "f.png")

	ch, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatalf("daemon re-committed %v", ch)
	}
	for _, name := range []string{"n.md", "f.png"} {
		if m := d.Get(name); m == nil || m.Clock.String() != "lap:1" || m.ModBy != "lap" {
			t.Fatalf("%s=%+v", name, m)
		}
	}
	if m := d.Get("mine.md"); m == nil {
		t.Fatal("daemon's own file lost")
	}
}

func TestAddVersionsUnionAndOrder(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("v1"))
	time.Sleep(2 * time.Millisecond)
	a.Write("n.md", []byte("v2"))
	time.Sleep(2 * time.Millisecond)
	a.Write("n.md", []byte("v3"))
	transfer(t, a, b, "n.md") // b has only v3
	if len(b.Get("n.md").History) != 1 {
		t.Fatal("b should have one version")
	}
	if HistoryDigest(a.Get("n.md")) == HistoryDigest(b.Get("n.md")) {
		t.Fatal("digests should differ")
	}
	here, there := b.MissingVersions("n.md", a.Get("n.md").History)
	if len(here) != 2 || len(there) != 0 {
		t.Fatalf("missing here=%d there=%d", len(here), len(there))
	}
	// Without snapshots nothing is added.
	if n, _ := b.AddVersions("n.md", here); n != 0 {
		t.Fatal("should skip versions without content")
	}
	for _, v := range here {
		data, _ := a.Snapshot("n.md", v.Hash)
		if err := b.PutSnapshot("n.md", v.Hash, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.PutSnapshot("n.md", here[0].Hash, []byte("wrong")); err == nil {
		t.Fatal("wrong content should be rejected")
	}
	n, err := b.AddVersions("n.md", here)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	h := b.Get("n.md").History
	if len(h) != 3 || h[0].Seq != 1 || h[2].Seq != 3 || h[2].Clock.String() != b.Get("n.md").Clock.String() {
		t.Fatalf("history=%+v", h)
	}
	if got, _ := b.Snapshot("n.md", h[0].Hash); string(got) != "v1" {
		t.Fatalf("oldest=%q", got)
	}
	if HistoryDigest(a.Get("n.md")) != HistoryDigest(b.Get("n.md")) {
		t.Fatal("digests should now match")
	}
	// Idempotent.
	if n, _ := b.AddVersions("n.md", a.Get("n.md").History); n != 0 {
		t.Fatal("re-adding should add nothing")
	}
	// Blob versions are ledger rows without content: they need no snapshot.
	put(t, a, "p.png", bin+"1")
	put(t, a, "p.png", bin+"22")
	transfer(t, a, b, "p.png")
	here, _ = b.MissingVersions("p.png", a.Get("p.png").History)
	if n, _ := b.AddVersions("p.png", here); n != 1 || len(b.Get("p.png").History) != 2 {
		t.Fatalf("blob rows added=%d", n)
	}
}

func TestAddVersionsKeepsCurrentLast(t *testing.T) {
	// A pulled older version with a later mtime (clock skew) must not
	// displace the current version from the end of the ledger.
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("cur"))
	transfer(t, a, b, "n.md")
	skewed := Version{Hash: hashOf([]byte("old")), ModTime: time.Now().Add(time.Hour), ModBy: "x", Clock: clock.Clock{"x": 1}}
	b.PutSnapshot("n.md", skewed.Hash, []byte("old"))
	if n, _ := b.AddVersions("n.md", []Version{skewed}); n != 1 {
		t.Fatal("should add")
	}
	h := b.Get("n.md").History
	if h[len(h)-1].Hash != b.Get("n.md").Hash {
		t.Fatalf("current not last: %+v", h)
	}
}

// ---- blobs and stubs --------------------------------------------------------

func TestStubFillAndDrop(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "big.zip", bin+"zip")
	// Metadata only: b learns about the file but has no content.
	if r := send(t, a, b, "big.zip", false); r != Accepted {
		t.Fatal(r)
	}
	m := b.Get("big.zip")
	if m == nil || m.Have || !m.Blob || m.Size != int64(len(bin)+3) || m.Hash != a.Get("big.zip").Hash {
		t.Fatalf("stub=%+v", m)
	}
	if _, err := b.OpenFile("big.zip"); err == nil {
		t.Fatal("stub should not open")
	}
	// A stub is not a deletion on scan.
	if ch, _ := b.Scan(); len(ch) != 0 {
		t.Fatalf("changed=%v", ch)
	}
	// Same version with content fills the stub.
	if r := send(t, a, b, "big.zip", true); r != Accepted {
		t.Fatal(r)
	}
	if got := mustRead(t, b, "big.zip"); got != bin+"zip" || !b.Get("big.zip").Have {
		t.Fatalf("content=%q", got)
	}
	if r := send(t, a, b, "big.zip", true); r != Unchanged {
		t.Fatal(r)
	}
	// Drop keeps the entry, frees the content, records nothing.
	if err := b.Drop("big.zip"); err != nil {
		t.Fatal(err)
	}
	if m.Have || m.Clock.String() != "a:1" {
		t.Fatalf("after drop=%+v", m)
	}
	if ch, _ := b.Scan(); len(ch) != 0 {
		t.Fatalf("changed=%v", ch)
	}
	// Fill brings it back; wrong bytes are refused and leave nothing behind.
	if err := b.Fill("big.zip", strings.NewReader("wrong")); err == nil {
		t.Fatal("expected hash mismatch")
	}
	if _, err := os.Stat(b.LocalPath("big.zip")); err == nil || m.Have {
		t.Fatal("a failed fill must leave the stub as it was")
	}
	if err := b.Fill("big.zip", strings.NewReader(bin+"zip")); err != nil || !m.Have {
		t.Fatal("not filled", err)
	}
	if err := b.Fill("x", strings.NewReader("wrong")); err == nil {
		t.Fatal("fill of unknown file should fail")
	}
	// Text is always held: it cannot be dropped.
	b.Write("n.md", []byte("text"))
	if err := b.Drop("n.md"); err == nil {
		t.Fatal("dropping text should fail")
	}
}

func TestMetadataNeverReplacesContent(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "doc.pdf", bin+"v1")
	send(t, a, b, "doc.pdf", true)
	put(t, a, "doc.pdf", bin+"v2 longer")
	if r := send(t, a, b, "doc.pdf", false); r != Rejected {
		t.Fatal(r)
	}
	if m := b.Get("doc.pdf"); !m.Have || m.Clock.String() != "a:1" {
		t.Fatalf("b=%+v", m)
	}
	if r := send(t, a, b, "doc.pdf", true); r != Accepted {
		t.Fatal(r)
	}
	if m := b.Get("doc.pdf"); !m.Have || m.Size != int64(len(bin)+9) || len(m.History) != 2 {
		t.Fatalf("b=%+v", m)
	}
}

func TestDeleteStubPropagates(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "old.mov", bin+"movie")
	send(t, a, b, "old.mov", false)
	if err := b.DeleteBy("old.mov", "phone"); err != nil { // delete a stub
		t.Fatal(err)
	}
	if m := b.Get("old.mov"); !m.Deleted || m.ModBy != "phone" {
		t.Fatalf("b=%+v", m)
	}
	if r := send(t, b, a, "old.mov", false); r != Accepted {
		t.Fatal(r)
	}
	if _, err := os.Stat(a.LocalPath("old.mov")); err == nil {
		t.Fatal("content should be gone on a")
	}
}

func TestBlobConflictKeepsBoth(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "p.jpg", bin+"base")
	send(t, a, b, "p.jpg", true)
	put(t, a, "p.jpg", bin+"from a")
	time.Sleep(10 * time.Millisecond)
	put(t, b, "p.jpg", bin+"from b") // newer, wins
	if r := send(t, b, a, "p.jpg", true); r != Conflicted {
		t.Fatal(r)
	}
	files := a.List(false)
	if len(files) != 2 {
		t.Fatalf("files=%v", files)
	}
	if got := mustRead(t, a, "p.jpg"); got != bin+"from b" {
		t.Fatalf("winner=%q", got)
	}
	var loser string
	for _, m := range files {
		if m.Name != "p.jpg" {
			loser = m.Name
		}
	}
	if !strings.HasPrefix(loser, "p.conflict-a-") || !strings.HasSuffix(loser, ".jpg") {
		t.Fatalf("loser=%q", loser)
	}
	if got := mustRead(t, a, loser); got != bin+"from a" {
		t.Fatalf("loser content=%q", got)
	}
	if clock.Compare(a.Get("p.jpg").Clock, b.Get("p.jpg").Clock) != clock.Dominates {
		t.Fatal("merged clock should dominate")
	}
	// Local wins, remote content arrives: remote is kept as the conflict copy.
	c := open(t, "c")
	put(t, c, "q.jpg", bin+"base")
	send(t, c, a, "q.jpg", true)
	put(t, c, "q.jpg", bin+"from c")
	time.Sleep(10 * time.Millisecond)
	put(t, a, "q.jpg", bin+"from a")
	if r := send(t, c, a, "q.jpg", true); r != Conflicted {
		t.Fatal(r)
	}
	n := 0
	for _, m := range a.List(false) {
		if strings.HasPrefix(m.Name, "q.conflict-c-") {
			n++
		}
	}
	if n != 1 || mustRead(t, a, "q.jpg") != bin+"from a" {
		t.Fatal("expected c's copy as a conflict file and a's content in place")
	}
}

func TestBlobConcurrentIdenticalContent(t *testing.T) {
	// The same bytes reached a and b by different routes (e.g. a copied the
	// file in by hand while b pulled it): clocks concurrent, hashes equal.
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "same.png", bin)
	put(t, b, "same.png", bin)
	if r := send(t, a, b, "same.png", true); r != Unchanged {
		t.Fatal(r)
	}
	if len(b.List(false)) != 1 {
		t.Fatal("no conflict copy expected")
	}
	if r := send(t, b, a, "same.png", true); r != Accepted { // b's merged clock dominates
		t.Fatal(r)
	}
	if clock.Compare(a.Get("same.png").Clock, b.Get("same.png").Clock) != clock.Equal {
		t.Fatal("clocks should converge")
	}
}

func TestFileSize(t *testing.T) {
	if FileSize(512) != "512 B" || FileSize(2048) != "2 KB" || FileSize(3<<20) != "3.0 MB" {
		t.Fatal(FileSize(512), FileSize(2048), FileSize(3<<20))
	}
}

func TestSearch(t *testing.T) {
	s := open(t, "a")
	s.Write("shop.md", []byte("buy Milk\nand eggs\nmore milk\n"))
	s.Write("work/milk-run.md", []byte("nothing here\n"))
	put(t, s, "cfg.json", `{"drink":"MILK"}`)
	put(t, s, "milk.png", bin+"milk in binary is not searched")
	put(t, s, "other.md", "tea")
	hits, cut := s.Search("milk", 0, 0)
	if cut || len(hits) != 4 {
		t.Fatalf("hits=%+v", hits)
	}
	by := map[string]Hit{}
	for _, h := range hits {
		by[h.Name] = h
	}
	if h := by["shop.md"]; h.NameMatch || len(h.Lines) != 2 || h.Lines[1].N != 3 || h.Lines[0].Text != "buy Milk" {
		t.Fatalf("shop=%+v", h)
	}
	if h := by["milk.png"]; !h.NameMatch || len(h.Lines) != 0 {
		t.Fatalf("png=%+v", h)
	}
	if h := by["cfg.json"]; len(h.Lines) != 1 {
		t.Fatalf("cfg=%+v", h)
	}
	hits, cut = s.Search("milk", 2, 1)
	if !cut || len(hits) != 2 {
		t.Fatalf("limited hits=%+v cut=%v", hits, cut)
	}
	if h, _ := s.Search("milk", 0, 1); h[2].Name != "shop.md" || len(h[2].Lines) != 1 || h[2].More != 1 {
		t.Fatalf("line cap=%+v", h)
	}
	if h, _ := s.Search("  ", 0, 0); h != nil {
		t.Fatal("blank query finds nothing")
	}
}

// ---- rename -----------------------------------------------------------------

func TestRenameKeepsHistoryAndIdentity(t *testing.T) {
	a := open(t, "a")
	a.Write("old.md", []byte("v1"))
	time.Sleep(2 * time.Millisecond)
	a.Write("old.md", []byte("v2"))
	if err := a.Rename("old.md", "dir/new.md", "phone"); err != nil {
		t.Fatal(err)
	}
	nm := a.Get("dir/new.md")
	if nm == nil || nm.Hash != hashOf([]byte("v2")) || nm.ModBy != "phone" || nm.RenamedFrom != "old.md" || !nm.Have || nm.Size != 2 {
		t.Fatalf("new=%+v", nm)
	}
	if nm.Clock.String() != "a:3" {
		t.Fatalf("clock=%s", nm.Clock)
	}
	if len(nm.History) != 3 || nm.History[2].RenamedFrom != "old.md" || nm.History[0].Seq != 1 {
		t.Fatalf("history=%+v", nm.History)
	}
	if got, _ := a.Snapshot("dir/new.md", nm.History[0].Hash); string(got) != "v1" {
		t.Fatalf("old snapshot not carried: %q", got)
	}
	if got := mustRead(t, a, "dir/new.md"); got != "v2" {
		t.Fatalf("content=%q", got)
	}
	old := a.Get("old.md")
	if !old.Deleted || old.RenamedTo != "dir/new.md" || old.Clock.String() != "a:3" || len(old.History) != 3 || old.History[2].RenamedTo != "dir/new.md" {
		t.Fatalf("old=%+v", old)
	}
	if _, err := os.Stat(a.LocalPath("old.md")); err == nil {
		t.Fatal("old working file should be gone")
	}
	if got, _ := a.Snapshot("old.md", old.History[0].Hash); string(got) != "v1" {
		t.Fatal("old ledger should stay browsable")
	}
	// Scan sees nothing to do.
	if ch, _ := a.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
	// Errors.
	if err := a.Rename("old.md", "x.md", ""); err == nil {
		t.Fatal("renaming a tombstone should fail")
	}
	a.Write("other.md", []byte("x"))
	if err := a.Rename("dir/new.md", "other.md", ""); err == nil {
		t.Fatal("renaming onto an existing file should fail")
	}
	if err := a.Rename("dir/new.md", "dir/new.md", ""); err == nil {
		t.Fatal("same name should fail")
	}
}

func TestRenamePropagates(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n.md", []byte("v1"))
	transfer(t, a, b, "n.md")
	a.Write("n.md", []byte("v2"))
	if err := a.Rename("n.md", "m.md", ""); err != nil {
		t.Fatal(err)
	}
	if r := transfer(t, a, b, "m.md"); r != Accepted {
		t.Fatal(r)
	}
	if r := transfer(t, a, b, "n.md"); r != Accepted {
		t.Fatal(r)
	}
	if got := mustRead(t, b, "m.md"); got != "v2" {
		t.Fatalf("b m=%q", got)
	}
	if m := b.Get("m.md"); m.RenamedFrom != "n.md" || m.History[len(m.History)-1].RenamedFrom != "n.md" {
		t.Fatalf("b m=%+v", m)
	}
	if o := b.Get("n.md"); !o.Deleted || o.RenamedTo != "m.md" {
		t.Fatalf("b n=%+v", o)
	}
	if _, err := b.Read("n.md"); err == nil {
		t.Fatal("b should not have n any more")
	}
	// Reviving the old name as a fresh file works and dominates the tombstone.
	b.Write("n.md", []byte("fresh"))
	if clock.Compare(b.Get("n.md").Clock, a.Get("n.md").Clock) != clock.Dominates {
		t.Fatal("fresh n should dominate the tombstone")
	}
}

func TestRenameCarriesContentOnPeer(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "big.mov", bin+"movie bytes")
	send(t, a, b, "big.mov", true) // b holds it
	if err := a.Rename("big.mov", "trips/big.mov", "phone"); err != nil {
		t.Fatal(err)
	}
	nm := a.Get("trips/big.mov")
	if nm == nil || !nm.Have || !nm.Blob || nm.RenamedFrom != "big.mov" || nm.ModBy != "phone" || nm.Clock.String() != "a:2" {
		t.Fatalf("new=%+v", nm)
	}
	if o := a.Get("big.mov"); !o.Deleted || o.RenamedTo != "trips/big.mov" || o.Have {
		t.Fatalf("old=%+v", o)
	}
	if ch, _ := a.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
	// b receives metadata only and fulfils it from its own copy.
	if !b.CanCarry(nm.Wire()) {
		t.Fatal("b should be able to carry")
	}
	if r := send(t, a, b, "trips/big.mov", false); r != Accepted {
		t.Fatal(r)
	}
	if m := b.Get("trips/big.mov"); !m.Have || mustRead(t, b, "trips/big.mov") != bin+"movie bytes" {
		t.Fatalf("b should hold the renamed file: %+v", m)
	}
	// The old tombstone arrives afterwards; nothing breaks.
	if r := send(t, a, b, "big.mov", false); r != Accepted {
		t.Fatal(r)
	}
	if ch, _ := b.Scan(); len(ch) != 0 {
		t.Fatalf("b scan changed %v", ch)
	}
	if names := b.List(false); len(names) != 1 || names[0].Name != "trips/big.mov" {
		t.Fatalf("b files=%v", names)
	}
	// A stub renames too, without content.
	c := open(t, "c")
	send(t, a, c, "trips/big.mov", false)
	if err := c.Rename("trips/big.mov", "x.mov", ""); err != nil {
		t.Fatal(err)
	}
	if m := c.Get("x.mov"); m.Have || !strings.HasPrefix(m.Clock.String(), "a:2,c:1") {
		t.Fatalf("c=%+v", m)
	}
}

func TestTextRecordedAsBlobIsPromotedWhereTheBytesAre(t *testing.T) {
	// A version can arrive marked as a blob although it is text: files from
	// before layout 2 are, on every machine. Whoever holds the bytes keeps a
	// snapshot, so the content survives the next edit; clocks do not move.
	a, b := open(t, "a"), open(t, "b")
	a.Write("notes.md", []byte("first"))
	meta := a.Get("notes.md").Wire()
	meta.Blob = true
	// Metadata only: b has a stub and cannot know.
	if r, err := b.Apply(meta, nil); err != nil || r != Accepted {
		t.Fatal(r, err)
	}
	if m := b.Get("notes.md"); !m.Blob || m.Have {
		t.Fatalf("stub=%+v", m)
	}
	// The bytes arrive: promoted, snapshot kept, same clock.
	if err := b.Fill("notes.md", strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	m := b.Get("notes.md")
	if m.Blob || m.History[0].Blob || m.Clock.String() != "a:1" || !b.HasSnapshot("notes.md", m.Hash) {
		t.Fatalf("after fill=%+v", m)
	}
	b.Write("notes.md", []byte("second"))
	if got, _ := b.Snapshot("notes.md", b.Get("notes.md").History[0].Hash); string(got) != "first" {
		t.Fatalf("first version lost: %q", got)
	}
	// With content in one go.
	c := open(t, "c")
	if r, err := c.Apply(meta, strings.NewReader("first")); err != nil || r != Accepted {
		t.Fatal(r, err)
	}
	if m := c.Get("notes.md"); m.Blob || !c.HasSnapshot("notes.md", m.Hash) {
		t.Fatalf("c=%+v", m)
	}
	// Real binaries stay blobs.
	put(t, a, "p.png", bin)
	transfer(t, a, c, "p.png")
	if m := c.Get("p.png"); !m.Blob || c.HasSnapshot("p.png", m.Hash) {
		t.Fatalf("png=%+v", m)
	}
}
