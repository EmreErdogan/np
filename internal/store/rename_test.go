package store

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

func TestRenameKeepsHistoryAndIdentity(t *testing.T) {
	a := open(t, "a")
	a.Write("old", []byte("v1"))
	time.Sleep(2 * time.Millisecond)
	a.Write("old", []byte("v2"))
	if err := a.Rename("old", "dir/new", "phone"); err != nil {
		t.Fatal(err)
	}
	nm := a.Get("dir/new")
	if nm == nil || nm.Hash != hashOf([]byte("v2")) || nm.ModBy != "phone" || nm.RenamedFrom != "old" {
		t.Fatalf("new=%+v", nm)
	}
	if nm.Clock.String() != "a:3" {
		t.Fatalf("clock=%s", nm.Clock)
	}
	if len(nm.History) != 3 || nm.History[2].RenamedFrom != "old" || nm.History[0].Seq != 1 {
		t.Fatalf("history=%+v", nm.History)
	}
	if got, _ := a.Snapshot("dir/new", nm.History[0].Hash); string(got) != "v1" {
		t.Fatalf("old snapshot not carried: %q", got)
	}
	if got, _ := a.Read("dir/new"); string(got) != "v2" {
		t.Fatalf("content=%q", got)
	}
	old := a.Get("old")
	if !old.Deleted || old.RenamedTo != "dir/new" || old.Clock.String() != "a:3" || len(old.History) != 3 || old.History[2].RenamedTo != "dir/new" {
		t.Fatalf("old=%+v", old)
	}
	if _, err := os.Stat(a.notePath("old")); err == nil {
		t.Fatal("old working file should be gone")
	}
	if got, _ := a.Snapshot("old", old.History[0].Hash); string(got) != "v1" {
		t.Fatal("old ledger should stay browsable")
	}
	// Scan sees nothing to do.
	if ch, _ := a.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
	// Errors.
	if err := a.Rename("old", "x", ""); err == nil {
		t.Fatal("renaming a tombstone should fail")
	}
	a.Write("other", []byte("x"))
	if err := a.Rename("dir/new", "other", ""); err == nil {
		t.Fatal("renaming onto an existing note should fail")
	}
	if err := a.Rename("dir/new", "dir/new", ""); err == nil {
		t.Fatal("same name should fail")
	}
}

func TestRenamePropagates(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.Write("n", []byte("v1"))
	transfer(t, a, b, "n")
	a.Write("n", []byte("v2"))
	if err := a.Rename("n", "m", ""); err != nil {
		t.Fatal(err)
	}
	if r := transfer(t, a, b, "m"); r != Accepted {
		t.Fatal(r)
	}
	if r := transfer(t, a, b, "n"); r != Accepted {
		t.Fatal(r)
	}
	if got, _ := b.Read("m"); string(got) != "v2" {
		t.Fatalf("b m=%q", got)
	}
	if m := b.Get("m"); m.RenamedFrom != "n" || m.History[len(m.History)-1].RenamedFrom != "n" {
		t.Fatalf("b m=%+v", m)
	}
	if o := b.Get("n"); !o.Deleted || o.RenamedTo != "m" {
		t.Fatalf("b n=%+v", o)
	}
	if _, err := b.Read("n"); err == nil {
		t.Fatal("b should not have n any more")
	}
	// Reviving the old name as a fresh note works and dominates the tombstone.
	b.Write("n", []byte("fresh"))
	if clock.Compare(b.Get("n").Clock, a.Get("n").Clock) != clock.Dominates {
		t.Fatal("fresh n should dominate the tombstone")
	}
}

func TestRenameFileCarriesContentOnPeer(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	put(t, a, "big.mov", "movie bytes")
	send(t, a, b, "big.mov", true) // b holds it
	if err := a.RenameFile("big.mov", "trips/big.mov", "phone"); err != nil {
		t.Fatal(err)
	}
	nm := a.File("trips/big.mov")
	if nm == nil || !nm.Have || nm.RenamedFrom != "big.mov" || nm.ModBy != "phone" || nm.Clock.String() != "a:2" {
		t.Fatalf("new=%+v", nm)
	}
	if o := a.File("big.mov"); !o.Deleted || o.RenamedTo != "trips/big.mov" || o.Have {
		t.Fatalf("old=%+v", o)
	}
	if ch, _ := a.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
	// b receives metadata only and fulfils it from its own copy.
	if !b.CanCarry(*nm) {
		t.Fatal("b should be able to carry")
	}
	if r := send(t, a, b, "trips/big.mov", false); r != Accepted {
		t.Fatal(r)
	}
	if m := b.File("trips/big.mov"); !m.Have {
		t.Fatalf("b should hold the renamed file: %+v", m)
	}
	f, _ := b.OpenFile("trips/big.mov")
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	f.Close()
	if string(buf[:n]) != "movie bytes" {
		t.Fatalf("content=%q", buf[:n])
	}
	// The old tombstone arrives afterwards; nothing breaks.
	if r := send(t, a, b, "big.mov", false); r != Accepted {
		t.Fatal(r)
	}
	if ch, _ := b.Scan(); len(ch) != 0 {
		t.Fatalf("b scan changed %v", ch)
	}
	if names := b.Files(false); len(names) != 1 || names[0].Name != "trips/big.mov" {
		t.Fatalf("b files=%v", names)
	}
	// A stub renames too, without content.
	c := open(t, "c")
	send(t, a, c, "trips/big.mov", false)
	if err := c.RenameFile("trips/big.mov", "x.mov", ""); err != nil {
		t.Fatal(err)
	}
	if m := c.File("x.mov"); m.Have || !strings.HasPrefix(m.Clock.String(), "a:2,c:1") {
		t.Fatalf("c=%+v", m)
	}
}
