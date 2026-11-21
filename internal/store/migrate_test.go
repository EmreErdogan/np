package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

// legacy builds a layout-1 repository in dir: notes under notes/ with one
// snapshot per version, files under files/, and the old two-map index.
type legacy struct {
	t     *testing.T
	dir   string
	at    time.Time
	Notes map[string]*Meta `json:"notes"`
	Files map[string]*Meta `json:"files"`
}

func newLegacy(t *testing.T) *legacy {
	return &legacy{t: t, dir: t.TempDir(), at: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
		Notes: map[string]*Meta{}, Files: map[string]*Meta{}}
}

func (l *legacy) write(rel, content string) {
	l.t.Helper()
	p := filepath.Join(l.dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// note records versions of a note as node "desk"; an empty last version is a deletion.
func (l *legacy) note(name string, versions ...string) {
	m := &Meta{Name: name, Clock: clock.Clock{}}
	for i, v := range versions {
		l.at = l.at.Add(time.Minute)
		m.Clock = m.Clock.Bump("desk")
		m.ModTime, m.ModBy = l.at, "desk"
		m.Deleted = v == "" && i == len(versions)-1
		m.Hash = ""
		if !m.Deleted {
			m.Hash = hashOf([]byte(v))
			l.write("history/"+name+"/"+m.Hash+".md", v)
		}
		m.History = append(m.History, Version{Seq: i + 1, Hash: m.Hash, ModTime: m.ModTime, ModBy: "desk", Clock: m.Clock.Copy(), Deleted: m.Deleted})
	}
	if !m.Deleted {
		l.write("notes/"+DefaultName(name), versions[len(versions)-1])
	}
	l.Notes[name] = m
}

// file records a file at clock lap:n; have=false is a stub, content "" a tombstone.
func (l *legacy) file(name, content string, n int, have bool) {
	l.at = l.at.Add(time.Minute)
	m := &Meta{Name: name, Clock: clock.Clock{"lap": n}, ModTime: l.at, ModBy: "lap"}
	if content == "" {
		m.Deleted = true
	} else {
		m.Hash, m.Size, m.Have = hashOf([]byte(content)), int64(len(content)), have
		if have {
			l.write("files/"+name, content)
		}
	}
	l.Files[name] = m
}

func (l *legacy) open(node string) *Store {
	l.t.Helper()
	// The old index wrote no "have" for notes and no "layout" at all.
	raw, _ := json.Marshal(map[string]any{"node": node, "notes": l.Notes, "files": l.Files})
	l.write("index.json", string(raw))
	s, err := Open(l.dir, node)
	if err != nil {
		l.t.Fatal(err)
	}
	return s
}

func buildLegacy(t *testing.T) *legacy {
	l := newLegacy(t)
	l.note("todo", "v1", "v2")
	l.note("work/plan", "plan")
	l.note("cfg.json", "{}")
	l.note("gone", "x", "")
	l.file("pic.png", bin+"png", 1, true)
	l.file("big.bin", bin+"big", 2, false)
	l.file("old.txt", "", 3, false)
	// Collisions.
	l.note("plan", "the note")
	l.file("plan.md", "the file", 68, true)
	l.note("same", "identical")
	l.file("same.md", "identical", 4, true)
	l.note("rev", "revived")
	l.file("rev.md", "", 5, false)
	l.note("dead", "was here", "")
	l.file("dead.md", "file lives", 6, true)
	return l
}

func TestMigrateLayout1(t *testing.T) {
	l := buildLegacy(t)
	s := l.open("desk")

	if len(s.Notices) != 2 || !strings.Contains(s.Notices[1], "plan.note.md") {
		t.Fatalf("notices=%q", s.Notices)
	}
	if _, err := os.Stat(filepath.Join(l.dir, "index.v1.json")); err != nil {
		t.Fatal("old index should be kept")
	}
	if _, err := os.Stat(filepath.Join(l.dir, "notes")); err == nil {
		t.Fatal("notes/ should be gone")
	}
	// Nothing was deleted: every original note file is still there, set aside.
	for rel, want := range map[string]string{"todo.md": "v2", "plan.md": "the note", "work/plan.md": "plan", "same.md": "identical"} {
		if got, err := os.ReadFile(filepath.Join(l.dir, "notes.v1", filepath.FromSlash(rel))); err != nil || string(got) != want {
			t.Fatalf("notes.v1/%s: %q %v", rel, got, err)
		}
	}
	// Nothing is a local change after the migration: no clock moves.
	if ch, err := s.Scan(); err != nil || len(ch) != 0 {
		t.Fatalf("scan changed %v err=%v", ch, err)
	}

	type want struct {
		clock   string
		content string // "" = not held or deleted
		deleted bool
		blob    bool
		rows    int
	}
	wants := map[string]want{
		"todo.md":      {"desk:2", "v2", false, false, 2},
		"work/plan.md": {"desk:1", "plan", false, false, 1},
		"cfg.json":     {"desk:1", "{}", false, false, 1},
		"gone.md":      {"desk:2", "", true, false, 2},
		"pic.png":      {"lap:1", bin + "png", false, true, 1},
		"big.bin":      {"lap:2", "", false, true, 1},
		"old.txt":      {"lap:3", "", true, false, 1},
		"plan.md":      {"lap:68", "the file", false, false, 1},
		"plan.note.md": {"desk:1", "the note", false, false, 1},
		"same.md":      {"desk:1,lap:4", "identical", false, false, 2},
		"rev.md":       {"desk:1,lap:5", "revived", false, false, 2},
		"dead.md":      {"desk:2,lap:6", "file lives", false, false, 3},
	}
	if got := len(s.List(true)); got != len(wants) {
		t.Fatalf("%d entries, want %d", got, len(wants))
	}
	for name, w := range wants {
		m := s.Get(name)
		if m == nil {
			t.Fatalf("%s missing", name)
		}
		if m.Name != name || m.Clock.String() != w.clock || m.Deleted != w.deleted || m.Blob != w.blob || len(m.History) != w.rows {
			t.Errorf("%s: %+v", name, m)
			continue
		}
		last := m.History[len(m.History)-1]
		if last.Clock.String() != w.clock || last.Hash != m.Hash || last.Seq != len(m.History) {
			t.Errorf("%s: current row %+v", name, last)
		}
		if w.content == "" {
			if m.Have {
				t.Errorf("%s should not be held", name)
			}
			continue
		}
		if got := mustRead(t, s, name); got != w.content || m.Size != int64(len(w.content)) {
			t.Errorf("%s: content %q size %d", name, got, m.Size)
		}
	}
	// Text that was a history-less file gets a snapshot of what it holds
	// right now, on the first scan, so its next edit cannot lose it.
	for _, name := range []string{"plan.md", "dead.md"} {
		m := s.Get(name)
		if got, err := s.Snapshot(name, m.Hash); err != nil || string(got) != mustRead(t, s, name) || m.History[len(m.History)-1].Blob {
			t.Fatalf("%s: current content has no snapshot: %q %v", name, got, err)
		}
	}
	if s.HasSnapshot("pic.png", s.Get("pic.png").Hash) {
		t.Fatal("a binary file gets no snapshot")
	}
	// History moved with the notes, under the full name.
	todo := s.Get("todo.md")
	if got, err := s.Snapshot("todo.md", todo.History[0].Hash); err != nil || string(got) != "v1" {
		t.Fatalf("todo v1: %q %v", got, err)
	}
	if got, _ := s.Snapshot("plan.note.md", s.Get("plan.note.md").Hash); string(got) != "the note" {
		t.Fatalf("renamed note snapshot: %q", got)
	}
	if got, _ := s.Snapshot("dead.md", s.Get("dead.md").History[0].Hash); string(got) != "was here" {
		t.Fatalf("absorbed tombstone history: %q", got)
	}
	if _, err := os.Stat(filepath.Join(l.dir, "history", "todo")); err == nil {
		t.Fatal("old history dir should be gone")
	}

	// An older binary must fail to read the new index rather than misread it.
	raw, _ := os.ReadFile(filepath.Join(l.dir, "index.json"))
	var v1 struct {
		Notes map[string]*Meta `json:"notes"`
	}
	if err := json.Unmarshal(raw, &v1); err == nil {
		t.Fatal("layout 2 index must not parse as layout 1")
	}

	// Reopening does not migrate again; text that was a file edits like any other.
	s2, err := Open(l.dir, "desk")
	if err != nil || len(s2.Notices) != 0 {
		t.Fatalf("reopen: %v %q", err, s2.Notices)
	}
	if d, ok := s2.ReadText("plan.md"); !ok || string(d) != "the file" {
		t.Fatal("a text file that was synced as a blob should still read as text")
	}
	if err := s2.Write("plan.md", []byte("the file, edited")); err != nil {
		t.Fatal(err)
	}
	if m := s2.Get("plan.md"); m.Blob || len(m.History) != 2 || !s2.HasSnapshot("plan.md", m.Hash) || m.Clock.String() != "desk:1,lap:68" {
		t.Fatalf("after first edit: %+v", m)
	}
	if got, _ := s2.Snapshot("plan.md", s2.Get("plan.md").History[0].Hash); string(got) != "the file" {
		t.Fatalf("the content from before the first edit must stay readable: %q", got)
	}
}

func TestMigrateIsTheSameOnEveryNode(t *testing.T) {
	// Two machines that were in sync migrate on their own and must agree on
	// every name, clock and ledger without talking to each other.
	a, b := buildLegacy(t).open("desk"), buildLegacy(t).open("lap")
	// The first scan gives text that had no history its snapshot; both
	// machines hold the same bytes, so they still agree afterwards.
	for _, s := range []*Store{a, b} {
		if ch, err := s.Scan(); err != nil || len(ch) != 0 {
			t.Fatalf("scan changed %v err=%v", ch, err)
		}
	}
	la, lb := a.List(true), b.List(true)
	if len(la) != len(lb) {
		t.Fatalf("%d vs %d entries", len(la), len(lb))
	}
	for i := range la {
		wa, wb := la[i].Wire(), lb[i].Wire()
		if !reflect.DeepEqual(wa, wb) {
			t.Errorf("%s differs:\n %+v\n %+v", wa.Name, wa, wb)
		}
		if HistoryDigest(la[i]) != HistoryDigest(lb[i]) {
			t.Errorf("%s: ledgers differ", la[i].Name)
		}
		if la[i].Deleted || !la[i].Have {
			continue
		}
		f, err := a.OpenFile(la[i].Name)
		if err != nil {
			t.Fatal(err)
		}
		r, err := b.Apply(la[i].Wire(), f)
		f.Close()
		if err != nil || r != Unchanged {
			t.Errorf("%s: %s %v", la[i].Name, r, err)
		}
	}
}

func TestMigrateCanRunAgainAfterInterruption(t *testing.T) {
	l := buildLegacy(t)
	first := l.open("desk")
	names := len(first.List(true))
	// Put the old index back, as if the run had died before writing the new one.
	raw, _ := os.ReadFile(filepath.Join(l.dir, "index.v1.json"))
	os.WriteFile(filepath.Join(l.dir, "index.json"), raw, 0o644)
	s, err := Open(l.dir, "desk")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List(true)) != names {
		t.Fatalf("%d entries, want %d", len(s.List(true)), names)
	}
	if ch, _ := s.Scan(); len(ch) != 0 {
		t.Fatalf("scan changed %v", ch)
	}
	if mustRead(t, s, "plan.md") != "the file" || mustRead(t, s, "plan.note.md") != "the note" || mustRead(t, s, "todo.md") != "v2" {
		t.Fatal("content moved to the wrong place on the second run")
	}
}

func TestOpenRefusesNewerLayout(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"layout":99}`), 0o644)
	if _, err := Open(dir, "a"); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("err=%v", err)
	}
}

func TestNeedsMigration(t *testing.T) {
	l := buildLegacy(t)
	raw, _ := json.Marshal(map[string]any{"node": "desk", "notes": l.Notes, "files": l.Files})
	l.write("index.json", string(raw))
	if !NeedsMigration(l.dir) {
		t.Fatal("a layout 1 store needs migration")
	}
	if _, err := Open(l.dir, "desk"); err != nil {
		t.Fatal(err)
	}
	if NeedsMigration(l.dir) {
		t.Fatal("already migrated")
	}
	if NeedsMigration(t.TempDir()) {
		t.Fatal("an empty directory has nothing to migrate")
	}
	if c, err := ReadConfig(t.TempDir()); err != nil || c.Port != DefaultPort {
		t.Fatalf("config=%+v err=%v", c, err)
	}
}
