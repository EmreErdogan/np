package store

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/merge"
)

func same(local *Meta, remote Meta) bool {
	return local.Hash == remote.Hash && local.Deleted == remote.Deleted
}

// Apply merges a version received from a peer into the local store. src
// carries the content when the sender included it; nil means metadata only.
//
// Ordering is decided by vector clocks. Metadata alone never replaces
// content this node holds: such an update is Rejected and the caller should
// try a peer that has the bytes. Concurrent edits of a text file are
// combined with a three-way merge against the last version both sides
// knew; when the edits overlap, or either side is a blob or a delete, the
// newer modification time wins and the loser is kept as a conflict copy.
func (s *Store) Apply(remote Meta, src io.Reader) (ApplyResult, error) {
	if err := ValidName(remote.Name); err != nil {
		return Rejected, err
	}
	if _, err := s.Scan(); err != nil { // pick up local edits before comparing
		return Rejected, err
	}
	name := remote.Name
	local := s.idx.Entries[name]
	cmp := clock.Dominates
	if local != nil {
		cmp = clock.Compare(remote.Clock, local.Clock)
	}
	switch cmp {
	case clock.Equal, clock.Dominated:
		if !same(local, remote) {
			return Rejected, nil
		}
		if cmp == clock.Equal && !local.Have && !local.Deleted && src != nil {
			if err := s.installContent(local, src); err != nil {
				return Rejected, err
			}
			return Accepted, s.saveIndex()
		}
		return Unchanged, nil
	case clock.Dominates:
		if src == nil && !remote.Deleted && local != nil && local.Have && !local.Deleted {
			return Rejected, nil
		}
		if err := s.install(remote, src); err != nil {
			return Rejected, err
		}
		return Accepted, s.saveIndex()
	}

	// Concurrent with identical content (the same bytes arrived by two
	// routes, or both sides merged the same way): reconcile the clocks.
	if same(local, remote) {
		local.Clock = clock.Merge(local.Clock, remote.Clock)
		if n := len(local.History); n > 0 {
			local.History[n-1].Clock = local.Clock.Copy()
		}
		res := Unchanged
		if !local.Have && !local.Deleted && src != nil {
			if err := s.installContent(local, src); err != nil {
				return Rejected, err
			}
			res = Accepted
		}
		return res, s.saveIndex()
	}

	// Concurrent text edits: try a three-way merge.
	if !local.Deleted && !remote.Deleted && !local.Blob && !remote.Blob && local.Have && src != nil {
		data, err := io.ReadAll(src)
		if err != nil {
			return Rejected, err
		}
		if out, ok := s.merge3(local, remote, data); ok {
			if err := s.commitMerge(local, remote, out); err != nil {
				return Rejected, err
			}
			return Merged, s.saveIndex()
		}
		src = bytes.NewReader(data)
	}

	// Concurrent: newer mtime wins; the loser's content, when present, is
	// kept as a conflict copy.
	remoteWins := remote.ModTime.After(local.ModTime) ||
		(remote.ModTime.Equal(local.ModTime) && remote.ModBy > local.ModBy)
	merged := clock.Merge(local.Clock, remote.Clock)
	if remoteWins {
		aside := ""
		if local.Have && !local.Deleted {
			lp, err := s.Path(conflictName(name, local.ModBy, local.ModTime))
			if err != nil {
				return Rejected, err
			}
			if err := os.Rename(s.filePath(name), lp); err != nil {
				return Rejected, err
			}
			aside = lp
		}
		if err := s.install(remote, src); err != nil {
			if aside != "" {
				os.Rename(aside, s.filePath(name)) // put the local copy back
			}
			return Rejected, err
		}
	} else if src != nil && !remote.Deleted {
		lp, err := s.Path(conflictName(name, remote.ModBy, remote.ModTime))
		if err != nil {
			return Rejected, err
		}
		if _, _, err := writeFileFrom(lp, src, remote.Hash); err != nil {
			return Rejected, err
		}
	}
	// Bump so the result dominates both inputs and propagates everywhere.
	// It is one ledger row: when install just appended the remote version,
	// that row gets the new clock instead of a second row.
	m := s.idx.Entries[name]
	m.Clock = merged.Bump(s.Node)
	if remoteWins {
		m.History[len(m.History)-1].Clock = m.Clock.Copy()
	} else {
		m.History = append(m.History, Version{
			Seq: len(m.History) + 1, Hash: m.Hash, Size: m.Size, ModTime: m.ModTime, ModBy: m.ModBy,
			Clock: m.Clock.Copy(), Deleted: m.Deleted, Blob: m.Blob,
		})
	}
	if err := s.saveIndex(); err != nil {
		return Rejected, err
	}
	if _, err := s.Scan(); err != nil { // records the conflict copy
		return Rejected, err
	}
	return Conflicted, nil
}

// merge3 finds the latest local version the remote side had already seen
// and merges both edits on top of it. ok is false when there is no such
// ancestor, the content is not text, or the edits overlap.
func (s *Store) merge3(local *Meta, remote Meta, remoteData []byte) ([]byte, bool) {
	var base *Version
	for i := len(local.History) - 1; i >= 0; i-- {
		v := &local.History[i]
		if v.Deleted || v.Blob {
			continue
		}
		if c := clock.Compare(v.Clock, remote.Clock); c == clock.Equal || c == clock.Dominated {
			base = v
			break
		}
	}
	if base == nil {
		return nil, false
	}
	baseData, err := s.Snapshot(local.Name, base.Hash)
	if err != nil {
		return nil, false
	}
	localData, err := os.ReadFile(s.filePath(local.Name))
	if err != nil {
		return nil, false
	}
	if !merge.IsText(baseData) || !merge.IsText(localData) || !merge.IsText(remoteData) {
		return nil, false
	}
	return merge.Merge(baseData, localData, remoteData)
}

// commitMerge records out as a new version by this node that dominates both
// local and remote.
func (s *Store) commitMerge(local *Meta, remote Meta, out []byte) error {
	p, err := s.Path(local.Name)
	if err != nil {
		return err
	}
	if err := writeAtomic(p, out); err != nil {
		return err
	}
	h := hashOf(out)
	if err := s.writeSnapshot(local.Name, h, out); err != nil {
		return err
	}
	local.Clock = clock.Merge(local.Clock, remote.Clock).Bump(s.Node)
	local.Hash, local.Size = h, int64(len(out))
	local.ModTime, local.ModBy = time.Now().UTC(), s.Node
	local.Deleted, local.Blob, local.Have, local.Stamp = false, false, true, s.stamp(local.Name)
	local.History = append(local.History, Version{
		Seq: len(local.History) + 1, Hash: h, Size: local.Size, ModTime: local.ModTime, ModBy: local.ModBy, Clock: local.Clock.Copy(),
	})
	return nil
}

// install makes remote the current version as-is (no clock bump). The
// content is written and verified before the index entry changes, so a
// failed transfer leaves the previous version intact.
func (s *Store) install(remote Meta, src io.Reader) error {
	name := remote.Name
	have := false
	size := remote.Size
	switch {
	case remote.Deleted:
		if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case src != nil:
		p, err := s.Path(name)
		if err != nil {
			return err
		}
		_, n, err := writeFileFrom(p, src, remote.Hash)
		if err != nil {
			return err
		}
		if err := os.Chtimes(p, remote.ModTime, remote.ModTime); err != nil {
			return err
		}
		size, have = n, true
	case s.carry(remote):
		have = true
	default:
		// A stub: whatever is on disk is an older version.
		if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if have && !remote.Blob {
		if err := s.snapshotWorking(name, remote.Hash); err != nil {
			return err
		}
	}
	m := s.idx.Entries[name]
	if m == nil {
		m = &Meta{Name: name}
		s.idx.Entries[name] = m
	}
	m.Clock = remote.Clock.Copy()
	m.Hash, m.Size, m.ModTime, m.ModBy = remote.Hash, size, remote.ModTime.UTC(), remote.ModBy
	m.Deleted, m.Blob, m.Have = remote.Deleted, remote.Blob, have
	m.RenamedFrom, m.RenamedTo = remote.RenamedFrom, remote.RenamedTo
	m.Stamp = ""
	if have {
		m.Stamp = s.stamp(name)
	}
	m.History = append(m.History, Version{
		Seq: len(m.History) + 1, Hash: m.Hash, Size: m.Size, ModTime: m.ModTime, ModBy: m.ModBy,
		Clock: m.Clock.Copy(), Deleted: m.Deleted, Blob: m.Blob,
		RenamedFrom: remote.RenamedFrom, RenamedTo: remote.RenamedTo,
	})
	if have {
		return s.promoteIfText(m)
	}
	return nil
}
