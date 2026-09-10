package proto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

// File API:
//
//	GET /np/v1/files          -> []store.FileMeta
//	GET /np/v1/files/{name}   -> raw content, X-Np-Meta header (404 when not held)
//	PUT /np/v1/files/{name}   <- X-Np-Meta header + raw content (empty body when
//	                             meta.Have is false: metadata only), -> Apply
//
// Content is streamed, never held in memory, and the node lock is only taken
// around the store update, so a large transfer does not stall the web UI.

const metaHeader = "X-Np-Meta"

// fileClient has no overall timeout: transfers can be large. Dial and
// header timeouts still apply through the default transport.
var fileClient = &http.Client{}

func (n *Node) mountFiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /np/v1/files", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		n.Store.Scan()
		writeJSON(w, n.filesIndex())
	}))
	mux.HandleFunc("GET /np/v1/files/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		name := r.PathValue("name")
		n.Lock()
		n.Store.Scan()
		m := n.Store.File(name)
		var meta store.FileMeta
		if m != nil {
			meta = *m
		}
		f, err := n.Store.OpenFile(name)
		n.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()
		mj, _ := json.Marshal(meta)
		w.Header().Set(metaHeader, string(mj))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		io.Copy(w, f)
	}))
	mux.HandleFunc("PUT /np/v1/files/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
		var meta store.FileMeta
		if err := json.Unmarshal([]byte(r.Header.Get(metaHeader)), &meta); err != nil {
			http.Error(w, "bad "+metaHeader+": "+err.Error(), http.StatusBadRequest)
			return
		}
		if meta.Name != r.PathValue("name") {
			http.Error(w, "name mismatch", http.StatusBadRequest)
			return
		}
		var src io.Reader
		if meta.Have && !meta.Deleted {
			// Spool to disk before taking the lock.
			t, err := n.spool(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer os.Remove(t)
			f, err := os.Open(t)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer f.Close()
			src = f
		}
		n.Lock()
		res, err := n.Store.ApplyFile(meta, src)
		n.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.logf("%s pushed file %s: %s", peer.Name, meta.Name, res)
		if res == store.Accepted || res == store.Conflicted {
			n.NotifyChanged()
		}
		writeJSON(w, Apply{Result: res})
	}))
}

func (n *Node) filesIndex() []store.FileMeta {
	files := n.Store.Files(true)
	out := make([]store.FileMeta, len(files))
	for i, m := range files {
		out[i] = *m
	}
	return out
}

// spool copies r into a temp file under the files directory (dot-prefixed,
// so scans ignore it) and returns its path.
func (n *Node) spool(r io.Reader) (string, error) {
	f, err := os.CreateTemp(filepath.Join(n.Store.Dir, "files"), ".incoming-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// RemoteFiles fetches a peer's file index keyed by name.
func (n *Node) RemoteFiles(ctx context.Context, p ts.Peer) (map[string]store.FileMeta, error) {
	var remote []store.FileMeta
	if err := do(ctx, http.MethodGet, n.base(p)+"/files", nil, &remote); err != nil {
		return nil, fmt.Errorf("fetch file index from %s: %w", p.Name, err)
	}
	out := make(map[string]store.FileMeta, len(remote))
	for _, m := range remote {
		out[m.Name] = m
	}
	return out, nil
}

// download streams a peer's copy of name into a spool file and returns its
// path with the metadata the peer sent.
func (n *Node) download(ctx context.Context, p ts.Peer, name string) (string, store.FileMeta, error) {
	var meta store.FileMeta
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base(p)+"/files/"+name, nil)
	if err != nil {
		return "", meta, err
	}
	resp, err := fileClient.Do(req)
	if err != nil {
		return "", meta, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", meta, fmt.Errorf("GET %s: %s: %s", name, resp.Status, string(msg))
	}
	if err := json.Unmarshal([]byte(resp.Header.Get(metaHeader)), &meta); err != nil {
		return "", meta, fmt.Errorf("GET %s: bad metadata: %w", name, err)
	}
	tmp, err := n.spool(resp.Body)
	return tmp, meta, err
}

// upload pushes name (content when held, metadata otherwise) to a peer.
func (n *Node) upload(ctx context.Context, p ts.Peer, meta store.FileMeta) (store.ApplyResult, error) {
	var body io.Reader
	var size int64 = 0
	if meta.Have && !meta.Deleted {
		n.Lock()
		f, err := n.Store.OpenFile(meta.Name)
		n.Unlock()
		if err != nil {
			return "", err
		}
		defer f.Close()
		body, size = f, meta.Size
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, n.base(p)+"/files/"+meta.Name, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	mj, _ := json.Marshal(meta)
	req.Header.Set(metaHeader, string(mj))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := fileClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("PUT %s: %s: %s", meta.Name, resp.Status, string(msg))
	}
	var ack Apply
	return ack.Result, json.NewDecoder(resp.Body).Decode(&ack)
}

// wantContent decides whether to pull the bytes of a remote version now.
func (n *Node) wantContent(local *store.FileMeta, remote store.FileMeta) bool {
	if !remote.Have || remote.Deleted {
		return false
	}
	if n.Store.Config.KeepAll || (local != nil && local.Have && !local.Deleted) {
		return true
	}
	limit := n.Store.Config.AutoFetchLimit()
	return limit >= 0 && remote.Size <= limit
}

// syncFiles is the file phase of Sync: pull what the peer has newer (bytes
// or just metadata), then push what we have newer.
func (n *Node) syncFiles(ctx context.Context, p ts.Peer, rep *SyncReport) error {
	remoteBy, err := n.RemoteFiles(ctx, p)
	if errors.Is(err, errNotFound) {
		// The peer runs an np without file support; notes still synced.
		n.Lock()
		haveFiles := len(n.Store.Files(false)) > 0
		n.Unlock()
		if haveFiles {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s does not sync files yet (upgrade it)", p.Name))
		}
		return nil
	}
	if err != nil {
		return err
	}
	type pull struct {
		meta    store.FileMeta
		content bool
	}
	var pulls []pull
	n.Lock()
	if _, err := n.Store.Scan(); err != nil {
		n.Unlock()
		return err
	}
	for name, rm := range remoteBy {
		lm := n.Store.File(name)
		if lm != nil {
			switch clock.Compare(rm.Clock, lm.Clock) {
			case clock.Equal:
				if lm.Have || lm.Deleted || !n.wantContent(lm, rm) {
					continue
				}
			case clock.Dominated:
				continue
			}
			if !rm.Have && !rm.Deleted && lm.Have && !lm.Deleted {
				continue // metadata alone never replaces content we hold
			}
		}
		pulls = append(pulls, pull{rm, n.wantContent(lm, rm) && !n.Store.CanCarry(rm)})
	}
	n.Unlock()

	for _, pl := range pulls {
		name := pl.meta.Name
		var src io.Reader
		if pl.content {
			tmp, meta, err := n.download(ctx, p, name)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("pull file %s: %v", name, err))
				continue
			}
			f, err := os.Open(tmp)
			if err != nil {
				os.Remove(tmp)
				rep.Errors = append(rep.Errors, fmt.Sprintf("pull file %s: %v", name, err))
				continue
			}
			pl.meta = meta
			src = f
			defer os.Remove(tmp)
			defer f.Close()
		}
		n.Lock()
		res, err := n.Store.ApplyFile(pl.meta, src)
		n.Unlock()
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("apply file %s: %v", name, err))
			continue
		}
		switch res {
		case store.Accepted, store.Conflicted:
			rep.Pulled = append(rep.Pulled, "files/"+name)
			if res == store.Conflicted {
				rep.Conflicts = append(rep.Conflicts, "files/"+name)
			}
			if !pl.content && !pl.meta.Deleted {
				rep.NotFetched = append(rep.NotFetched, "files/"+name)
			}
		}
	}

	n.Lock()
	var pushes []store.FileMeta
	for _, lm := range n.Store.Files(true) {
		rm, ok := remoteBy[lm.Name]
		if ok {
			c := clock.Compare(lm.Clock, rm.Clock)
			if c == clock.Equal && lm.Have && !rm.Have && !lm.Deleted && n.peerWants(rm) {
				pushes = append(pushes, *lm) // fill the peer's stub
				continue
			}
			if c != clock.Dominates {
				continue
			}
			if !lm.Have && !lm.Deleted && rm.Have && !rm.Deleted {
				continue // we only know about it; the peer holds older bytes
			}
		}
		pushes = append(pushes, *lm)
	}
	n.Unlock()
	for _, lm := range pushes {
		res, err := n.upload(ctx, p, lm)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("push file %s: %v", lm.Name, err))
			continue
		}
		if res == store.Accepted || res == store.Conflicted {
			rep.Pushed = append(rep.Pushed, "files/"+lm.Name)
		}
	}
	return nil
}

// peerWants guesses whether a peer that only holds metadata would take the
// bytes: we cannot see its config, so only fill stubs the size we would
// fetch ourselves; larger content waits until that peer asks.
func (n *Node) peerWants(rm store.FileMeta) bool {
	limit := n.Store.Config.AutoFetchLimit()
	return limit >= 0 && rm.Size <= limit
}

// FetchFile pulls the content of a file this node only knows about, from the
// hub or any online peer that holds it.
func (n *Node) FetchFile(ctx context.Context, name string) (ts.Peer, error) {
	n.Lock()
	m := n.Store.File(name)
	n.Unlock()
	if m == nil || m.Deleted {
		return ts.Peer{}, fmt.Errorf("no file %q", name)
	}
	if m.Have {
		return ts.Peer{}, nil
	}
	peers, err := ts.Peers(ctx)
	if err != nil {
		return ts.Peer{}, err
	}
	// Hub first.
	hub := n.Store.Config.Hub
	for i, p := range peers {
		if p.Name == hub && i > 0 {
			peers[0], peers[i] = peers[i], peers[0]
		}
	}
	var lastErr error = errors.New("no online peer holds " + name)
	for _, p := range peers {
		if p.Self || !p.Online {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		remote, err := n.RemoteFiles(pctx, p)
		cancel()
		if err != nil {
			continue
		}
		rm, ok := remote[name]
		if !ok || !rm.Have || rm.Deleted || rm.Hash != m.Hash {
			continue
		}
		if err := n.FetchFileFrom(ctx, p, name); err != nil {
			lastErr = err
			continue
		}
		return p, nil
	}
	return ts.Peer{}, lastErr
}

// FetchFileFrom downloads name from p and fills the local stub.
func (n *Node) FetchFileFrom(ctx context.Context, p ts.Peer, name string) error {
	tmp, _, err := n.download(ctx, p, name)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	n.Lock()
	defer n.Unlock()
	return n.Store.FillFile(name, f)
}

// CompareFiles classifies local files against a peer's file index; files
// only the peer has are returned as Behind.
func CompareFiles(local []*store.FileMeta, remote map[string]store.FileMeta) map[string]SyncState {
	out := make(map[string]SyncState, len(local))
	seen := map[string]bool{}
	for _, lm := range local {
		seen[lm.Name] = true
		rm, ok := remote[lm.Name]
		if !ok {
			out[lm.Name] = New
			continue
		}
		out[lm.Name] = state(clock.Compare(lm.Clock, rm.Clock))
	}
	for name, rm := range remote {
		if !seen[name] && !rm.Deleted {
			out[name] = Behind
		}
	}
	return out
}
