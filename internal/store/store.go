// Package store manages the on-disk repository:
//
//	<dir>/files/<name>              working files of any type, safe to edit with any editor
//	<dir>/history/<name>/<hash>.md  immutable snapshots of every text version
//	<dir>/index.json                per-file metadata (vector clock, hash, ledger, tombstones)
//	<dir>/config.json               hub, port, allowed logins
//
// Every file is one entry under its real relative path ("todo.md",
// "trips/cat.jpg"). What an entry can do follows from its content: small
// text is versioned, merged and always synced; anything else is a blob
// that keeps no history and whose bytes travel lazily.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/merge"
)

const (
	// Layout is the on-disk format this binary reads and writes.
	Layout = 2
	// TextMax is the largest file that is treated as versioned text.
	TextMax = 1 << 20
	// DefaultAutoFetch is the size up to which blob content is pulled
	// without being asked (2 MiB). Config.AutoFetch < 0 disables it.
	DefaultAutoFetch = 2 << 20

	snapExt = ".md" // snapshot files are <hash>.md whatever the entry's type
	mdExt   = ".md"
)

// Version is one row of an entry's ledger.
type Version struct {
	Seq     int         `json:"seq"`
	Hash    string      `json:"hash"`
	Size    int64       `json:"size,omitempty"`
	ModTime time.Time   `json:"mtime"`
	ModBy   string      `json:"by"`
	Clock   clock.Clock `json:"clock"`
	Deleted bool        `json:"deleted,omitempty"`
	// Blob versions keep no snapshot: the row only says who changed what when.
	Blob bool `json:"blob,omitempty"`
	// RenamedFrom / RenamedTo mark the two ends of a rename.
	RenamedFrom string `json:"from,omitempty"`
	RenamedTo   string `json:"to,omitempty"`
}

// Meta is the synchronised state of one file.
type Meta struct {
	Name    string      `json:"name"`
	Clock   clock.Clock `json:"clock"`
	Hash    string      `json:"hash"`
	Size    int64       `json:"size"`
	ModTime time.Time   `json:"mtime"`
	ModBy   string      `json:"by"`
	Deleted bool        `json:"deleted,omitempty"`
	// Blob is set by the node that committed the current version: the
	// content is binary or larger than TextMax, so it has no snapshot and
	// its bytes are fetched lazily.
	Blob bool `json:"blob,omitempty"`
	// Have is per node: whether this node holds the content of Hash.
	Have    bool      `json:"have"`
	History []Version `json:"history,omitempty"`
	// RenamedFrom is set on an entry created by a rename; RenamedTo on the
	// tombstone it left behind.
	RenamedFrom string `json:"renamed_from,omitempty"`
	RenamedTo   string `json:"renamed_to,omitempty"`
	// HistoryDigest is filled only on the wire (index responses) so peers
	// can tell whether their ledgers differ; see HistoryDigest().
	HistoryDigest string `json:"hdigest,omitempty"`
	// Stamp is local: size and mtime of the working file when it was last
	// hashed, so Scan can skip files that did not change.
	Stamp string `json:"stamp,omitempty"`
}

// Wire returns the metadata as peers see it: no ledger, nothing local.
func (m *Meta) Wire() Meta {
	c := *m
	c.History = nil
	c.Stamp = ""
	return c
}

// Index is index.json.
type Index struct {
	Layout int    `json:"layout"`
	Node   string `json:"node"`
	// Guard occupies the key older binaries read their notes from. It is a
	// string, so they fail to load the index instead of misreading it.
	Guard   string           `json:"notes"`
	Entries map[string]*Meta `json:"entries"`
}

const indexGuard = "np layout 2: every file is under entries; upgrade np on this machine"

// Config is config.json.
type Config struct {
	Hub      string   `json:"hub,omitempty"`
	Port     int      `json:"port,omitempty"`
	Interval int      `json:"interval_seconds,omitempty"`
	Allow    []string `json:"allow,omitempty"` // extra tailnet logins allowed to sync
	// AutoFetch is the largest blob whose content is pulled without being
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
	// Notices describes what a layout migration did when Open ran one.
	Notices []string
	idx     *Index
	author  string // overrides ModBy during scanAs
	// idxStamp identifies the index.json this process last read or wrote,
	// so a long-running daemon notices when a CLI command in another
	// process updated it (see Scan).
	idxStamp string
}

// ApplyResult describes what Apply did with an incoming file.
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
// A repository in an older layout is migrated first (see migrate.go).
func Open(dir, node string) (*Store, error) {
	if node == "" {
		return nil, errors.New("store: empty node name")
	}
	for _, sub := range []string{"files", "history"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{Dir: dir, Node: node}
	if err := s.loadIndex(); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(dir, "notes")) // an empty leftover of layout 1; fails when it holds anything
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
	raw, err := os.ReadFile(s.indexPath())
	if errors.Is(err, fs.ErrNotExist) {
		s.idx = &Index{Layout: Layout, Node: s.Node, Guard: indexGuard, Entries: map[string]*Meta{}}
		s.idxStamp = ""
		return nil
	}
	if err != nil {
		return err
	}
	var probe struct {
		Layout int `json:"layout"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	switch {
	case probe.Layout > Layout:
		return fmt.Errorf("index.json has layout %d, this np understands up to %d: upgrade np", probe.Layout, Layout)
	case probe.Layout < Layout:
		return s.migrate(raw)
	}
	idx := &Index{}
	if err := json.Unmarshal(raw, idx); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	if idx.Entries == nil {
		idx.Entries = map[string]*Meta{}
	}
	// Hostname changed: keep going, clocks are keyed by name so old entries stay valid.
	idx.Node, idx.Guard = s.Node, indexGuard
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

// ---- names ------------------------------------------------------------------

// extRe matches a short alphanumeric file extension. Anything else after a
// dot (like ".conflict-laptop-20260908") is part of the name, not a type.
var extRe = regexp.MustCompile(`\.([A-Za-z0-9]{1,8})$`)

// Ext returns the name's extension in lower case without the dot, or "".
func Ext(name string) string {
	m := extRe.FindStringSubmatch(path.Base(name))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// IsMarkdown reports whether a name is rendered as markdown.
func IsMarkdown(name string) bool {
	e := Ext(name)
	return e == "md" || e == "markdown"
}

// DefaultName is the name a new text file gets from what the user typed:
// markdown unless an extension says otherwise ("todo" -> "todo.md").
func DefaultName(arg string) string {
	if Ext(arg) == "" {
		return arg + mdExt
	}
	return arg
}

// RenameTarget turns what a user typed as the new name of old into a full
// name: a trailing slash means "into this folder", and a name without an
// extension keeps the extension old has ("np mv todo.md work/todo").
func RenameTarget(old, to string) string {
	into := strings.HasSuffix(to, "/")
	to = strings.Trim(to, "/")
	if into {
		return to + "/" + path.Base(old)
	}
	if Ext(to) == "" {
		if loc := extRe.FindStringIndex(path.Base(old)); loc != nil && loc[0] > 0 {
			to += path.Base(old)[loc[0]:]
		}
	}
	return to
}

// Resolve maps what a user typed to the name of an existing entry: the
// exact name, or the markdown shorthand ("todo" for "todo.md"). Live
// entries win over tombstones. It returns "" when nothing matches.
func (s *Store) Resolve(arg string) string {
	cands := []string{arg}
	if !strings.HasSuffix(arg, mdExt) {
		cands = append(cands, arg+mdExt)
	}
	for _, c := range cands {
		if m := s.idx.Entries[c]; m != nil && !m.Deleted {
			return c
		}
	}
	for _, c := range cands {
		if s.idx.Entries[c] != nil {
			return c
		}
	}
	return ""
}

// ValidName rejects names that could escape the files directory or clash
// with np's own temporary files.
func ValidName(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	if strings.HasSuffix(name, ".tmp") {
		return fmt.Errorf("invalid name %q (.tmp is reserved)", name)
	}
	if err := validSegments(name); err != nil {
		return fmt.Errorf("invalid name %q", name)
	}
	return nil
}

// validSegments rejects paths with empty, dot, dot-dot or dot-prefixed parts.
func validSegments(name string) error {
	if filepath.IsAbs(name) || strings.Contains(name, "\\") {
		return errors.New("absolute path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return errors.New("bad segment")
		}
	}
	return nil
}

// conflictName is where the losing side of a conflict is kept; the
// extension stays last so the copy opens like the original.
func conflictName(name, by string, t time.Time) string {
	dir, base := path.Split(name)
	ext := ""
	if loc := extRe.FindStringIndex(base); loc != nil && loc[0] > 0 {
		base, ext = base[:loc[0]], base[loc[0]:]
	}
	return fmt.Sprintf("%s%s.conflict-%s-%s%s", dir, base, by, t.Format("20060102-150405"), ext)
}

func (s *Store) filePath(name string) string {
	return filepath.Join(s.Dir, "files", filepath.FromSlash(name))
}

func (s *Store) snapshotPath(name, hash string) string {
	return filepath.Join(s.Dir, "history", filepath.FromSlash(name), hash+snapExt)
}

// LocalPath is the absolute working path of a file (no dirs created).
func (s *Store) LocalPath(name string) string { return s.filePath(name) }

// Path returns the working path of a file, creating parent dirs.
func (s *Store) Path(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	p := s.filePath(name)
	return p, os.MkdirAll(filepath.Dir(p), 0o755)
}

// ---- reading ----------------------------------------------------------------

// Get returns metadata for a file (including tombstones and stubs), or nil.
func (s *Store) Get(name string) *Meta { return s.idx.Entries[name] }

// List returns all metadata sorted by name, including tombstones if withDeleted.
func (s *Store) List(withDeleted bool) []*Meta {
	out := make([]*Meta, 0, len(s.idx.Entries))
	for _, m := range s.idx.Entries {
		if m.Deleted && !withDeleted {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// held returns the entry when this node holds its content.
func (s *Store) held(name string) (*Meta, error) {
	m := s.idx.Entries[name]
	if m == nil || m.Deleted {
		return nil, fmt.Errorf("no file %q", name)
	}
	if !m.Have {
		return nil, fmt.Errorf("%s is not fetched on this machine (np get %s)", name, name)
	}
	return m, nil
}

// Read returns the content of a file this node holds.
func (s *Store) Read(name string) ([]byte, error) {
	if _, err := s.held(name); err != nil {
		return nil, err
	}
	return os.ReadFile(s.filePath(name))
}

// OpenFile opens the content of a file this node holds.
func (s *Store) OpenFile(name string) (*os.File, error) {
	if _, err := s.held(name); err != nil {
		return nil, err
	}
	return os.Open(s.filePath(name))
}

// ReadText returns the content when the file is held here and is text np
// can show, edit and search. It looks at the bytes, not at Meta.Blob, so a
// text file that was synced as a blob still reads as text.
func (s *Store) ReadText(name string) ([]byte, bool) {
	m := s.idx.Entries[name]
	if m == nil || m.Deleted || !m.Have || (m.Blob && m.Size > TextMax) {
		return nil, false
	}
	data, err := os.ReadFile(s.filePath(name))
	if err != nil || !merge.IsText(data) {
		return nil, false
	}
	return data, true
}

// ---- scanning and local changes ---------------------------------------------

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func stampOf(info fs.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// stampGrace is how old a file's mtime must be before its stamp is
// trusted. Filesystems tick coarsely (milliseconds on Linux, a second or
// two elsewhere), so a file written twice within one tick can keep its
// size and mtime while its content changes; such a file is hashed again
// on the next scan instead (the "racy clean" problem git solves the same way).
const stampGrace = 2 * time.Second

// trustedStamp is the stamp to remember for a file just hashed, or "" when
// the file changed too recently to be recognised by size and mtime alone.
func trustedStamp(info fs.FileInfo) string {
	if time.Since(info.ModTime()) < stampGrace {
		return ""
	}
	return stampOf(info)
}

// stamp returns the Stamp to remember for the working file of name.
func (s *Store) stamp(name string) string {
	info, err := os.Stat(s.filePath(name))
	if err != nil {
		return ""
	}
	return trustedStamp(info)
}

// content is what Scan learns about a working file: its hash and, when it
// is small text, the bytes to snapshot.
type content struct {
	hash string
	size int64
	text bool
	data []byte
}

func inspect(p string, size int64) (content, error) {
	if size > TextMax {
		h, err := hashFile(p)
		return content{hash: h, size: size}, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return content{}, err
	}
	return content{hash: hashOf(data), size: int64(len(data)), text: merge.IsText(data), data: data}, nil
}

// Scan compares the working files with the index and records every change
// made outside np (editor, rm, new files) as a new version by this node.
// Files whose size and mtime did not change are not read again. Missing
// stubs are not deletions. It returns the names that changed.
func (s *Store) Scan() ([]string, error) {
	if err := s.reloadIfChanged(); err != nil {
		return nil, err
	}
	root := filepath.Join(s.Dir, "files")
	seen := map[string]bool{}
	var changed []string
	dirty := false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while we were walking
			}
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
		name := filepath.ToSlash(rel)
		if ValidName(name) != nil {
			return nil
		}
		info, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		seen[name] = true
		m := s.idx.Entries[name]
		if m != nil && !m.Deleted && m.Have && m.Stamp != "" && m.Stamp == stampOf(info) {
			return nil
		}
		st := trustedStamp(info)
		c, err := inspect(p, info.Size())
		if err != nil {
			return err
		}
		if m != nil && !m.Deleted && m.Hash == c.hash {
			// Same content (touched, re-fetched or copied back): no new version.
			if c.text {
				if err := s.writeSnapshot(name, c.hash, c.data); err != nil {
					return err
				}
				if m.Blob {
					promote(m)
					dirty = true
				}
			}
			if !m.Have || m.Stamp != st {
				m.Have, m.Stamp = true, st
				dirty = true
			}
			return nil
		}
		if err := s.commit(name, c, info.ModTime(), st); err != nil {
			return err
		}
		changed = append(changed, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name, m := range s.idx.Entries {
		if !seen[name] && !m.Deleted && m.Have {
			s.commitDelete(name, time.Now())
			changed = append(changed, name)
		}
	}
	if dirty || len(changed) > 0 {
		if err := s.saveIndex(); err != nil {
			return nil, err
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func (s *Store) by() string {
	if s.author != "" {
		return s.author
	}
	return s.Node
}

// promote turns the current version of m into versioned text. A version
// is recorded as a blob when the node that recorded it could not look at
// the bytes (files that predate layout 2, or a peer that has not looked
// yet); the first node that holds text for it keeps a snapshot, so the
// content survives the next edit. The clock does not change.
func promote(m *Meta) {
	m.Blob = false
	if n := len(m.History); n > 0 && m.History[n-1].Hash == m.Hash {
		m.History[n-1].Blob = false
	}
}

// promoteIfText snapshots and promotes m when it is recorded as a blob but
// the working file is text np versions.
func (s *Store) promoteIfText(m *Meta) error {
	if !m.Blob || m.Size > TextMax {
		return nil
	}
	data, err := os.ReadFile(s.filePath(m.Name))
	if err != nil || !merge.IsText(data) {
		return err
	}
	if err := s.writeSnapshot(m.Name, m.Hash, data); err != nil {
		return err
	}
	promote(m)
	return nil
}

// commit records the working file as a new local version (clock bumped by
// this node) without saving the index.
func (s *Store) commit(name string, c content, mt time.Time, stamp string) error {
	if c.text {
		if err := s.writeSnapshot(name, c.hash, c.data); err != nil {
			return err
		}
	}
	m := s.idx.Entries[name]
	if m == nil {
		m = &Meta{Name: name, Clock: clock.Clock{}}
		s.idx.Entries[name] = m
	}
	m.Clock = m.Clock.Bump(s.Node)
	m.Hash, m.Size, m.ModTime, m.ModBy = c.hash, c.size, mt.UTC(), s.by()
	m.Deleted, m.Blob, m.Have, m.Stamp = false, !c.text, true, stamp
	m.RenamedTo = ""
	m.History = append(m.History, Version{
		Seq: len(m.History) + 1, Hash: m.Hash, Size: m.Size, ModTime: m.ModTime, ModBy: m.ModBy,
		Clock: m.Clock.Copy(), Blob: m.Blob,
	})
	return nil
}

// commitDelete records a local tombstone without saving the index.
func (s *Store) commitDelete(name string, at time.Time) {
	m := s.idx.Entries[name]
	if m == nil {
		m = &Meta{Name: name, Clock: clock.Clock{}}
		s.idx.Entries[name] = m
	}
	m.Clock = m.Clock.Bump(s.Node)
	m.Hash, m.Size, m.ModTime, m.ModBy = "", 0, at.UTC(), s.by()
	m.Deleted, m.Have, m.Stamp = true, false, ""
	m.History = append(m.History, Version{
		Seq: len(m.History) + 1, ModTime: m.ModTime, ModBy: m.ModBy, Clock: m.Clock.Copy(), Deleted: true,
	})
}

func (s *Store) writeSnapshot(name, hash string, data []byte) error {
	p := s.snapshotPath(name, hash)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeAtomic(p, data)
}

// snapshotWorking stores the working file of name as the snapshot of hash.
func (s *Store) snapshotWorking(name, hash string) error {
	if s.HasSnapshot(name, hash) {
		return nil
	}
	data, err := os.ReadFile(s.filePath(name))
	if err != nil {
		return err
	}
	return s.writeSnapshot(name, hash, data)
}

// Snapshot returns the content of a historical version.
func (s *Store) Snapshot(name, hash string) ([]byte, error) {
	return os.ReadFile(s.snapshotPath(name, hash))
}

// Write replaces the content of a file and commits it as this node's edit.
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
	if err := writeAtomic(p, data); err != nil {
		return err
	}
	return s.scanAs(by)
}

// Put stores src under name as this node's version, attributed to by. The
// content is streamed, so it may be of any size.
func (s *Store) Put(name string, src io.Reader, by string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if _, _, err := writeFileFrom(p, src, ""); err != nil {
		return err
	}
	return s.scanAs(by)
}

// Delete removes a file (content or stub) and records a tombstone.
func (s *Store) Delete(name string) error {
	return s.DeleteBy(name, s.Node)
}

// DeleteBy is Delete attributed to another author (see WriteBy).
func (s *Store) DeleteBy(name, by string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := s.scanAs(by); err != nil { // an unsaved edit gets its snapshot first
		return err
	}
	m := s.idx.Entries[name]
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.author = by
	s.commitDelete(name, time.Now())
	s.author = ""
	return s.saveIndex()
}

// Drop removes the local content of a blob but keeps knowing about it, so
// it can be fetched again later. No version is recorded.
func (s *Store) Drop(name string) error {
	m := s.idx.Entries[name]
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if !m.Blob {
		return fmt.Errorf("%s is text; text files are kept on every machine", name)
	}
	if !m.Have {
		return nil
	}
	if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	m.Have, m.Stamp = false, ""
	return s.saveIndex()
}

// Fill stores the content of a stub, verifying it against the hash the
// index already knows.
func (s *Store) Fill(name string, src io.Reader) error {
	m := s.idx.Entries[name]
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if m.Have {
		return nil
	}
	if err := s.installContent(m, src); err != nil {
		return err
	}
	return s.saveIndex()
}

// installContent writes src as the content of the version m describes.
func (s *Store) installContent(m *Meta, src io.Reader) error {
	p, err := s.Path(m.Name)
	if err != nil {
		return err
	}
	_, n, err := writeFileFrom(p, src, m.Hash)
	if err != nil {
		return err
	}
	if err := os.Chtimes(p, m.ModTime, m.ModTime); err != nil {
		return err
	}
	if !m.Blob {
		if err := s.snapshotWorking(m.Name, m.Hash); err != nil {
			return err
		}
	}
	m.Size, m.Have, m.Stamp = n, true, s.stamp(m.Name)
	return s.promoteIfText(m)
}

// scanAs runs Scan with pending changes attributed to by.
func (s *Store) scanAs(by string) error {
	s.author = by
	defer func() { s.author = "" }()
	_, err := s.Scan()
	return err
}

// ---- small helpers ----------------------------------------------------------

// writeAtomic replaces p through a temp file, so a crash never leaves a
// half-written working file for Scan to commit.
func writeAtomic(p string, data []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// writeFileFrom streams src into dst through a temp file, returning the
// content hash and size. When want is set the write is discarded on mismatch.
func writeFileFrom(dst string, src io.Reader, want string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want != "" && sum != want {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("content hash mismatch for %s", filepath.Base(dst))
	}
	return sum, n, os.Rename(tmp, dst)
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
	return writeAtomic(p, b)
}

// FileSize formats a byte count for humans.
func FileSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- appending --------------------------------------------------------------

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

// Append adds text to the end of a text file (creating it if needed) as this node.
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
	var existing []byte
	if m := s.idx.Entries[name]; m != nil && !m.Deleted {
		data, err := s.Read(name)
		if err != nil {
			return err
		}
		if !merge.IsText(data) {
			return fmt.Errorf("%s is not a text file", name)
		}
		existing = data
	}
	return s.WriteBy(name, Join(string(existing), text), by)
}
