package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

// History API (see .docs/design-history-sync.md):
//
//	GET /np/v2/history/{name}          -> []store.Version
//	PUT /np/v2/history/{name}          <- []store.Version, -> {"added": n}
//	GET /np/v2/snapshot/{hash}/{name}  -> raw content
//	PUT /np/v2/snapshot/{hash}/{name}  <- raw content (verified against hash)
//
// Blob versions are ledger rows without a snapshot.

func (n *Node) mountHistory(mux *http.ServeMux) {
	mux.HandleFunc("GET /np/v2/history/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		m := n.Store.Get(r.PathValue("name"))
		if m == nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		writeJSON(w, m.History)
	}))
	mux.HandleFunc("PUT /np/v2/history/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
		var vs []store.Version
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&vs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.Lock()
		added, err := n.Store.AddVersions(r.PathValue("name"), vs)
		n.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if added > 0 {
			n.logf("%s pushed %d historical version(s) of %s", peer.Name, added, r.PathValue("name"))
		}
		writeJSON(w, map[string]int{"added": added})
	}))
	mux.HandleFunc("GET /np/v2/snapshot/{hash}/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		name, hash := r.PathValue("name"), r.PathValue("hash")
		if err := store.ValidName(name); err != nil || !validHash(hash) {
			http.Error(w, "bad snapshot address", http.StatusBadRequest)
			return
		}
		data, err := n.Store.Snapshot(name, hash) // snapshots are immutable files: no lock needed
		if err != nil {
			http.Error(w, "no such snapshot", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	}))
	mux.HandleFunc("PUT /np/v2/snapshot/{hash}/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		name, hash := r.PathValue("name"), r.PathValue("hash")
		if !validHash(hash) {
			http.Error(w, "bad snapshot address", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.Lock()
		err = n.Store.PutSnapshot(name, hash, data)
		n.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// validHash accepts only what a content hash looks like, so the value can
// be used as a file name.
func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// raw performs a request with a raw body and returns the response body.
func raw(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, bytes.TrimSpace(msg))
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", errNotFound, err)
		}
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// syncHistory unions the ledgers of every file whose digest differs from
// the peer's: pull the versions (and snapshots) we lack, push the ones the
// peer lacks. Runs after the content phase, so every remote file already
// has a local entry. The node lock is held only around store access.
func (n *Node) syncHistory(ctx context.Context, p ts.Peer, rep *SyncReport) error {
	remoteBy, err := n.RemoteIndex(ctx, p)
	if err != nil {
		return err
	}
	var names []string
	n.Lock()
	for name, rm := range remoteBy {
		lm := n.Store.Get(name)
		if lm == nil || rm.HistoryDigest == "" || rm.HistoryDigest == store.HistoryDigest(lm) {
			continue
		}
		names = append(names, name)
	}
	n.Unlock()
	sort.Strings(names)
	for _, name := range names {
		if err := n.syncLedger(ctx, p, name, rep); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("history %s: %v", name, err))
		}
	}
	return nil
}

func (n *Node) syncLedger(ctx context.Context, p ts.Peer, name string, rep *SyncReport) error {
	base := n.base(p)
	var remote []store.Version
	if err := do(ctx, http.MethodGet, base+"/history/"+esc(name), nil, &remote); err != nil {
		return err
	}
	n.Lock()
	here, there := n.Store.MissingVersions(name, remote)
	var fetch []store.Version
	for _, v := range here {
		if !v.Deleted && !v.Blob && !n.Store.HasSnapshot(name, v.Hash) {
			fetch = append(fetch, v)
		}
	}
	n.Unlock()

	// Pull: content first, then the rows whose content arrived.
	got := map[string][]byte{}
	var errs []error
	for _, v := range fetch {
		if _, dup := got[v.Hash]; dup {
			continue
		}
		data, err := raw(ctx, http.MethodGet, base+"/snapshot/"+v.Hash+"/"+esc(name), nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot %.8s: %w", v.Hash, err))
			continue
		}
		got[v.Hash] = data
	}
	n.Lock()
	for hash, data := range got {
		if err := n.Store.PutSnapshot(name, hash, data); err != nil {
			errs = append(errs, fmt.Errorf("snapshot %.8s: %w", hash, err))
		}
	}
	// AddVersions skips rows whose snapshot is still missing, so a failed
	// fetch leaves the digests different and the next sync retries.
	added, err := n.Store.AddVersions(name, here)
	var ledger []store.Version
	if m := n.Store.Get(name); m != nil {
		ledger = append(ledger, m.History...)
	}
	n.Unlock()
	if err != nil {
		errs = append(errs, err)
	}
	rep.Versions += added

	// Push.
	if len(there) > 0 {
		ok := true
		for _, v := range there {
			if v.Deleted || v.Blob {
				continue
			}
			data, err := n.Store.Snapshot(name, v.Hash)
			if err != nil {
				continue // nothing to give; the peer will skip this row
			}
			if _, err := raw(ctx, http.MethodPut, base+"/snapshot/"+v.Hash+"/"+esc(name), data); err != nil {
				errs = append(errs, fmt.Errorf("push snapshot %.8s: %w", v.Hash, err))
				ok = false
				break
			}
		}
		if ok {
			var ack struct {
				Added int `json:"added"`
			}
			if err := do(ctx, http.MethodPut, base+"/history/"+esc(name), ledger, &ack); err != nil {
				errs = append(errs, fmt.Errorf("push: %w", err))
			}
			rep.VersionsPushed += ack.Added
		}
	}
	return errors.Join(errs...)
}
