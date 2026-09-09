// Package store manages the on-disk note repository:
//
//	<dir>/notes/<name>.md     working files, safe to edit with any editor
//	<dir>/history/<name>/<hash>.md  immutable snapshots of every version
//	<dir>/files/<name>        arbitrary files, no history, fetched lazily (files.go)
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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/merge"
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
	Node  string               `json:"node"`
	Notes map[string]*Meta     `json:"notes"`
	Files map[string]*FileMeta `json:"files,omitempty"`
}

// Config is config.json.
type Config struct {
	Hub      string   `json:"hub,omitempty"`
	Port     int      `json:"port,omitempty"`
	Interval int      `json:"interval_seconds,omitempty"`
	Allow    []string `json:"allow,omitempty"` // extra tailnet logins allowed to sync
	// AutoFetch is the largest file whose content is pulled without being
	// asked; 0 means DefaultAutoFetch, negative disables automatic fetching.
	AutoFetch int64 `json:"auto_fetch_bytes,omitempty"`
	// KeepAll makes this node pull every file's content (hub / archive role).
	KeepAll bool `json:"keep_all,omitempty"`
}

// AutoFetchLimit is the effective auto-fetch threshold in bytes (-1 = never).
func (c Config) AutoFetchLimit() int64 {
	switch {
	case c.AutoFetch == 0:
		return DefaultAutoFetch
	case c.AutoFetch < 0:
		return -1
	}
	return c.AutoFetch
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
	author string // overrides ModBy during scanAs
	// idxStamp identifies the index.json this process last read or wrote,
	// so a long-running daemon notices when a CLI command in another
	// process updated it (see Scan).
	idxStamp string
}

// ApplyResult describes what Apply did with an incoming note.
type ApplyResult string

const (
	Accepted   ApplyResult = "accepted"
	Unchanged  ApplyResult = "unchanged"
	Merged     ApplyResult = "merged" // concurrent edits combined without overlap
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
	for _, sub := range []string{"notes", "history", "files"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{Dir: dir, Node: node}
	if err := s.loadIndex(); err != nil {
		return nil, err
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

func (s *Store) indexPath() string { return filepath.Join(s.Dir, "index.json") }

// indexStamp identifies the current index.json on disk ("" when absent).
func (s *Store) indexStamp() string {
	st, err := os.Stat(s.indexPath())
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

func (s *Store) loadIndex() error {
	idx := &Index{Node: s.Node, Notes: map[string]*Meta{}, Files: map[string]*FileMeta{}}
	if err := readJSON(s.indexPath(), idx); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("index.json: %w", err)
	}
	if idx.Notes == nil {
		idx.Notes = map[string]*Meta{}
	}
	if idx.Files == nil {
		idx.Files = map[string]*FileMeta{}
	}
	// Hostname changed: keep going, clocks are keyed by name so old entries stay valid.
	idx.Node = s.Node
	s.idx = idx
	s.idxStamp = s.indexStamp()
	return nil
}

// reloadIfChanged re-reads index.json when another process wrote it since
// we last touched it (np sync run from the CLI while the daemon is up).
func (s *Store) reloadIfChanged() error {
	if s.indexStamp() == s.idxStamp {
		return nil
	}
	return s.loadIndex()
}

func (s *Store) saveIndex() error {
	if err := writeJSON(s.indexPath(), s.idx); err != nil {
		return err
	}
	s.idxStamp = s.indexStamp()
	return nil
}

// extRe matches a short alphanumeric file extension. Anything else after a
// dot (like ".conflict-laptop-20260908") is part of the name, not a type.
var extRe = regexp.MustCompile(`\.([A-Za-z0-9]{1,8})$`)

// Ext returns the note's file extension without the dot, or "" for markdown
// notes (which are stored with an implicit .md).
func Ext(name string) string {
	m := extRe.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// Canon normalises user input: "todo.md" and "todo" are the same note.
func Canon(name string) string {
	return strings.TrimSuffix(name, noteExt)
}

// FileName is the path of a note relative to the notes directory.
func FileName(name string) string {
	if Ext(name) != "" {
		return name
	}
	return name + noteExt
}

// ValidName rejects names that could escape the notes directory.
func ValidName(name string) error {
	if name == "" {
		return errors.New("empty note name")
	}
	if strings.HasSuffix(name, noteExt) {
		return fmt.Errorf("note name should not end with %s (markdown is the default)", noteExt)
	}
	if err := validSegments(name); err != nil {
		return fmt.Errorf("invalid note name %q", name)
	}
	return nil
}

// validSegments rejects paths with empty, dot, dot-dot or dot-prefixed parts.
func validSegments(name string) error {
	if filepath.IsAbs(name) {
		return errors.New("absolute path")
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return errors.New("bad segment")
		}
	}
	return nil
}

func (s *Store) notePath(name string) string {
	return filepath.Join(s.Dir, "notes", filepath.FromSlash(FileName(name)))
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
// It returns the names that changed; files under files/ are reported with a
// "files/" prefix.
func (s *Store) Scan() ([]string, error) {
	if err := s.reloadIfChanged(); err != nil {
		return nil, err
	}
	changed, err := s.scanNotes()
	if err != nil {
		return nil, err
	}
	files, err := s.scanFiles()
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		changed = append(changed, "files/"+f)
	}
	if len(files) > 0 {
		if err := s.saveIndex(); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func (s *Store) scanNotes() ([]string, error) {
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
		if strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		name := Canon(filepath.ToSlash(rel))
		// Only files whose name round-trips are notes: "x.md" -> "x",
		// "cfg.json" -> "cfg.json"; a bare "README" is ignored.
		if ValidName(name) != nil || FileName(name) != filepath.ToSlash(rel) {
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
	if s.author != "" {
		m.ModBy = s.author
	}
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

// LocalPath is the absolute working file path of a note (no dirs created).
func (s *Store) LocalPath(name string) string { return s.notePath(name) }

// Path returns the working file path for a note, creating parent dirs.
func (s *Store) Path(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	p := s.notePath(name)
	return p, os.MkdirAll(filepath.Dir(p), 0o755)
}

// Write replaces the working copy and commits it as this node's edit.
func (s *Store) Write(name string, data []byte) error {
	return s.WriteBy(name, data, s.Node)
}

// WriteBy is Write with the edit attributed to another author, e.g. a phone
// editing through this node's web UI. The vector clock still advances under
// this node's name; only the displayed author differs.
func (s *Store) WriteBy(name string, data []byte, by string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return err
	}
	return s.scanAs(by)
}

// Delete removes the working copy and records a tombstone.
func (s *Store) Delete(name string) error {
	return s.DeleteBy(name, s.Node)
}

// DeleteBy is Delete attributed to another author (see WriteBy).
func (s *Store) DeleteBy(name, by string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := os.Remove(s.notePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.scanAs(by)
}

// scanAs runs Scan with pending changes attributed to by.
func (s *Store) scanAs(by string) error {
	s.author = by
	defer func() { s.author = "" }()
	_, err := s.Scan()
	return err
}

// Apply merges a note received from a peer into the local store.
// Ordering is decided by vector clocks. Concurrent edits of a text note are
// combined with a three-way merge against the last version both sides knew;
// when the edits overlap (or either side is a delete or binary), the newer
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
	// Concurrent with identical content (both sides merged the same way):
	// just reconcile the clocks, nothing to record.
	if local.Hash == remote.Hash && local.Deleted == remote.Deleted {
		local.Clock = clock.Merge(local.Clock, remote.Clock)
		local.History[len(local.History)-1].Clock = local.Clock.Copy()
		return Unchanged, s.saveIndex()
	}
	// Concurrent text edits: try a three-way merge.
	if !local.Deleted && !remote.Deleted {
		if out, ok := s.merge3(local, remote, content); ok {
			if err := s.commitMerge(local, remote, out); err != nil {
				return Rejected, err
			}
			return Merged, s.saveIndex()
		}
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
	// The merge is recorded as a single history entry: if install just appended
	// the remote version, rewrite that entry's clock instead of adding another.
	m := s.idx.Notes[name]
	m.Clock = merged.Bump(s.Node)
	if remoteWins {
		m.History[len(m.History)-1].Clock = m.Clock.Copy()
	} else {
		m.History = append(m.History, Version{Seq: len(m.History) + 1, Hash: m.Hash, ModTime: m.ModTime, ModBy: m.ModBy, Clock: m.Clock.Copy(), Deleted: m.Deleted})
	}
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

// merge3 finds the latest local version the remote side had already seen
// and merges both edits on top of it. ok is false when there is no such
// ancestor, the note is not text, or the edits overlap.
func (s *Store) merge3(local *Meta, remote Meta, remoteData []byte) ([]byte, bool) {
	var base *Version
	for i := len(local.History) - 1; i >= 0; i-- {
		v := &local.History[i]
		if v.Deleted {
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
	localData, err := s.Read(local.Name)
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
	if err := os.WriteFile(p, out, 0o644); err != nil {
		return err
	}
	h := hashOf(out)
	if err := s.writeSnapshot(local.Name, h, out); err != nil {
		return err
	}
	local.Clock = clock.Merge(local.Clock, remote.Clock).Bump(s.Node)
	local.Hash = h
	local.ModTime = time.Now().UTC()
	local.ModBy = s.Node
	local.Deleted = false
	local.History = append(local.History, Version{
		Seq: len(local.History) + 1, Hash: h, ModTime: local.ModTime, ModBy: local.ModBy, Clock: local.Clock.Copy(),
	})
	return nil
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

// listItemRe matches a markdown bullet, numbered or task list line.
var listItemRe = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)

// Join appends text to existing content following np's separator rule: a
// blank line is inserted so the addition renders as its own paragraph,
// except when a list item is appended to a list, which keeps the list tight.
func Join(existing, text string) []byte {
	text = strings.TrimRight(text, "\n") + "\n"
	if strings.TrimSpace(existing) == "" {
		return []byte(text)
	}
	if !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	lines := strings.Split(strings.TrimRight(existing, "\n"), "\n")
	last := lines[len(lines)-1]
	tight := listItemRe.MatchString(text) && listItemRe.MatchString(last)
	if !tight && !strings.HasSuffix(existing, "\n\n") {
		existing += "\n"
	}
	return []byte(existing + text)
}

// Append adds text to the end of a note (creating it if needed) as this node.
func (s *Store) Append(name, text string) error {
	return s.AppendBy(name, text, s.Node)
}

// AppendBy is Append attributed to another author (see WriteBy).
func (s *Store) AppendBy(name, text, by string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to add")
	}
	if _, err := s.Scan(); err != nil {
		return err
	}
	existing, err := s.Read(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.WriteBy(name, Join(string(existing), text), by)
}
