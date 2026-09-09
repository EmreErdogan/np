package store

// Files are arbitrary content (images, video, archives) stored under
// <dir>/files/<name>. They sync like notes (vector clocks, tombstones,
// conflict copies) but keep no version history, and content travels lazily:
// every peer learns that a file exists; the bytes only follow when the file
// is small enough (Config.AutoFetch), the peer keeps everything
// (Config.KeepAll), it already held an earlier version, or the user asks.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
)

// FileMeta is the synchronised state of one file. Have is per node: whether
// this node holds the content for the version described by Hash.
type FileMeta struct {
	Name    string      `json:"name"`
	Clock   clock.Clock `json:"clock"`
	Hash    string      `json:"hash"`
	Size    int64       `json:"size"`
	ModTime time.Time   `json:"mtime"`
	ModBy   string      `json:"by"`
	Deleted bool        `json:"deleted,omitempty"`
	Have    bool        `json:"have"`
}

// DefaultAutoFetch is the size up to which file content is pulled without
// being asked (2 MiB). Config.AutoFetch < 0 disables automatic fetching.
const DefaultAutoFetch = 2 << 20

// ValidFileName rejects names that could escape the files directory.
func ValidFileName(name string) error {
	if name == "" {
		return errors.New("empty file name")
	}
	if strings.HasSuffix(name, ".tmp") {
		return fmt.Errorf("invalid file name %q (.tmp is reserved)", name)
	}
	if err := validSegments(name); err != nil {
		return fmt.Errorf("invalid file name %q", name)
	}
	return nil
}

func (s *Store) filePath(name string) string {
	return filepath.Join(s.Dir, "files", filepath.FromSlash(name))
}

// FileLocalPath is the absolute working path of a file (no dirs created).
func (s *Store) FileLocalPath(name string) string { return s.filePath(name) }

// FilePath returns the working path of a file, creating parent dirs.
func (s *Store) FilePath(name string) (string, error) {
	if err := ValidFileName(name); err != nil {
		return "", err
	}
	p := s.filePath(name)
	return p, os.MkdirAll(filepath.Dir(p), 0o755)
}

// File returns metadata for a file (including tombstones and stubs), or nil.
func (s *Store) File(name string) *FileMeta { return s.idx.Files[name] }

// Files returns all file metadata sorted by name.
func (s *Store) Files(withDeleted bool) []*FileMeta {
	out := make([]*FileMeta, 0, len(s.idx.Files))
	for _, m := range s.idx.Files {
		if m.Deleted && !withDeleted {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OpenFile opens the content of a file this node holds.
func (s *Store) OpenFile(name string) (*os.File, error) {
	m := s.idx.Files[name]
	if m == nil || m.Deleted {
		return nil, fmt.Errorf("no file %q", name)
	}
	if !m.Have {
		return nil, fmt.Errorf("%s is not fetched on this machine (np get %s)", name, name)
	}
	return os.Open(s.filePath(name))
}

// scanFiles records files added, changed or removed under files/ as this
// node's versions. Unchanged files are recognised by size and mtime so
// large files are hashed once. Missing stubs are not deletions.
func (s *Store) scanFiles() ([]string, error) {
	root := filepath.Join(s.Dir, "files")
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
		name := filepath.ToSlash(rel)
		if ValidFileName(name) != nil {
			return nil
		}
		seen[name] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		m := s.idx.Files[name]
		if m != nil && !m.Deleted && m.Have && m.Size == info.Size() && m.ModTime.Equal(info.ModTime().UTC()) {
			return nil
		}
		h, err := hashFile(p)
		if err != nil {
			return err
		}
		if m != nil && !m.Deleted && m.Hash == h {
			// Same content (re-fetched, touched, or copied back): no new version.
			m.Have, m.Size, m.ModTime = true, info.Size(), info.ModTime().UTC()
			return nil
		}
		s.commitFile(name, h, info.Size(), info.ModTime(), false)
		changed = append(changed, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name, m := range s.idx.Files {
		if !seen[name] && !m.Deleted && m.Have {
			s.commitFile(name, "", 0, time.Now(), true)
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// commitFile records a local version (clock bumped by this node).
func (s *Store) commitFile(name, hash string, size int64, mt time.Time, deleted bool) {
	m := s.idx.Files[name]
	if m == nil {
		m = &FileMeta{Name: name, Clock: clock.Clock{}}
		s.idx.Files[name] = m
	}
	m.Clock = m.Clock.Bump(s.Node)
	m.Hash = hash
	m.Size = size
	m.ModTime = mt.UTC()
	m.ModBy = s.Node
	if s.author != "" {
		m.ModBy = s.author
	}
	m.Deleted = deleted
	m.Have = !deleted
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

// PutFile stores src under name as this node's version, attributed to by.
func (s *Store) PutFile(name string, src io.Reader, by string) error {
	p, err := s.FilePath(name)
	if err != nil {
		return err
	}
	if _, _, err := writeFileFrom(p, src, ""); err != nil {
		return err
	}
	return s.scanAs(by)
}

// DeleteFile removes a file (content or stub) and records a tombstone.
func (s *Store) DeleteFile(name, by string) error {
	if err := ValidFileName(name); err != nil {
		return err
	}
	m := s.idx.Files[name]
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.author = by
	s.commitFile(name, "", 0, time.Now(), true)
	s.author = ""
	return s.saveIndex()
}

// DropFile removes the local content of a file but keeps knowing about it,
// so it can be fetched again later. No version is recorded.
func (s *Store) DropFile(name string) error {
	m := s.idx.Files[name]
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if !m.Have {
		return nil
	}
	if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	m.Have = false
	return s.saveIndex()
}

// FillFile stores the content of a stub, verifying it against the hash the
// index already knows.
func (s *Store) FillFile(name string, src io.Reader) error {
	m := s.idx.Files[name]
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if m.Have {
		return nil
	}
	if err := s.installFileContent(m, src); err != nil {
		return err
	}
	return s.saveIndex()
}

func (s *Store) installFileContent(m *FileMeta, src io.Reader) error {
	p, err := s.FilePath(m.Name)
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
	m.Size, m.Have = n, true
	return nil
}

// ApplyFile merges a file received from a peer. src carries the content
// when the sender included it; nil means metadata only. Metadata alone never
// replaces content this node holds: such an update is Rejected and the
// caller should try a peer that has the bytes.
func (s *Store) ApplyFile(remote FileMeta, src io.Reader) (ApplyResult, error) {
	if err := ValidFileName(remote.Name); err != nil {
		return Rejected, err
	}
	if _, err := s.Scan(); err != nil {
		return Rejected, err
	}
	name := remote.Name
	local := s.idx.Files[name]
	cmp := clock.Dominates
	if local != nil {
		cmp = clock.Compare(remote.Clock, local.Clock)
	}
	switch cmp {
	case clock.Equal:
		if local.Hash == remote.Hash && local.Deleted == remote.Deleted {
			if !local.Have && !local.Deleted && src != nil {
				if err := s.installFileContent(local, src); err != nil {
					return Rejected, err
				}
				return Accepted, s.saveIndex()
			}
			return Unchanged, nil
		}
		return Rejected, nil
	case clock.Dominated:
		return Rejected, nil
	case clock.Dominates:
		if src == nil && !remote.Deleted && local != nil && local.Have && !local.Deleted {
			return Rejected, nil
		}
		if err := s.installFile(remote, src); err != nil {
			return Rejected, err
		}
		return Accepted, s.saveIndex()
	}
	// Concurrent with identical content (the same bytes arrived by two
	// routes): reconcile the clocks, nothing to record.
	if local.Hash == remote.Hash && local.Deleted == remote.Deleted {
		local.Clock = clock.Merge(local.Clock, remote.Clock)
		if !local.Have && !local.Deleted && src != nil {
			if err := s.installFileContent(local, src); err != nil {
				return Rejected, err
			}
			return Accepted, s.saveIndex()
		}
		return Unchanged, s.saveIndex()
	}
	// Concurrent: newer mtime wins; the loser's content, when present, is
	// kept as a conflict copy.
	remoteWins := remote.ModTime.After(local.ModTime) ||
		(remote.ModTime.Equal(local.ModTime) && remote.ModBy > local.ModBy)
	merged := clock.Merge(local.Clock, remote.Clock)
	if remoteWins {
		if local.Have && !local.Deleted {
			loser := conflictName(name, local.ModBy, local.ModTime)
			lp, err := s.FilePath(loser)
			if err != nil {
				return Rejected, err
			}
			if err := os.Rename(s.filePath(name), lp); err != nil {
				return Rejected, err
			}
		}
		if err := s.installFile(remote, src); err != nil {
			return Rejected, err
		}
	} else if src != nil && !remote.Deleted {
		lp, err := s.FilePath(conflictName(name, remote.ModBy, remote.ModTime))
		if err != nil {
			return Rejected, err
		}
		if _, _, err := writeFileFrom(lp, src, remote.Hash); err != nil {
			return Rejected, err
		}
	}
	m := s.idx.Files[name]
	m.Clock = merged.Bump(s.Node)
	if _, err := s.Scan(); err != nil { // records the conflict copy
		return Rejected, err
	}
	return Conflicted, s.saveIndex()
}

func conflictName(name, by string, t time.Time) string {
	return fmt.Sprintf("%s.conflict-%s-%s", name, by, t.Format("20060102-150405"))
}

// installFile writes remote's metadata (and content when given) as-is.
func (s *Store) installFile(remote FileMeta, src io.Reader) error {
	name := remote.Name
	m := s.idx.Files[name]
	if m == nil {
		m = &FileMeta{Name: name}
		s.idx.Files[name] = m
	}
	m.Clock = remote.Clock.Copy()
	m.Hash = remote.Hash
	m.Size = remote.Size
	m.ModTime = remote.ModTime.UTC()
	m.ModBy = remote.ModBy
	m.Deleted = remote.Deleted
	m.Have = false
	if remote.Deleted || src == nil {
		// Tombstone or stub: whatever is on disk is an older version.
		if err := os.Remove(s.filePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return s.installFileContent(m, src)
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
