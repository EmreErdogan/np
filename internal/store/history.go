package store

// History sync support: every node keeps a ledger of the versions it has
// seen; sync unions the ledgers so all nodes end up with every version.
// See .docs/design-history-sync.md for the reasoning.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// HistoryDigest summarises a note's ledger: equal digests mean identical
// sets of versions, so peers can skip the exchange.
func HistoryDigest(m *Meta) string {
	keys := make([]string, 0, len(m.History))
	for _, v := range m.History {
		keys = append(keys, v.Clock.String())
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:8])
}

// HasSnapshot reports whether the content of a version is on disk.
func (s *Store) HasSnapshot(name, hash string) bool {
	_, err := os.Stat(s.snapshotPath(name, hash))
	return err == nil
}

// PutSnapshot stores content received from a peer, verifying its hash.
func (s *Store) PutSnapshot(name, hash string, data []byte) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if hashOf(data) != hash {
		return fmt.Errorf("snapshot hash mismatch for %s", name)
	}
	return s.writeSnapshot(name, hash, data)
}

// AddVersions unions vs into a note's ledger. Versions already present (by
// clock) are ignored, as are non-deleted versions whose snapshot is
// missing, so a caller pulls the content first. The ledger is re-sorted by
// time and renumbered; the current version stays last. Returns how many
// were added.
func (s *Store) AddVersions(name string, vs []Version) (int, error) {
	m := s.idx.Notes[name]
	if m == nil {
		return 0, fmt.Errorf("no note %q", name)
	}
	have := map[string]bool{}
	for _, v := range m.History {
		have[v.Clock.String()] = true
	}
	added := 0
	for _, v := range vs {
		key := v.Clock.String()
		if have[key] || key == "" {
			continue
		}
		if !v.Deleted && !s.HasSnapshot(name, v.Hash) {
			continue
		}
		v.Clock = v.Clock.Copy()
		v.ModTime = v.ModTime.UTC()
		m.History = append(m.History, v)
		have[key] = true
		added++
	}
	if added == 0 {
		return 0, nil
	}
	cur := m.Clock.String()
	sort.SliceStable(m.History, func(i, j int) bool {
		a, b := m.History[i], m.History[j]
		if (a.Clock.String() == cur) != (b.Clock.String() == cur) {
			return b.Clock.String() == cur // current sorts last
		}
		if !a.ModTime.Equal(b.ModTime) {
			return a.ModTime.Before(b.ModTime)
		}
		return a.Clock.String() < b.Clock.String()
	})
	for i := range m.History {
		m.History[i].Seq = i + 1
	}
	return added, s.saveIndex()
}

// MissingVersions returns the entries of remote that the local ledger of
// name lacks, and the local entries remote lacks.
func (s *Store) MissingVersions(name string, remote []Version) (here, there []Version) {
	m := s.idx.Notes[name]
	local := map[string]bool{}
	if m != nil {
		for _, v := range m.History {
			local[v.Clock.String()] = true
		}
	}
	rem := map[string]bool{}
	for _, v := range remote {
		rem[v.Clock.String()] = true
		if !local[v.Clock.String()] {
			here = append(here, v)
		}
	}
	if m != nil {
		for _, v := range m.History {
			if !rem[v.Clock.String()] {
				there = append(there, v)
			}
		}
	}
	return here, there
}
