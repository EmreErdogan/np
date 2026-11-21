// Package proto implements np's sync protocol: a small HTTP API served only
// on the machine's Tailscale address. Authentication is the tailnet itself;
// every request is attributed with Tailscale WhoIs.
//
//	GET /np/v1/ping                    -> Ping (same path in every protocol version)
//	GET /np/v2/index                   -> []store.Meta (no ledger, with hdigest)
//	GET /np/v2/files/{name}            -> raw content, X-Np-Meta header (404 when not held)
//	PUT /np/v2/files/{name}            <- X-Np-Meta header + raw content (empty body when
//	                                      meta.Have is false: metadata only), -> Apply
//	GET|PUT /np/v2/history/{name}      ledger union (history.go)
//	GET|PUT /np/v2/snapshot/{hash}/{name}
//
// Content is streamed, never held in memory, and the node lock is only taken
// around store updates, so a large or slow transfer does not stall the web UI.
package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

// Version is the sync protocol this binary speaks. Peers on another
// version are told to upgrade instead of being synced with.
const Version = "2"

const metaHeader = "X-Np-Meta"

type Ping struct {
	Node    string `json:"node"`
	Version string `json:"version"`
	Notes   int    `json:"notes"`
}

type Apply struct {
	Result store.ApplyResult `json:"result"`
}

// Node is a store plus the identity it runs as; all access is serialised.
type Node struct {
	Store *store.Store
	Self  ts.Identity
	mu    sync.Mutex
	Log   *log.Logger
	// WhoIs identifies callers; defaults to ts.WhoIs. Tests override it.
	WhoIs func(ctx context.Context, remoteAddr string) (ts.Peer, error)
	// Changed receives a signal whenever a peer pushes a change to us, so a
	// daemon can fan the change out to other peers.
	Changed chan struct{}
	// Mount registers extra routes (the web UI) on the served mux.
	Mount []func(mux *http.ServeMux)
}

// NotifyChanged signals Changed without blocking.
func (n *Node) NotifyChanged() {
	if n.Changed == nil {
		return
	}
	select {
	case n.Changed <- struct{}{}:
	default:
	}
}

func (n *Node) Lock()   { n.mu.Lock() }
func (n *Node) Unlock() { n.mu.Unlock() }

// Logf logs through the node's logger when one is set.
func (n *Node) Logf(format string, a ...any) { n.logf(format, a...) }

func (n *Node) logf(format string, a ...any) {
	if n.Log != nil {
		n.Log.Printf(format, a...)
	}
}

// ---- server -----------------------------------------------------------------

func (n *Node) allowed(p ts.Peer) bool {
	if p.Login != "" && p.Login == n.Self.Login {
		return true
	}
	for _, a := range n.Store.Config.Allow {
		if strings.EqualFold(a, p.Login) {
			return true
		}
	}
	return false
}

// Auth wraps a handler so it only runs for callers the tailnet identifies
// as allowed; the identified peer is passed along.
func (n *Node) Auth(next func(w http.ResponseWriter, r *http.Request, peer ts.Peer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		whois := n.WhoIs
		if whois == nil {
			whois = ts.WhoIs
		}
		peer, err := whois(ctx, r.RemoteAddr)
		if err != nil {
			http.Error(w, "whois failed: "+err.Error(), http.StatusForbidden)
			return
		}
		if !n.allowed(peer) {
			n.logf("denied %s (%s) from %s", peer.Name, peer.Login, r.RemoteAddr)
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		next(w, r, peer)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Handler returns the HTTP API.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /np/v1/ping", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		writeJSON(w, Ping{Node: n.Self.Name, Version: Version, Notes: len(n.Store.List(false))})
	}))
	mux.HandleFunc("GET /np/v2/index", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		if _, err := n.Store.Scan(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, n.index())
	}))
	mux.HandleFunc("GET /np/v2/files/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		name := r.PathValue("name")
		n.Lock()
		n.Store.Scan()
		var meta store.Meta
		if m := n.Store.Get(name); m != nil {
			meta = m.Wire()
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
	mux.HandleFunc("PUT /np/v2/files/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
		var meta store.Meta
		if err := json.Unmarshal([]byte(r.Header.Get(metaHeader)), &meta); err != nil {
			http.Error(w, "bad "+metaHeader+": "+err.Error(), http.StatusBadRequest)
			return
		}
		if meta.Name != r.PathValue("name") {
			http.Error(w, "name mismatch", http.StatusBadRequest)
			return
		}
		meta = meta.Wire()
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
		res, err := n.Store.Apply(meta, src)
		n.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.logf("%s pushed %s: %s", peer.Name, meta.Name, res)
		if res == store.Accepted || res == store.Merged || res == store.Conflicted {
			n.NotifyChanged()
		}
		writeJSON(w, Apply{Result: res})
	}))
	n.mountHistory(mux)
	for _, m := range n.Mount {
		m(mux)
	}
	return mux
}

func (n *Node) index() []store.Meta {
	all := n.Store.List(true)
	out := make([]store.Meta, 0, len(all))
	for _, m := range all {
		c := m.Wire()
		c.HistoryDigest = store.HistoryDigest(m)
		out = append(out, c)
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

// Serve listens on the Tailscale address only and blocks until ctx is done.
func (n *Node) Serve(ctx context.Context) error {
	if !n.Self.IP.IsValid() {
		return errors.New("no tailscale IP to listen on")
	}
	addr := netip.AddrPortFrom(n.Self.IP, uint16(n.Store.Config.Port)).String()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: n.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	n.logf("np %s listening on http://%s", n.Self.Name, addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- client -----------------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

// fileClient has no overall timeout: transfers can be large. Dial and
// header timeouts still apply through the default transport.
var fileClient = &http.Client{}

// errNotFound wraps 404 responses so callers can tell a missing route (an
// np with another protocol version on the peer) from other failures.
var errNotFound = errors.New("not found")

func (n *Node) addr(p ts.Peer) string {
	return "http://" + netip.AddrPortFrom(p.IP, uint16(n.Store.Config.Port)).String()
}

func (n *Node) base(p ts.Peer) string { return n.addr(p) + "/np/v2" }

// esc escapes a file name for use in a URL path, keeping the slashes.
func esc(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", errNotFound, err)
		}
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// PingPeer checks whether a peer runs np.
func (n *Node) PingPeer(ctx context.Context, p ts.Peer) (Ping, error) {
	var out Ping
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return out, do(ctx, http.MethodGet, n.addr(p)+"/np/v1/ping", nil, &out)
}

// SyncReport summarises one sync run.
type SyncReport struct {
	Peer           string
	Pulled         []string
	Pushed         []string
	Merged         []string // pulled text files that were three-way merged with a local edit
	NotFetched     []string // files whose metadata arrived without content
	Versions       int      // historical versions pulled
	VersionsPushed int
	Conflicts      []string
	Errors         []string
}

func (r SyncReport) String() string {
	s := fmt.Sprintf("%s: pulled %d, pushed %d", r.Peer, len(r.Pulled), len(r.Pushed))
	if len(r.Merged) > 0 {
		s += fmt.Sprintf(", %d merged", len(r.Merged))
	}
	if len(r.NotFetched) > 0 {
		s += fmt.Sprintf(", %d not fetched", len(r.NotFetched))
	}
	if r.Versions+r.VersionsPushed > 0 {
		s += fmt.Sprintf(", history +%d/-%d", r.Versions, r.VersionsPushed)
	}
	if len(r.Conflicts) > 0 {
		s += fmt.Sprintf(", %d conflict(s)", len(r.Conflicts))
	}
	if len(r.Errors) > 0 {
		s += fmt.Sprintf(", %d error(s)", len(r.Errors))
	}
	return s
}

// Detail is String plus the file names, for logs.
func (r SyncReport) Detail() string {
	s := r.String()
	if len(r.Pulled) > 0 {
		s += " <- " + strings.Join(r.Pulled, ",")
	}
	if len(r.Pushed) > 0 {
		s += " -> " + strings.Join(r.Pushed, ",")
	}
	for _, e := range r.Errors {
		s += "; " + e
	}
	return s
}

// Sync exchanges files with a peer in both directions: pull what the peer
// has newer or concurrent (merging locally), push what we have newer,
// including merge results, then union the ledgers.
func (n *Node) Sync(ctx context.Context, p ts.Peer) (SyncReport, error) {
	rep := SyncReport{Peer: p.Name}
	if err := n.syncFiles(ctx, p, &rep); err != nil {
		return rep, err
	}
	if err := n.syncHistory(ctx, p, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// RemoteIndex fetches a peer's index keyed by name.
func (n *Node) RemoteIndex(ctx context.Context, p ts.Peer) (map[string]store.Meta, error) {
	var remote []store.Meta
	if err := do(ctx, http.MethodGet, n.base(p)+"/index", nil, &remote); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("%s runs an np with another sync protocol (this one speaks %s); run `np upgrade` on both", p.Name, Version)
		}
		return nil, fmt.Errorf("fetch index from %s: %w", p.Name, err)
	}
	out := make(map[string]store.Meta, len(remote))
	for _, m := range remote {
		out[m.Name] = m
	}
	return out, nil
}

// download streams a peer's copy of name into a spool file and returns its
// path with the metadata the peer sent.
func (n *Node) download(ctx context.Context, p ts.Peer, name string) (string, store.Meta, error) {
	var meta store.Meta
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base(p)+"/files/"+esc(name), nil)
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
		return "", meta, fmt.Errorf("GET %s: %s: %s", name, resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.Unmarshal([]byte(resp.Header.Get(metaHeader)), &meta); err != nil {
		return "", meta, fmt.Errorf("GET %s: bad metadata: %w", name, err)
	}
	tmp, err := n.spool(resp.Body)
	return tmp, meta.Wire(), err
}

// upload pushes name (content when held, metadata otherwise) to a peer.
func (n *Node) upload(ctx context.Context, p ts.Peer, meta store.Meta) (store.ApplyResult, error) {
	var body io.Reader
	var size int64
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, n.base(p)+"/files/"+esc(meta.Name), body)
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
		return "", fmt.Errorf("PUT %s: %s: %s", meta.Name, resp.Status, strings.TrimSpace(string(msg)))
	}
	var ack Apply
	return ack.Result, json.NewDecoder(resp.Body).Decode(&ack)
}

// wantContent decides whether to pull the bytes of a remote version now:
// always for text, for blobs when this node keeps everything, held an
// earlier version, or the blob is small enough.
func (n *Node) wantContent(local *store.Meta, remote store.Meta) bool {
	if !remote.Have || remote.Deleted {
		return false
	}
	if !remote.Blob || n.Store.Config.KeepAll || (local != nil && local.Have && !local.Deleted) {
		return true
	}
	limit := n.Store.Config.AutoFetchLimit()
	return limit >= 0 && remote.Size <= limit
}

// peerWants guesses whether a peer that only holds metadata would take the
// bytes: we cannot see its config, so only fill stubs we would fetch
// ourselves; larger blobs wait until that peer asks.
func (n *Node) peerWants(rm store.Meta) bool {
	if !rm.Blob {
		return true
	}
	limit := n.Store.Config.AutoFetchLimit()
	return limit >= 0 && rm.Size <= limit
}

// syncFiles is the content phase of Sync. Plans are made under the node
// lock, transfers run without it, each result is applied under it.
func (n *Node) syncFiles(ctx context.Context, p ts.Peer, rep *SyncReport) error {
	remoteBy, err := n.RemoteIndex(ctx, p)
	if err != nil {
		return err
	}
	type pull struct {
		meta    store.Meta
		content bool
	}
	var pulls []pull
	n.Lock()
	if _, err := n.Store.Scan(); err != nil {
		n.Unlock()
		return err
	}
	for name, rm := range remoteBy {
		lm := n.Store.Get(name)
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
	// Live files first: a renamed file is fulfilled from the bytes held
	// under its old name, which that name's tombstone would remove.
	sort.Slice(pulls, func(i, j int) bool {
		if pulls[i].meta.Deleted != pulls[j].meta.Deleted {
			return !pulls[i].meta.Deleted
		}
		return pulls[i].meta.Name < pulls[j].meta.Name
	})

	for _, pl := range pulls {
		res, err := n.pullOne(ctx, p, pl.meta, pl.content)
		name := pl.meta.Name
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("pull %s: %v", name, err))
			continue
		}
		switch res {
		case store.Accepted, store.Merged, store.Conflicted:
			rep.Pulled = append(rep.Pulled, name)
			if res == store.Merged {
				rep.Merged = append(rep.Merged, name)
			}
			if res == store.Conflicted {
				rep.Conflicts = append(rep.Conflicts, name)
			}
			if !pl.content && !pl.meta.Deleted {
				n.Lock()
				m := n.Store.Get(name)
				stub := m != nil && !m.Have && !m.Deleted
				n.Unlock()
				if stub {
					rep.NotFetched = append(rep.NotFetched, name)
				}
			}
		}
	}

	n.Lock()
	var pushes []store.Meta
	for _, lm := range n.Store.List(true) {
		rm, ok := remoteBy[lm.Name]
		if ok {
			c := clock.Compare(lm.Clock, rm.Clock)
			if c == clock.Equal && lm.Have && !rm.Have && !lm.Deleted && !rm.Deleted && n.peerWants(rm) {
				pushes = append(pushes, lm.Wire()) // fill the peer's stub
				continue
			}
			if c != clock.Dominates { // concurrent: the pull failed; leave it for the next run
				continue
			}
			if !lm.Have && !lm.Deleted && rm.Have && !rm.Deleted {
				continue // we only know about it; the peer holds older bytes
			}
		}
		pushes = append(pushes, lm.Wire())
	}
	n.Unlock()
	for _, lm := range pushes {
		res, err := n.upload(ctx, p, lm)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("push %s: %v", lm.Name, err))
			continue
		}
		switch res {
		case store.Accepted, store.Merged, store.Conflicted:
			rep.Pushed = append(rep.Pushed, lm.Name)
		}
	}
	return nil
}

// pullOne fetches one remote version (with its bytes when content is set)
// and applies it.
func (n *Node) pullOne(ctx context.Context, p ts.Peer, meta store.Meta, content bool) (store.ApplyResult, error) {
	var src io.Reader
	if content {
		tmp, got, err := n.download(ctx, p, meta.Name)
		if err != nil {
			return "", err
		}
		defer os.Remove(tmp)
		f, err := os.Open(tmp)
		if err != nil {
			return "", err
		}
		defer f.Close()
		meta, src = got, f
	} else {
		meta = meta.Wire()
		meta.HistoryDigest = ""
	}
	n.Lock()
	defer n.Unlock()
	return n.Store.Apply(meta, src)
}

// SyncAll syncs with every online peer that answers np's ping. Peers are
// pinged concurrently and synced one after another.
func (n *Node) SyncAll(ctx context.Context) ([]SyncReport, error) {
	peers, err := ts.Peers(ctx)
	if err != nil {
		return nil, err
	}
	type probe struct {
		p  ts.Peer
		ok bool
	}
	results := make([]probe, len(peers))
	done := make(chan struct{}, len(peers))
	for i, p := range peers {
		results[i].p = p
		go func(i int, p ts.Peer) {
			defer func() { done <- struct{}{} }()
			if p.Self || !p.Online {
				return
			}
			_, err := n.PingPeer(ctx, p)
			results[i].ok = err == nil
		}(i, p)
	}
	for range peers {
		<-done
	}
	var reps []SyncReport
	for _, r := range results {
		if !r.ok {
			continue
		}
		rep, err := n.Sync(ctx, r.p)
		if err != nil {
			rep = SyncReport{Peer: r.p.Name, Errors: []string{err.Error()}}
		}
		reps = append(reps, rep)
	}
	return reps, nil
}

// FetchFile pulls the content of a file this node only knows about, from the
// hub or any online peer that holds it.
func (n *Node) FetchFile(ctx context.Context, name string) (ts.Peer, error) {
	n.Lock()
	var hash string
	m := n.Store.Get(name)
	known := m != nil && !m.Deleted
	have := known && m.Have
	if known {
		hash = m.Hash
	}
	n.Unlock()
	if !known {
		return ts.Peer{}, fmt.Errorf("no file %q", name)
	}
	if have {
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
		remote, err := n.RemoteIndex(pctx, p)
		cancel()
		if err != nil {
			continue
		}
		rm, ok := remote[name]
		if !ok || !rm.Have || rm.Deleted || rm.Hash != hash {
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
	return n.Store.Fill(name, f)
}

// SyncState describes how a local file relates to a peer's copy.
type SyncState string

const (
	Synced   SyncState = "synced"
	Ahead    SyncState = "ahead"    // local has changes the peer lacks
	Behind   SyncState = "behind"   // peer has changes we lack
	Diverged SyncState = "conflict" // both changed; next sync will merge
	New      SyncState = "new"      // peer has never seen this file
)

func state(c int) SyncState {
	switch c {
	case clock.Equal:
		return Synced
	case clock.Dominates:
		return Ahead
	case clock.Dominated:
		return Behind
	}
	return Diverged
}

// Compare classifies every local file against a peer index. Files that
// exist only on the peer are returned under their name as Behind.
func Compare(local []*store.Meta, remote map[string]store.Meta) map[string]SyncState {
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
