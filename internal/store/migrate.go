package store

// Migration from layout 1 (notes/ and files/ as two namespaces, index keys
// "notes" and "files") to layout 2 (one tree under files/, index key
// "entries"). See .docs/design-unified-namespace.md.
//
// The result depends only on the old index, and no clock is bumped, so
// machines that were in sync before they upgraded are in sync afterwards.
// Every step skips work that is already done: an interrupted migration can
// simply run again.
//
// Nothing is deleted or overwritten: files/ is not touched, notes are
// copied into it, and the old notes directory and index stay behind as
// notes.v1/ and index.v1.json.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

// NeedsMigration reports whether the repository at dir is in an older
// layout, i.e. whether opening it would migrate it.
func NeedsMigration(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return false
	}
	var probe struct {
		Layout int `json:"layout"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.Layout < Layout
}

type legacyIndex struct {
	Notes map[string]*Meta `json:"notes"`
	Files map[string]*Meta `json:"files"`
}

func (s *Store) migrate(raw []byte) error {
	var old legacyIndex
	if err := json.Unmarshal(raw, &old); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	backup := filepath.Join(s.Dir, "index.v1.json")
	if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(backup, raw, 0o644); err != nil {
			return err
		}
	}

	entries := map[string]*Meta{}
	taken := map[string]bool{}
	// Files stay where they are, as blobs with a one-row ledger.
	for name, f := range old.Files {
		if f == nil || ValidName(name) != nil {
			continue
		}
		f.Name, f.Blob, f.Stamp, f.HistoryDigest = name, !f.Deleted, "", ""
		f.History = []Version{{
			Seq: 1, Hash: f.Hash, Size: f.Size, ModTime: f.ModTime, ModBy: f.ModBy, Clock: f.Clock.Copy(),
			Deleted: f.Deleted, Blob: f.Blob, RenamedFrom: f.RenamedFrom, RenamedTo: f.RenamedTo,
		}}
		entries[name] = f
		taken[name] = true
	}

	names := make([]string, 0, len(old.Notes))
	for n := range old.Notes {
		names = append(names, n)
		taken[DefaultName(n)] = true
	}
	sort.Strings(names)
	ref := func(n string) string {
		if n == "" {
			return ""
		}
		return DefaultName(n)
	}
	var notices []string
	moved := 0
	for _, n := range names {
		m := old.Notes[n]
		target := DefaultName(n)
		if m == nil || ValidName(target) != nil {
			continue
		}
		m.Have, m.Blob, m.Stamp, m.HistoryDigest = !m.Deleted, false, "", ""
		m.RenamedFrom, m.RenamedTo = ref(m.RenamedFrom), ref(m.RenamedTo)
		for i := range m.History {
			m.History[i].RenamedFrom, m.History[i].RenamedTo = ref(m.History[i].RenamedFrom), ref(m.History[i].RenamedTo)
		}
		src := filepath.Join(s.Dir, "notes", filepath.FromSlash(target))
		oldHistory := filepath.Join(s.Dir, "history", filepath.FromSlash(n))
		if f := entries[target]; f != nil {
			switch {
			case m.Deleted:
				// The file keeps everything and absorbs the note's past.
				absorb(f, m)
				if err := moveSnapshots(oldHistory, filepath.Dir(s.snapshotPath(target, "x"))); err != nil {
					return err
				}
				continue
			case f.Deleted || f.Hash == m.Hash:
				absorb(m, f)
			default:
				target = freeNoteName(target, taken)
				taken[target] = true
				notices = append(notices, fmt.Sprintf("note %s is now %s (a file named %s already existed and keeps its name)", n, target, DefaultName(n)))
			}
		}
		m.Name = target
		if !m.Deleted {
			if err := copyIn(src, s.filePath(target)); err != nil {
				return err
			}
			if st, err := os.Stat(s.filePath(target)); err == nil {
				m.Size = st.Size()
			}
			moved++
		}
		if err := moveSnapshots(oldHistory, filepath.Dir(s.snapshotPath(target, "x"))); err != nil {
			return err
		}
		entries[target] = m
	}

	s.idx = &Index{Layout: Layout, Node: s.Node, Guard: indexGuard, Entries: entries}
	if err := s.saveIndex(); err != nil {
		return err
	}
	// The originals stay, out of the way of scans, until the user removes them.
	legacy, kept := filepath.Join(s.Dir, "notes"), filepath.Join(s.Dir, "notes.v1")
	removeEmptyDirs(legacy)
	if _, err := os.Stat(legacy); err == nil {
		if _, err := os.Stat(kept); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(legacy, kept); err != nil {
				return err
			}
		}
	}
	s.Notices = append([]string{fmt.Sprintf(
		"store upgraded to layout 2: %d note(s) copied into %s; the originals are kept in notes.v1/ and the previous index as index.v1.json",
		moved, filepath.Join(s.Dir, "files"))}, notices...)
	return nil
}

// absorb folds other into keep when both end up under one name: keep's
// current version stays current, with a clock that dominates both.
func absorb(keep, other *Meta) {
	keep.Clock = clock.Merge(keep.Clock, other.Clock)
	if n := len(keep.History); n > 0 {
		keep.History[n-1].Clock = keep.Clock.Copy()
	}
	keep.History = append(append([]Version(nil), other.History...), keep.History...)
	sortLedger(keep)
}

// freeNoteName picks the name a note gets when a file already owns its
// natural one: "plan.md" -> "plan.note.md", then "plan.note2.md", ...
func freeNoteName(name string, taken map[string]bool) string {
	dir, base := path.Split(name)
	ext := ""
	if loc := extRe.FindStringIndex(base); loc != nil && loc[0] > 0 {
		base, ext = base[:loc[0]], base[loc[0]:]
	}
	for i := 1; ; i++ {
		suffix := ".note"
		if i > 1 {
			suffix += strconv.Itoa(i)
		}
		if cand := dir + base + suffix + ext; !taken[cand] {
			return cand
		}
	}
}

// copyIn copies a note's working file into the files tree, keeping its
// mtime; the original stays where it is. Whatever already sits at the
// destination with other content is kept under a conflict name, which the
// next scan records as a file of its own.
func copyIn(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return nil // an earlier run finished and set the originals aside, or it was never there
	}
	if _, err := os.Stat(dst); err == nil {
		a, errA := hashFile(src)
		b, errB := hashFile(dst)
		if errA == nil && errB == nil && a == b {
			return nil
		}
		aside := filepath.Join(filepath.Dir(dst), conflictName(filepath.Base(dst), "migration", time.Now()))
		if err := os.Rename(dst, aside); err != nil {
			return err
		}
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, _, err := writeFileFrom(dst, f, ""); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// moveSnapshots moves the snapshot files of one history dir into another.
// Sub-directories belong to other entries and stay.
func moveSnapshots(from, to string) error {
	if from == to {
		return nil
	}
	ents, err := os.ReadDir(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		src, dst := filepath.Join(from, e.Name()), filepath.Join(to, e.Name())
		if _, err := os.Stat(dst); err == nil {
			if err := os.Remove(src); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	os.Remove(from) // succeeds only when nothing else lives there
	return nil
}

// removeEmptyDirs deletes root and the directories under it that hold nothing.
func removeEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}
