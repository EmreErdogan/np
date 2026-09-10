package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

// History API (see .docs/design-history-sync.md):
//
//	GET /np/v1/history/{name}          -> []store.Version
//	PUT /np/v1/history/{name}          <- []store.Version, -> {"added": n}
//	GET /np/v1/snapshot/{hash}/{name}  -> raw content
//	PUT /np/v1/snapshot/{hash}/{name}  <- raw content (verified against hash)

func (n *Node) mountHistory(mux *http.ServeMux) {
	mux.HandleFunc("GET /np/v1/history/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		m := n.Store.Get(r.PathValue("name"))
		if m == nil {
			http.Error(w, "no such note", http.StatusNotFound)
			return
		}
		writeJSON(w, m.History)
	}))
	mux.HandleFunc("PUT /np/v1/history/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
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
	mux.HandleFunc("GET /np/v1/snapshot/{hash}/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		name, hash := r.PathValue("name"), r.PathValue("hash")
		if err := store.ValidName(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.Lock()
		data, err := n.Store.Snapshot(name, hash)
		n.Unlock()
		if err != nil {
			http.Error(w, "no such snapshot", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	}))
	mux.HandleFunc("PUT /np/v1/snapshot/{hash}/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		name, hash := r.PathValue("name"), r.PathValue("hash")
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

// syncHistory unions the ledgers of every note whose digest differs from
// the peer's: pull the versions (and snapshots) we lack, push the ones the
// peer lacks. Runs after the content phase, so every remote note already
// has a local Meta.
func (n *Node) syncHistory(ctx context.Context, p ts.Peer, rep *SyncReport) error {
	base := n.base(p)
	remoteBy, err := n.RemoteIndex(ctx, p)
	if err != nil {
		return err
	}
	n.Lock()
	defer n.Unlock()
	for name, rm := range remoteBy {
		lm := n.Store.Get(name)
		if lm == nil || rm.HistoryDigest == "" || rm.HistoryDigest == store.HistoryDigest(lm) {
			continue // older peer (no digest) or identical ledgers
		}
		var remote []store.Version
		if err := do(ctx, http.MethodGet, base+"/history/"+name, nil, &remote); err != nil {
			if errors.Is(err, errNotFound) {
				return nil // older peer without the history API
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("history %s: %v", name, err))
			continue
		}
		here, there := n.Store.MissingVersions(name, remote)
		// Pull.
		var ready []store.Version
		for _, v := range here {
			if !v.Deleted && !n.Store.HasSnapshot(name, v.Hash) {
				data, err := raw(ctx, http.MethodGet, base+"/snapshot/"+v.Hash+"/"+name, nil)
				if err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("snapshot %s@%s: %v", name, v.Hash[:8], err))
					continue
				}
				if err := n.Store.PutSnapshot(name, v.Hash, data); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("snapshot %s@%s: %v", name, v.Hash[:8], err))
					continue
				}
			}
			ready = append(ready, v)
		}
		if len(ready) > 0 {
			added, err := n.Store.AddVersions(name, ready)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("history %s: %v", name, err))
				continue
			}
			rep.Versions += added
		}
		// Push.
		if len(there) == 0 {
			continue
		}
		ok := true
		for _, v := range there {
			if v.Deleted {
				continue
			}
			data, err := n.Store.Snapshot(name, v.Hash)
			if err != nil {
				continue // nothing to give; the peer will skip this entry
			}
			if _, err := raw(ctx, http.MethodPut, base+"/snapshot/"+v.Hash+"/"+name, data); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("push snapshot %s@%s: %v", name, v.Hash[:8], err))
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		var ack struct {
			Added int `json:"added"`
		}
		if err := do(ctx, http.MethodPut, base+"/history/"+name, lm.History, &ack); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("push history %s: %v", name, err))
			continue
		}
		rep.VersionsPushed += ack.Added
	}
	return nil
}
