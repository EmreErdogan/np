package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

func put(t *testing.T, s *Store, name, content string) {
	t.Helper()
	if err := s.PutFile(name, strings.NewReader(content), s.Node); err != nil {
		t.Fatal(err)
	}
}

// send applies from's file to to, with content when withContent is set.
func send(t *testing.T, from, to *Store, name string, withContent bool) ApplyResult {
	t.Helper()
	m := from.File(name)
	var src *os.File
	if withContent && m.Have && !m.Deleted {
		f, err := from.OpenFile(name)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		src = f
	}
	var r ApplyResult
	var err error
	if src != nil {
		r, err = to.ApplyFile(*m, src)
	} else {
		r, err = to.ApplyFile(*m, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFileScanAndHashCache(t *testing.T) {
	s := open(t, "a")
	p, _ := s.FilePath("pics/cat.png")
	os.WriteFile(p, []byte("PNG\x00data"), 0o644)
	ch, _ := s.Scan()
	if len(ch) != 1 || ch[0] != "files/pics/cat.png" {
		t.Fatalf("changed=%v", ch)
	}
	m := s.File("pics/cat.png")
	if m == nil || !m.Have || m.Size != 8 || m.Clock.String() != "a:1" {
		t.Fatalf("meta=%+v", m)
	}
	// Unchanged file: no new version.
	if ch, _ := s.Scan(); len(ch) != 0 {
		t.Fatalf("changed=%v", ch)
	}
	// Edited file: new version.
	os.WriteFile(p, []byte("PNG\x00data2"), 0o644)
	os.Chtimes(p, time.Now().Add(time.Second), time.Now().Add(time.Second))
	s.Scan()
	if m.Clock.String() != "a:2" || m.Size != 9 {
		t.Fatalf("meta=%+v", m)
	}
	// Removed file: tombstone.
	os.Remove(p)
	s.Scan()
	if !m.Deleted || m.Have || m.Clock.String() != "a:3" {
		t.Fatalf("meta=%+v", m)
	}
	// Temp and dot files are ignored.
	os.WriteFile(filepath.Join(s.Dir, "files", "x.tmp"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(s.Dir, "files", ".hidden"), []byte("x"), 0o644)
	if ch, _ := s.Scan(); len(ch) != 0 {
		t.Fatalf("changed=%v", ch)
	}
}

func TestFileTransferAndStub(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "big.zip", "zip contents")
	// Metadata only: b learns about the file but has no content.
	if r := send(t, a, b, "big.zip", false); r != Accepted {
		t.Fatal(r)
	}
	m := b.File("big.zip")
	if m == nil || m.Have || m.Size != 12 || m.Hash != a.File("big.zip").Hash {
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
	f, err := b.OpenFile("big.zip")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(f)
	f.Close()
	if buf.String() != "zip contents" || !b.File("big.zip").Have {
		t.Fatalf("content=%q", buf.String())
	}
	if r := send(t, a, b, "big.zip", true); r != Unchanged {
		t.Fatal(r)
	}
	// Drop keeps the entry, frees the content, records nothing.
	if err := b.DropFile("big.zip"); err != nil {
		t.Fatal(err)
	}
	if m.Have || m.Clock.String() != "a:1" {
		t.Fatalf("after drop=%+v", m)
	}
	if ch, _ := b.Scan(); len(ch) != 0 {
		t.Fatalf("changed=%v", ch)
	}
	// FillFile brings it back.
	if err := b.FillFile("big.zip", strings.NewReader("zip contents")); err != nil {
		t.Fatal(err)
	}
	if !m.Have {
		t.Fatal("not filled")
	}
	if err := b.FillFile("x", strings.NewReader("wrong")); err == nil {
		t.Fatal("fill of unknown file should fail")
	}
}

func TestFileFillRejectsWrongContent(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "f.bin", "right")
	send(t, a, b, "f.bin", false)
	if err := b.FillFile("f.bin", strings.NewReader("wrong")); err == nil {
		t.Fatal("expected hash mismatch")
	}
	if b.File("f.bin").Have {
		t.Fatal("should still be a stub")
	}
	if _, err := os.Stat(b.filePath("f.bin")); err == nil {
		t.Fatal("no file should be left behind")
	}
}

func TestFileMetadataNeverReplacesContent(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "doc.pdf", "v1")
	send(t, a, b, "doc.pdf", true)
	put(t, a, "doc.pdf", "v2 longer")
	if r := send(t, a, b, "doc.pdf", false); r != Rejected {
		t.Fatal(r)
	}
	if m := b.File("doc.pdf"); !m.Have || m.Clock.String() != "a:1" {
		t.Fatalf("b=%+v", m)
	}
	if r := send(t, a, b, "doc.pdf", true); r != Accepted {
		t.Fatal(r)
	}
	if m := b.File("doc.pdf"); !m.Have || m.Size != 9 {
		t.Fatalf("b=%+v", m)
	}
}

func TestFileDeleteAndTombstone(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "old.mov", "movie")
	send(t, a, b, "old.mov", false)
	if err := b.DeleteFile("old.mov", "phone"); err != nil { // delete a stub
		t.Fatal(err)
	}
	if m := b.File("old.mov"); !m.Deleted || m.ModBy != "phone" {
		t.Fatalf("b=%+v", m)
	}
	if r := send(t, b, a, "old.mov", false); r != Accepted {
		t.Fatal(r)
	}
	if _, err := os.Stat(a.filePath("old.mov")); err == nil {
		t.Fatal("content should be gone on a")
	}
	if len(a.Files(false)) != 0 || len(a.Files(true)) != 1 {
		t.Fatal("tombstone expected")
	}
}

func TestFileConflictKeepsBoth(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "p.jpg", "base")
	send(t, a, b, "p.jpg", true)
	put(t, a, "p.jpg", "from a")
	time.Sleep(10 * time.Millisecond)
	put(t, b, "p.jpg", "from b") // newer, wins
	if r := send(t, b, a, "p.jpg", true); r != Conflicted {
		t.Fatal(r)
	}
	files := a.Files(false)
	if len(files) != 2 {
		t.Fatalf("files=%v", files)
	}
	f, _ := a.OpenFile("p.jpg")
	var buf bytes.Buffer
	buf.ReadFrom(f)
	f.Close()
	if buf.String() != "from b" {
		t.Fatalf("winner=%q", buf.String())
	}
	var loser string
	for _, m := range files {
		if m.Name != "p.jpg" {
			loser = m.Name
		}
	}
	if !strings.HasPrefix(loser, "p.jpg.conflict-a-") {
		t.Fatalf("loser=%q", loser)
	}
	lf, _ := a.OpenFile(loser)
	buf.Reset()
	buf.ReadFrom(lf)
	lf.Close()
	if buf.String() != "from a" {
		t.Fatalf("loser content=%q", buf.String())
	}
	if clock.Compare(a.File("p.jpg").Clock, b.File("p.jpg").Clock) != clock.Dominates {
		t.Fatal("merged clock should dominate")
	}
	// Local wins, remote content arrives: remote is kept as the conflict copy.
	c := open(t, "c")
	put(t, c, "q.jpg", "base")
	send(t, c, a, "q.jpg", true)
	put(t, c, "q.jpg", "from c")
	time.Sleep(10 * time.Millisecond)
	put(t, a, "q.jpg", "from a")
	if r := send(t, c, a, "q.jpg", true); r != Conflicted {
		t.Fatal(r)
	}
	n := 0
	for _, m := range a.Files(false) {
		if strings.HasPrefix(m.Name, "q.jpg.conflict-c-") {
			n++
		}
	}
	if n != 1 {
		t.Fatal("expected c's copy as a conflict file")
	}
}

func TestValidFileName(t *testing.T) {
	for _, ok := range []string{"a.png", "dir/b.mp4", "README", "notes.md"} {
		if err := ValidFileName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/../b", ".hidden", "x.tmp", "/abs"} {
		if err := ValidFileName(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestFileSize(t *testing.T) {
	if FileSize(512) != "512 B" || FileSize(2048) != "2 KB" || FileSize(3<<20) != "3.0 MB" {
		t.Fatal(FileSize(512), FileSize(2048), FileSize(3<<20))
	}
}
