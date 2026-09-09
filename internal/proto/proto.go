// Package proto implements np's sync protocol: a small JSON-over-HTTP API
// served only on the machine's Tailscale address. Authentication is the
// tailnet itself; every request is attributed with Tailscale WhoIs.
//
//	GET /np/v1/ping           -> Ping
//	GET /np/v1/index          -> []store.Meta (without history)
//	GET /np/v1/notes/{name}   -> Note
//	PUT /np/v1/notes/{name}   <- Note, -> Apply
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
	"strings"
	"sync"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

const Version = "1"

type Ping struct {
	Node    string `json:"node"`
	Version string `json:"version"`
	Notes   int    `json:"notes"`
}

type Note struct {
	Meta    store.Meta `json:"meta"`
	Content []byte     `json:"content"` // base64 in JSON
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
	mux.HandleFunc("GET /np/v1/index", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		if _, err := n.Store.Scan(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, n.index())
	}))
	mux.HandleFunc("GET /np/v1/notes/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
		n.Lock()
		defer n.Unlock()
		note, err := n.load(r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, note)
	}))
	mux.HandleFunc("PUT /np/v1/notes/{name...}", n.Auth(func(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
		var note Note
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&note); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if note.Meta.Name != r.PathValue("name") {
			http.Error(w, "name mismatch", http.StatusBadRequest)
			return
		}
		n.Lock()
		res, err := n.Store.Apply(note.Meta, note.Content)
		n.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.logf("%s pushed %s: %s", peer.Name, note.Meta.Name, res)
		if res == store.Accepted || res == store.Merged || res == store.Conflicted {
			n.NotifyChanged()
		}
		writeJSON(w, Apply{Result: res})
	}))
	n.mountFiles(mux)
	for _, m := range n.Mount {
		m(mux)
	}
	return mux
}

func (n *Node) index() []store.Meta {
	all := n.Store.List(true)
	out := make([]store.Meta, 0, len(all))
	for _, m := range all {
		c := *m
		c.History = nil
		out = append(out, c)
	}
	return out
}

func (n *Node) load(name string) (Note, error) {
	m := n.Store.Get(name)
	if m == nil {
		return Note{}, fmt.Errorf("no note %q", name)
	}
	note := Note{Meta: *m}
	note.Meta.History = nil
	if !m.Deleted {
		data, err := n.Store.Read(name)
		if err != nil {
			return Note{}, err
		}
		note.Content = data
	}
	return note, nil
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

// errNotFound wraps 404 responses so callers can tell a missing route (an
// older np on the peer) from other failures.
var errNotFound = errors.New("not found")

func (n *Node) base(p ts.Peer) string {
	return fmt.Sprintf("http://%s/np/v1", netip.AddrPortFrom(p.IP, uint16(n.Store.Config.Port)))
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
	return out, do(ctx, http.MethodGet, n.base(p)+"/ping", nil, &out)
}

// SyncReport summarises one sync run.
type SyncReport struct {
	Peer       string
	Pulled     []string
	Pushed     []string
	Merged     []string // pulled notes that were three-way merged with a local edit
	NotFetched []string // files whose metadata arrived without content
	Conflicts  []string
	Errors     []string
}

func (r SyncReport) String() string {
	s := fmt.Sprintf("%s: pulled %d, pushed %d", r.Peer, len(r.Pulled), len(r.Pushed))
	if len(r.Merged) > 0 {
		s += fmt.Sprintf(", %d merged", len(r.Merged))
	}
	if len(r.NotFetched) > 0 {
		s += fmt.Sprintf(", %d not fetched", len(r.NotFetched))
	}
	if len(r.Conflicts) > 0 {
		s += fmt.Sprintf(", %d conflict(s)", len(r.Conflicts))
	}
	if len(r.Errors) > 0 {
		s += fmt.Sprintf(", %d error(s)", len(r.Errors))
	}
	return s
}

// Detail is String plus the note names, for logs.
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

// Sync exchanges notes with a peer in both directions. Phase 1 pulls every
// note where the peer is ahead or concurrent (merging conflicts locally);
// phase 2 pushes every note where we are ahead, including merge results.
func (n *Node) Sync(ctx context.Context, p ts.Peer) (SyncReport, error) {
	rep := SyncReport{Peer: p.Name}
	if err := n.syncNotes(ctx, p, &rep); err != nil {
		return rep, err
	}
	if err := n.syncFiles(ctx, p, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func (n *Node) syncNotes(ctx context.Context, p ts.Peer, rep *SyncReport) error {
	base := n.base(p)
	remoteBy, err := n.RemoteIndex(ctx, p)
	if err != nil {
		return err
	}

	n.Lock()
	defer n.Unlock()
	if _, err := n.Store.Scan(); err != nil {
		return err
	}

	// Phase 1: pull.
	for name, rm := range remoteBy {
		lm := n.Store.Get(name)
		if lm != nil {
			switch clock.Compare(rm.Clock, lm.Clock) {
			case clock.Equal, clock.Dominated:
				continue
			}
		}
		var note Note
		if err := do(ctx, http.MethodGet, base+"/notes/"+name, nil, &note); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("pull %s: %v", name, err))
			continue
		}
		res, err := n.Store.Apply(note.Meta, note.Content)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("apply %s: %v", name, err))
			continue
		}
		switch res {
		case store.Accepted:
			rep.Pulled = append(rep.Pulled, name)
		case store.Merged:
			rep.Pulled = append(rep.Pulled, name)
			rep.Merged = append(rep.Merged, name)
		case store.Conflicted:
			rep.Pulled = append(rep.Pulled, name)
			rep.Conflicts = append(rep.Conflicts, name)
		}
	}

	// Phase 2: push.
	for _, lm := range n.Store.List(true) {
		if rm, ok := remoteBy[lm.Name]; ok {
			c := clock.Compare(lm.Clock, rm.Clock)
			if c == clock.Equal || c == clock.Dominated {
				continue
			}
			if c == clock.Concurrent { // pull failed above; leave it for next run
				continue
			}
		}
		note, err := n.load(lm.Name)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("read %s: %v", lm.Name, err))
			continue
		}
		var ack Apply
		if err := do(ctx, http.MethodPut, base+"/notes/"+lm.Name, note, &ack); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("push %s: %v", lm.Name, err))
			continue
		}
		switch ack.Result {
		case store.Accepted, store.Merged, store.Conflicted:
			rep.Pushed = append(rep.Pushed, lm.Name)
		}
	}
	return nil
}

// SyncAll syncs with every online peer that answers np's ping, in parallel.
// Peers are pinged concurrently; syncs run one at a time because the store
// lock serialises them anyway.
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

// RemoteIndex fetches a peer's note index keyed by name.
func (n *Node) RemoteIndex(ctx context.Context, p ts.Peer) (map[string]store.Meta, error) {
	var remote []store.Meta
	if err := do(ctx, http.MethodGet, n.base(p)+"/index", nil, &remote); err != nil {
		return nil, fmt.Errorf("fetch index from %s: %w", p.Name, err)
	}
	out := make(map[string]store.Meta, len(remote))
	for _, m := range remote {
		out[m.Name] = m
	}
	return out, nil
}

// SyncState describes how a local note relates to a peer's copy.
type SyncState string

const (
	Synced   SyncState = "synced"
	Ahead    SyncState = "ahead"    // local has changes the peer lacks
	Behind   SyncState = "behind"   // peer has changes we lack
	Diverged SyncState = "conflict" // both changed; next sync will merge
	New      SyncState = "new"      // peer has never seen this note
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

// Compare classifies every local note against a peer index. Notes that
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
