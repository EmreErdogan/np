// Package store manages the on-disk note repository:
//
//	<dir>/notes/<name>.md     working files, safe to edit with any editor
//	<dir>/history/<name>/<hash>.md  immutable snapshots of every version
//	<dir>/index.json          per-note metadata (vector clock, hash, tombstones)
//	<dir>/config.json         hub, port, allowed logins
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/emre/np/internal/clock"
)

const noteExt = ".md"

// Version is one historical snapshot of a note.
type Version struct {
	Seq     int         `json:"seq"`
	Hash    string      `json:"hash"`
	ModTime time.Time   `json:"mtime"`
	ModBy   string      `json:"by"`
	Clock   clock.Clock `json:"clock"`
	Deleted bool        `json:"deleted,omitempty"`
}

// Meta is the synchronised state of one note.
type Meta struct {
	Name    string      `json:"name"`
	Clock   clock.Clock `json:"clock"`
	Hash    string      `json:"hash"`
	ModTime time.Time   `json:"mtime"`
	ModBy   string      `json:"by"`
	Deleted bool        `json:"deleted,omitempty"`
	History []Version   `json:"history,omitempty"`
}

// Index is index.json.
type Index struct {
	Node  string           `json:"node"`
	Notes map[string]*Meta `json:"notes"`
}

// Config is config.json.
type Config struct {
	Hub      string   `json:"hub,omitempty"`
	Port     int      `json:"port,omitempty"`
	Interval int      `json:"interval_seconds,omitempty"`
	Allow    []string `json:"allow,omitempty"` // extra tailnet logins allowed to sync
}

const (
	DefaultPort     = 7373
	DefaultInterval = 15
)

// Store is an open repository.
type Store struct {
	Dir    string
	Node   string
	Config Config
	idx    *Index
}

// ApplyResult describes what Apply did with an incoming note.
type ApplyResult string

const (
	Accepted   ApplyResult = "accepted"
	Unchanged  ApplyResult = "unchanged"
	Conflicted ApplyResult = "conflicted"
	Rejected   ApplyResult = "rejected" // local is newer; sender should pull
)

// DefaultDir returns $NP_DIR or ~/.np.
func DefaultDir() (string, error) {
	if d := os.Getenv("NP_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".np"), nil
}

// Open loads or initialises the repository at dir for the given node name.
func Open(dir, node string) (*Store, error) {
	if node == "" {
		return nil, errors.New("store: empty node name")
	}
	for _, sub := range []string{"notes", "history"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{Dir: dir, Node: node, idx: &Index{Node: node, Notes: map[string]*Meta{}}}
	if err := readJSON(filepath.Join(dir, "index.json"), s.idx); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("index.json: %w", err)
	}
	if s.idx.Notes == nil {
		s.idx.Notes = map[string]*Meta{}
	}
	if s.idx.Node != node {
		// Hostname changed; keep going, clocks are keyed by name so old entries stay valid.
		s.idx.Node = node
	}
	if err := readJSON(filepath.Join(dir, "config.json"), &s.Config); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if s.Config.Port == 0 {
		s.Config.Port = DefaultPort
	}
	if s.Config.Interval == 0 {
		s.Config.Interval = DefaultInterval
	}
	return s, nil
}

// SaveConfig persists config.json.
func (s *Store) SaveConfig() error {
	return writeJSON(filepath.Join(s.Dir, "config.json"), s.Config)
}

func (s *Store) saveIndex() error {
	return writeJSON(filepath.Join(s.Dir, "index.json"), s.idx)
}

// ValidName rejects names that could escape the notes directory.
func ValidName(name string) error {
	if name == "" {
		return errors.New("empty note name")
	}
	if strings.HasSuffix(name, noteExt) {
		return fmt.Errorf("note name should not end with %s", noteExt)
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return fmt.Errorf("invalid note name %q", name)
		}
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("invalid note name %q", name)
	}
	return nil
}

func (s *Store) notePath(name string) string {
	return filepath.Join(s.Dir, "notes", filepath.FromSlash(name)+noteExt)
}

func (s *Store) snapshotPath(name, hash string) string {
	return filepath.Join(s.Dir, "history", filepath.FromSlash(name), hash+noteExt)
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Scan compares the working files with the index and records every change
// made outside np (editor, rm, new files) as a new version by this node.
// It returns the names that changed.
func (s *Store) Scan() ([]string, error) {
	root := filepath.Join(s.Dir, "notes")
	seen := map[string]bool{}
	var changed []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, noteExt) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		name := filepath.ToSlash(strings.TrimSuffix(rel, noteExt))
		if ValidName(name) != nil {
			return nil
		}
		seen[name] = true
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := hashOf(data)
		m := s.idx.Notes[name]
		if m != nil && !m.Deleted && m.Hash == h {
			return nil
		}
		info, _ := d.Info()
		mt := time.Now()
		if info != nil {
			mt = info.ModTime()
		}
		if err := s.commit(name, data, h, mt, false); err != nil {
			return err
		}
		changed = append(changed, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name, m := range s.idx.Notes {
		if !seen[name] && !m.Deleted {
			if err := s.commit(name, nil, "", time.Now(), true); err != nil {
				return nil, err
			}
			changed = append(changed, name)
		}
	}
	if len(changed) > 0 {
		if err := s.saveIndex(); err != nil {
			return nil, err
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// commit records a local version (clock bumped by this node) without saving the index.
func (s *Store) commit(name string, data []byte, hash string, mt time.Time, deleted bool) error {
	m := s.idx.Notes[name]
	if m == nil {
		m = &Meta{Name: name, Clock: clock.Clock{}}
		s.idx.Notes[name] = m
	}
	m.Clock = m.Clock.Bump(s.Node)
	m.Hash = hash
	m.ModTime = mt.UTC()
	m.ModBy = s.Node
	m.Deleted = deleted
	if !deleted {
		if err := s.writeSnapshot(name, hash, data); err != nil {
			return err
		}
	}
	m.History = append(m.History, Version{
		Seq: len(m.History) + 1, Hash: hash, ModTime: m.ModTime, ModBy: m.ModBy, Clock: m.Clock.Copy(), Deleted: deleted,
	})
	return nil
}

func (s *Store) writeSnapshot(name, hash string, data []byte) error {
	p := s.snapshotPath(name, hash)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Snapshot returns the content of a historical version.
func (s *Store) Snapshot(name, hash string) ([]byte, error) {
	return os.ReadFile(s.snapshotPath(name, hash))
}

// Get returns metadata for a note (including tombstones), or nil.
func (s *Store) Get(name string) *Meta {
	return s.idx.Notes[name]
}

// List returns all metadata sorted by name, including tombstones if withDeleted.
func (s *Store) List(withDeleted bool) []*Meta {
	out := make([]*Meta, 0, len(s.idx.Notes))
	for _, m := range s.idx.Notes {
		if m.Deleted && !withDeleted {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Read returns the working copy of a note.
func (s *Store) Read(name string) ([]byte, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	return os.ReadFile(s.notePath(name))
}

// Path returns the working file path for a note, creating parent dirs.
func (s *Store) Path(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	p := s.notePath(name)
	return p, os.MkdirAll(filepath.Dir(p), 0o755)
}

// Write replaces the working copy and commits it.
func (s *Store) Write(name string, data []byte) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return err
	}
	_, err = s.Scan()
	return err
}

// Delete removes the working copy and records a tombstone.
func (s *Store) Delete(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := os.Remove(s.notePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err := s.Scan()
	return err
}

// Apply merges a note received from a peer into the local store.
// Ordering is decided by vector clocks; on concurrent edits the newer
// modification time wins and the loser is preserved as a conflict note.
func (s *Store) Apply(remote Meta, content []byte) (ApplyResult, error) {
	if err := ValidName(remote.Name); err != nil {
		return Rejected, err
	}
	if _, err := s.Scan(); err != nil { // pick up local edits before comparing
		return Rejected, err
	}
	name := remote.Name
	local := s.idx.Notes[name]
	cmp := clock.Dominates
	if local != nil {
		cmp = clock.Compare(remote.Clock, local.Clock)
	}
	switch cmp {
	case clock.Equal, clock.Dominated:
		if local != nil && local.Hash == remote.Hash && local.Deleted == remote.Deleted {
			return Unchanged, nil
		}
		return Rejected, nil
	case clock.Dominates:
		if err := s.install(remote, content); err != nil {
			return Rejected, err
		}
		return Accepted, s.saveIndex()
	}
	// Concurrent: decide a winner, keep the loser as a conflict copy.
	remoteWins := remote.ModTime.After(local.ModTime) ||
		(remote.ModTime.Equal(local.ModTime) && remote.ModBy > local.ModBy)
	loserName := fmt.Sprintf("%s.conflict-%s-%s", name, local.ModBy, local.ModTime.Format("20060102-150405"))
	var loserData []byte
	if remoteWins {
		if !local.Deleted {
			loserData, _ = s.Read(name)
		}
	} else {
		loserName = fmt.Sprintf("%s.conflict-%s-%s", name, remote.ModBy, remote.ModTime.Format("20060102-150405"))
		if !remote.Deleted {
			loserData = content
		}
	}
	merged := clock.Merge(local.Clock, remote.Clock)
	if remoteWins {
		if err := s.install(remote, content); err != nil {
			return Rejected, err
		}
	}
	// Bump so the merged version dominates both inputs and propagates everywhere.
	m := s.idx.Notes[name]
	m.Clock = merged.Bump(s.Node)
	m.History = append(m.History, Version{Seq: len(m.History) + 1, Hash: m.Hash, ModTime: m.ModTime, ModBy: m.ModBy, Clock: m.Clock.Copy(), Deleted: m.Deleted})
	if loserData != nil {
		p, err := s.Path(loserName)
		if err != nil {
			return Rejected, err
		}
		if err := os.WriteFile(p, loserData, 0o644); err != nil {
			return Rejected, err
		}
		if _, err := s.Scan(); err != nil {
			return Rejected, err
		}
	}
	return Conflicted, s.saveIndex()
}

// install writes remote's content/metadata as-is (no clock bump).
func (s *Store) install(remote Meta, content []byte) error {
	name := remote.Name
	if remote.Deleted {
		if err := os.Remove(s.notePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else {
		if hashOf(content) != remote.Hash {
			return fmt.Errorf("content hash mismatch for %s", name)
		}
		p, err := s.Path(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return err
		}
		if err := os.Chtimes(p, remote.ModTime, remote.ModTime); err != nil {
			return err
		}
		if err := s.writeSnapshot(name, remote.Hash, content); err != nil {
			return err
		}
	}
	m := s.idx.Notes[name]
	if m == nil {
		m = &Meta{Name: name}
		s.idx.Notes[name] = m
	}
	m.Clock = remote.Clock.Copy()
	m.Hash = remote.Hash
	m.ModTime = remote.ModTime.UTC()
	m.ModBy = remote.ModBy
	m.Deleted = remote.Deleted
	m.History = append(m.History, Version{Seq: len(m.History) + 1, Hash: m.Hash, ModTime: m.ModTime, ModBy: m.ModBy, Clock: m.Clock.Copy(), Deleted: m.Deleted})
	return nil
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
