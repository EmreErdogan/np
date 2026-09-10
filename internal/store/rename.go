package store

// Rename keeps a note's identity: the new name inherits the clock and the
// whole ledger (snapshots are copied under the new name), the old name
// becomes a tombstone that points forward. Peers need no special handling:
// the new note carries its history, so history sync gives every node the
// same ledger under the new name. Files rename the same way; a peer that
// holds the old content moves it instead of downloading it again.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

// Rename moves a note to a new name, attributed to by.
func (s *Store) Rename(oldName, newName, by string) error {
	if err := ValidName(oldName); err != nil {
		return err
	}
	if err := ValidName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return errors.New("same name")
	}
	if _, err := s.Scan(); err != nil {
		return err
	}
	old := s.idx.Notes[oldName]
	if old == nil || old.Deleted {
		return fmt.Errorf("no note %q", oldName)
	}
	if cur := s.idx.Notes[newName]; cur != nil && !cur.Deleted {
		return fmt.Errorf("a note named %q already exists", newName)
	}
	// Working file and snapshots.
	dst, err := s.Path(newName)
	if err != nil {
		return err
	}
	if err := os.Rename(s.notePath(oldName), dst); err != nil {
		return err
	}
	if err := copySnapshots(filepath.Join(s.Dir, "history", filepath.FromSlash(oldName)), filepath.Join(s.Dir, "history", filepath.FromSlash(newName))); err != nil {
		return err
	}
	if by == "" {
		by = s.Node
	}
	now := time.Now().UTC()
	// New record: inherit clock and ledger, then one "renamed" version.
	nm := &Meta{Name: newName, Hash: old.Hash, ModTime: now, ModBy: by, RenamedFrom: oldName}
	nm.Clock = old.Clock.Copy()
	if cur := s.idx.Notes[newName]; cur != nil { // reviving a tombstone: dominate it too
		nm.Clock = clock.Merge(nm.Clock, cur.Clock)
		for _, v := range cur.History {
			nm.History = append(nm.History, v)
		}
	}
	nm.Clock = nm.Clock.Bump(s.Node)
	for _, v := range old.History {
		nm.History = append(nm.History, v)
	}
	nm.History = append(nm.History, Version{Hash: nm.Hash, ModTime: now, ModBy: by, Clock: nm.Clock.Copy(), RenamedFrom: oldName})
	for i := range nm.History {
		nm.History[i].Seq = i + 1
	}
	if err := os.Chtimes(dst, now, now); err != nil {
		return err
	}
	s.idx.Notes[newName] = nm
	// Old record: tombstone pointing forward.
	old.Clock = old.Clock.Bump(s.Node)
	old.Hash, old.ModTime, old.ModBy, old.Deleted, old.RenamedTo = "", now, by, true, newName
	old.History = append(old.History, Version{Seq: len(old.History) + 1, ModTime: now, ModBy: by, Clock: old.Clock.Copy(), Deleted: true, RenamedTo: newName})
	return s.saveIndex()
}

// copySnapshots copies every snapshot file from one history dir to another
// (the old name keeps its ledger browsable).
func copySnapshots(from, to string) error {
	entries, err := os.ReadDir(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		dst := filepath.Join(to, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RenameFile moves a file (content or stub) to a new name, attributed to by.
func (s *Store) RenameFile(oldName, newName, by string) error {
	if err := ValidFileName(oldName); err != nil {
		return err
	}
	if err := ValidFileName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return errors.New("same name")
	}
	if _, err := s.Scan(); err != nil {
		return err
	}
	old := s.idx.Files[oldName]
	if old == nil || old.Deleted {
		return fmt.Errorf("no file %q", oldName)
	}
	if cur := s.idx.Files[newName]; cur != nil && !cur.Deleted {
		return fmt.Errorf("a file named %q already exists", newName)
	}
	if by == "" {
		by = s.Node
	}
	now := time.Now().UTC()
	nm := &FileMeta{Name: newName, Hash: old.Hash, Size: old.Size, ModTime: now, ModBy: by, Have: old.Have, RenamedFrom: oldName}
	nm.Clock = old.Clock.Copy()
	if cur := s.idx.Files[newName]; cur != nil {
		nm.Clock = clock.Merge(nm.Clock, cur.Clock)
	}
	nm.Clock = nm.Clock.Bump(s.Node)
	if old.Have {
		dst, err := s.FilePath(newName)
		if err != nil {
			return err
		}
		if err := os.Rename(s.filePath(oldName), dst); err != nil {
			return err
		}
		if err := os.Chtimes(dst, now, now); err != nil {
			return err
		}
	}
	s.idx.Files[newName] = nm
	old.Clock = old.Clock.Bump(s.Node)
	old.Hash, old.Size, old.ModTime, old.ModBy, old.Deleted, old.Have, old.RenamedTo = "", 0, now, by, true, false, newName
	return s.saveIndex()
}

// CanCarry reports whether an incoming renamed file can be fulfilled from
// local content (so a sync need not download it).
func (s *Store) CanCarry(remote FileMeta) bool {
	if remote.RenamedFrom == "" || remote.Deleted {
		return false
	}
	old := s.idx.Files[remote.RenamedFrom]
	return old != nil && old.Have && old.Hash == remote.Hash
}

// carryFile fulfils an incoming renamed file from the content this node
// holds under the old name, so nothing is downloaded. Reports whether it did.
func (s *Store) carryFile(remote FileMeta) bool {
	if remote.RenamedFrom == "" || remote.Deleted {
		return false
	}
	old := s.idx.Files[remote.RenamedFrom]
	if old == nil || !old.Have || old.Hash != remote.Hash {
		return false
	}
	dst, err := s.FilePath(remote.Name)
	if err != nil {
		return false
	}
	// Copy rather than move: the old name's tombstone may not have arrived
	// yet, and a scan must not mistake the missing old file for a delete.
	data, err := os.ReadFile(s.filePath(remote.RenamedFrom))
	if err != nil {
		return false
	}
	if _, _, err := writeFileFrom(dst, bytesReader(data), remote.Hash); err != nil {
		return false
	}
	os.Chtimes(dst, remote.ModTime, remote.ModTime)
	return true
}
