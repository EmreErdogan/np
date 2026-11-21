package store

// Rename keeps a file's identity: the new name inherits the clock and the
// whole ledger (snapshots are copied under the new name), the old name
// becomes a tombstone that points forward. Peers need no special handling:
// the new entry carries its history, so history sync gives every node the
// same ledger under the new name, and a peer that holds the old content
// copies it locally instead of downloading it again.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

// Rename moves a file (content or stub) to a new name, attributed to by.
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
	old := s.idx.Entries[oldName]
	if old == nil || old.Deleted {
		return fmt.Errorf("no file %q", oldName)
	}
	cur := s.idx.Entries[newName]
	if cur != nil && !cur.Deleted {
		return fmt.Errorf("a file named %q already exists", newName)
	}
	if by == "" {
		by = s.Node
	}
	now := time.Now().UTC()
	if old.Have {
		dst, err := s.Path(newName)
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
	if err := copySnapshots(filepath.Dir(s.snapshotPath(oldName, "x")), filepath.Dir(s.snapshotPath(newName, "x"))); err != nil {
		return err
	}
	// New record: inherit clock and ledger, then one "renamed" version.
	nm := &Meta{
		Name: newName, Hash: old.Hash, Size: old.Size, ModTime: now, ModBy: by,
		Blob: old.Blob, Have: old.Have, RenamedFrom: oldName,
	}
	nm.Clock = old.Clock.Copy()
	if cur != nil { // reviving a tombstone: dominate it too
		nm.Clock = clock.Merge(nm.Clock, cur.Clock)
		nm.History = append(nm.History, cur.History...)
	}
	nm.Clock = nm.Clock.Bump(s.Node)
	nm.History = append(nm.History, old.History...)
	nm.History = append(nm.History, Version{
		Hash: nm.Hash, Size: nm.Size, ModTime: now, ModBy: by, Clock: nm.Clock.Copy(), Blob: nm.Blob, RenamedFrom: oldName,
	})
	for i := range nm.History {
		nm.History[i].Seq = i + 1
	}
	if nm.Have {
		nm.Stamp = s.stamp(newName)
	}
	s.idx.Entries[newName] = nm
	// Old record: tombstone pointing forward.
	old.Clock = old.Clock.Bump(s.Node)
	old.Hash, old.Size, old.ModTime, old.ModBy = "", 0, now, by
	old.Deleted, old.Have, old.Stamp, old.RenamedTo = true, false, "", newName
	old.History = append(old.History, Version{
		Seq: len(old.History) + 1, ModTime: now, ModBy: by, Clock: old.Clock.Copy(), Deleted: true, RenamedTo: newName,
	})
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

// CanCarry reports whether an incoming renamed file can be fulfilled from
// local content (so a sync need not download it).
func (s *Store) CanCarry(remote Meta) bool {
	if remote.RenamedFrom == "" || remote.Deleted {
		return false
	}
	old := s.idx.Entries[remote.RenamedFrom]
	return old != nil && old.Have && old.Hash == remote.Hash
}

// carry fulfils an incoming renamed file from the content this node holds
// under the old name, so nothing is downloaded. Reports whether it did.
func (s *Store) carry(remote Meta) bool {
	if !s.CanCarry(remote) {
		return false
	}
	dst, err := s.Path(remote.Name)
	if err != nil {
		return false
	}
	// Copy rather than move: the old name's tombstone may not have arrived
	// yet, and a scan must not mistake the missing old file for a delete.
	f, err := os.Open(s.filePath(remote.RenamedFrom))
	if err != nil {
		return false
	}
	defer f.Close()
	if _, _, err := writeFileFrom(dst, f, remote.Hash); err != nil {
		return false
	}
	os.Chtimes(dst, remote.ModTime, remote.ModTime)
	return true
}
