package proto

import (
	"context"
	"io"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EmreErdogan/np/internal/clock"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

func newNode(t *testing.T, name, login string) *Node {
	t.Helper()
	s, err := store.Open(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	return &Node{Store: s, Self: ts.Identity{Name: name, Login: login}}
}

// serve exposes n over httptest and returns a Peer that client nodes can dial.
func serve(t *testing.T, n *Node, callerLogin string) ts.Peer {
	t.Helper()
	n.WhoIs = func(context.Context, string) (ts.Peer, error) {
		return ts.Peer{Name: "caller", Login: callerLogin}, nil
	}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	n.Store.Config.Port = int(ap.Port())
	return ts.Peer{Name: n.Self.Name, IP: ap.Addr(), Online: true}
}

func TestSyncRoundTrip(t *testing.T) {
	ctx := context.Background()
	hub := newNode(t, "hub", "emre@example.com")
	hubPeer := serve(t, hub, "emre@example.com")
	a := newNode(t, "a", "emre@example.com")
	b := newNode(t, "b", "emre@example.com")
	a.Store.Config.Port = hub.Store.Config.Port
	b.Store.Config.Port = hub.Store.Config.Port

	a.Store.Write("shared", []byte("hello from a"))
	rep, err := a.Sync(ctx, hubPeer)
	if err != nil || len(rep.Pushed) != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	rep, err = b.Sync(ctx, hubPeer)
	if err != nil || len(rep.Pulled) != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	got, _ := b.Store.Read("shared")
	if string(got) != "hello from a" {
		t.Fatal(string(got))
	}

	// Sequential edit on b flows back to a through the hub.
	b.Store.Write("shared", []byte("edited on b"))
	if rep, _ = b.Sync(ctx, hubPeer); len(rep.Pushed) != 1 {
		t.Fatalf("%+v", rep)
	}
	if rep, _ = a.Sync(ctx, hubPeer); len(rep.Pulled) != 1 || len(rep.Conflicts) != 0 {
		t.Fatalf("%+v", rep)
	}
	got, _ = a.Store.Read("shared")
	if string(got) != "edited on b" {
		t.Fatal(string(got))
	}

	// Concurrent edits: a and b both change without syncing in between.
	a.Store.Write("shared", []byte("a wins?"))
	time.Sleep(20 * time.Millisecond)
	b.Store.Write("shared", []byte("b is newer"))
	a.Sync(ctx, hubPeer) // a pushes first
	rep, _ = b.Sync(ctx, hubPeer)
	if len(rep.Conflicts) != 1 {
		t.Fatalf("expected conflict on b: %+v", rep)
	}
	rep, _ = a.Sync(ctx, hubPeer)
	if len(rep.Pulled) < 2 { // merged "shared" + the conflict copy
		t.Fatalf("a should pull merge result and conflict note: %+v", rep)
	}
	for _, n := range []*Node{hub, a, b} {
		got, _ := n.Store.Read("shared")
		if string(got) != "b is newer" {
			t.Fatalf("%s has %q", n.Self.Name, got)
		}
		if len(n.Store.List(false)) != 2 {
			t.Fatalf("%s should have 2 notes, has %d", n.Self.Name, len(n.Store.List(false)))
		}
	}

	// Delete propagates.
	a.Store.Delete("shared")
	a.Sync(ctx, hubPeer)
	b.Sync(ctx, hubPeer)
	if _, err := b.Store.Read("shared"); err == nil {
		t.Fatal("delete did not propagate")
	}
	// Idempotent.
	rep, _ = b.Sync(ctx, hubPeer)
	if len(rep.Pulled)+len(rep.Pushed) != 0 {
		t.Fatalf("expected no-op sync: %+v", rep)
	}
}

func TestSyncDeniesOtherLogin(t *testing.T) {
	hub := newNode(t, "hub", "emre@example.com")
	hubPeer := serve(t, hub, "stranger@example.com")
	a := newNode(t, "a", "x")
	a.Store.Config.Port = hub.Store.Config.Port
	if _, err := a.Sync(context.Background(), hubPeer); err == nil {
		t.Fatal("expected 403")
	}
	hub.Store.Config.Allow = []string{"stranger@example.com"}
	if _, err := a.Sync(context.Background(), hubPeer); err != nil {
		t.Fatal(err)
	}
	_ = strconv.Itoa
}

func TestCompareStates(t *testing.T) {
	local := []*store.Meta{
		{Name: "same", Clock: clock.Clock{"a": 1}},
		{Name: "ahead", Clock: clock.Clock{"a": 2}},
		{Name: "behind", Clock: clock.Clock{"a": 1}},
		{Name: "div", Clock: clock.Clock{"a": 2}},
		{Name: "new", Clock: clock.Clock{"a": 1}},
	}
	remote := map[string]store.Meta{
		"same":   {Clock: clock.Clock{"a": 1}},
		"ahead":  {Clock: clock.Clock{"a": 1}},
		"behind": {Clock: clock.Clock{"a": 1, "b": 1}},
		"div":    {Clock: clock.Clock{"a": 1, "b": 1}},
		"remote": {Clock: clock.Clock{"b": 1}},
		"gone":   {Clock: clock.Clock{"b": 1}, Deleted: true},
	}
	got := Compare(local, remote)
	want := map[string]SyncState{"same": Synced, "ahead": Ahead, "behind": Behind, "div": Diverged, "new": New, "remote": Behind}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %s want %s", k, got[k], v)
		}
	}
}

func TestSyncMergesDisjointEditsThroughHub(t *testing.T) {
	ctx := context.Background()
	hub := newNode(t, "hub", "emre@example.com")
	hubPeer := serve(t, hub, "emre@example.com")
	a := newNode(t, "a", "emre@example.com")
	b := newNode(t, "b", "emre@example.com")
	a.Store.Config.Port = hub.Store.Config.Port
	b.Store.Config.Port = hub.Store.Config.Port

	a.Store.Write("list", []byte("- milk\n- eggs\n"))
	a.Sync(ctx, hubPeer)
	b.Sync(ctx, hubPeer)

	a.Store.Write("list", []byte("- oat milk\n- eggs\n"))
	b.Store.Write("list", []byte("- milk\n- eggs\n- bread\n"))
	a.Sync(ctx, hubPeer)
	// b pulls a's version, merges it locally, and pushes the result in the
	// same run.
	rep, _ := b.Sync(ctx, hubPeer)
	if len(rep.Conflicts) != 0 || len(rep.Merged) != 1 || len(rep.Pushed) != 1 {
		t.Fatalf("b: %+v", rep)
	}
	if rep, _ = b.Sync(ctx, hubPeer); len(rep.Pulled)+len(rep.Pushed) != 0 {
		t.Fatalf("b second sync should be a no-op: %+v", rep)
	}
	rep, _ = a.Sync(ctx, hubPeer)
	if len(rep.Pulled) != 1 {
		t.Fatalf("a: %+v", rep)
	}
	for _, n := range []*Node{hub, a, b} {
		got, _ := n.Store.Read("list")
		if string(got) != "- oat milk\n- eggs\n- bread\n" {
			t.Fatalf("%s has %q", n.Self.Name, got)
		}
		if len(n.Store.List(false)) != 1 {
			t.Fatalf("%s has %d notes", n.Self.Name, len(n.Store.List(false)))
		}
	}
}

func readFile(t *testing.T, n *Node, name string) string {
	t.Helper()
	f, err := n.Store.OpenFile(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

func TestSyncFilesLazy(t *testing.T) {
	ctx := context.Background()
	hub := newNode(t, "hub", "emre@example.com")
	hub.Store.Config.KeepAll = true
	hubPeer := serve(t, hub, "emre@example.com")
	a := newNode(t, "a", "emre@example.com")
	b := newNode(t, "b", "emre@example.com")
	a.Store.Config.Port = hub.Store.Config.Port
	b.Store.Config.Port = hub.Store.Config.Port
	a.Store.Config.AutoFetch = 16
	b.Store.Config.AutoFetch = 16

	small := "tiny"
	big := strings.Repeat("x", 100)
	a.Store.PutFile("icon.png", strings.NewReader(small), "a")
	a.Store.PutFile("movie.mp4", strings.NewReader(big), "a")

	// a pushes both with content; the hub keeps everything.
	rep, err := a.Sync(ctx, hubPeer)
	if err != nil || len(rep.Pushed) != 2 || len(rep.Errors) != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if readFile(t, hub, "movie.mp4") != big || readFile(t, hub, "icon.png") != small {
		t.Fatal("hub content")
	}

	// b pulls the small file with content and the big one as a stub.
	rep, _ = b.Sync(ctx, hubPeer)
	if len(rep.Pulled) != 2 || len(rep.NotFetched) != 1 || rep.NotFetched[0] != "files/movie.mp4" {
		t.Fatalf("b: %+v", rep)
	}
	if readFile(t, b, "icon.png") != small {
		t.Fatal("small file should have content")
	}
	if m := b.Store.File("movie.mp4"); m == nil || m.Have || m.Size != 100 {
		t.Fatalf("stub=%+v", m)
	}
	// Idempotent: nothing moves on the next run, the stub is not re-pulled.
	if rep, _ = b.Sync(ctx, hubPeer); len(rep.Pulled)+len(rep.Pushed) != 0 {
		t.Fatalf("second sync: %+v", rep)
	}

	// Explicit fetch fills the stub.
	if err := b.FetchFileFrom(ctx, hubPeer, "movie.mp4"); err != nil {
		t.Fatal(err)
	}
	if readFile(t, b, "movie.mp4") != big {
		t.Fatal("fetched content")
	}

	// A new version of a file b holds is pulled with content even if big.
	a.Store.PutFile("movie.mp4", strings.NewReader(big+"2"), "a")
	a.Sync(ctx, hubPeer)
	rep, _ = b.Sync(ctx, hubPeer)
	if len(rep.Pulled) != 1 || len(rep.NotFetched) != 0 {
		t.Fatalf("b update: %+v", rep)
	}
	if readFile(t, b, "movie.mp4") != big+"2" {
		t.Fatal("updated content")
	}

	// Delete propagates and removes content everywhere.
	if err := b.Store.DeleteFile("movie.mp4", "b"); err != nil {
		t.Fatal(err)
	}
	b.Sync(ctx, hubPeer)
	a.Sync(ctx, hubPeer)
	for _, n := range []*Node{hub, a, b} {
		if _, err := n.Store.OpenFile("movie.mp4"); err == nil {
			t.Fatalf("%s still has movie.mp4", n.Self.Name)
		}
	}
	if len(a.Store.Files(false)) != 1 {
		t.Fatal("a should have one file left")
	}
}

func TestSyncFilesStubDoesNotDowngradeHolder(t *testing.T) {
	ctx := context.Background()
	hub := newNode(t, "hub", "emre@example.com")
	hubPeer := serve(t, hub, "emre@example.com")
	a := newNode(t, "a", "emre@example.com")
	a.Store.Config.Port = hub.Store.Config.Port
	// Hub does not keep big files; a uploads one, hub stores a stub only?
	// No: pushes always carry content. But a later metadata-only push from a
	// node that only knows about a newer version must not wipe the hub.
	a.Store.Config.AutoFetch = 4
	hub.Store.Config.AutoFetch = 4
	a.Store.PutFile("f.bin", strings.NewReader("0123456789"), "a")
	a.Sync(ctx, hubPeer)
	if readFile(t, hub, "f.bin") != "0123456789" {
		t.Fatal("hub should hold the pushed content")
	}
	c := newNode(t, "c", "emre@example.com")
	c.Store.Config.Port = hub.Store.Config.Port
	c.Store.Config.AutoFetch = 4
	c.Sync(ctx, hubPeer) // c gets a stub
	if m := c.Store.File("f.bin"); m == nil || m.Have {
		t.Fatalf("c=%+v", m)
	}
	// c drops nothing but forges a newer stub-only version? Not possible in
	// practice: c can only bump what it edits, and editing needs content.
	// Instead: hub drops its copy; a still has content and refills the hub.
	if err := hub.Store.DropFile("f.bin"); err != nil {
		t.Fatal(err)
	}
	rep, _ := a.Sync(ctx, hubPeer)
	if len(rep.Pushed) != 0 { // 10 bytes > hub's 4-byte auto limit: not refilled unasked
		t.Fatalf("a: %+v", rep)
	}
	hub.Store.Config.KeepAll = true
	hub.Store.Config.Hub = ""
	// The hub pulls it back when it syncs (keep_all) with a.
	aPeer := serve(t, a, "emre@example.com")
	hub.Store.Config.Port = a.Store.Config.Port
	rep, _ = hub.Sync(ctx, aPeer)
	if len(rep.Pulled) != 1 || readFile(t, hub, "f.bin") != "0123456789" {
		t.Fatalf("hub refill: %+v", rep)
	}
}

func TestSyncHistoryUnion(t *testing.T) {
	ctx := context.Background()
	hub := newNode(t, "hub", "emre@example.com")
	hubPeer := serve(t, hub, "emre@example.com")
	a := newNode(t, "a", "emre@example.com")
	b := newNode(t, "b", "emre@example.com")
	a.Store.Config.Port = hub.Store.Config.Port
	b.Store.Config.Port = hub.Store.Config.Port

	// a makes three versions before ever syncing.
	for _, v := range []string{"v1", "v2", "v3"} {
		a.Store.Write("n", []byte(v))
		time.Sleep(2 * time.Millisecond)
	}
	rep, err := a.Sync(ctx, hubPeer)
	if err != nil || len(rep.Pushed) != 1 || rep.VersionsPushed != 2 {
		t.Fatalf("a: %+v %v", rep, err)
	}
	if len(hub.Store.Get("n").History) != 3 {
		t.Fatalf("hub history=%d", len(hub.Store.Get("n").History))
	}
	// b pulls the note and the whole ledger.
	rep, _ = b.Sync(ctx, hubPeer)
	if len(rep.Pulled) != 1 || rep.Versions != 2 {
		t.Fatalf("b: %+v", rep)
	}
	hb := b.Store.Get("n").History
	if len(hb) != 3 || hb[2].Hash != b.Store.Get("n").Hash {
		t.Fatalf("b history=%+v", hb)
	}
	if got, _ := b.Store.Snapshot("n", hb[0].Hash); string(got) != "v1" {
		t.Fatalf("b oldest=%q", got)
	}
	// b edits twice; a ends up with all five.
	b.Store.Write("n", []byte("v4"))
	time.Sleep(2 * time.Millisecond)
	b.Store.Write("n", []byte("v5"))
	b.Sync(ctx, hubPeer)
	rep, _ = a.Sync(ctx, hubPeer)
	if len(rep.Pulled) != 1 || rep.Versions != 1 {
		t.Fatalf("a second: %+v", rep)
	}
	for _, n := range []*Node{hub, a, b} {
		h := n.Store.Get("n").History
		if len(h) != 5 || h[4].Hash != n.Store.Get("n").Hash {
			t.Fatalf("%s history=%d", n.Self.Name, len(h))
		}
		for i, want := range []string{"v1", "v2", "v3", "v4", "v5"} {
			if got, _ := n.Store.Snapshot("n", h[i].Hash); string(got) != want {
				t.Fatalf("%s v%d=%q", n.Self.Name, i+1, got)
			}
		}
	}
	// Converged: nothing moves.
	if rep, _ = a.Sync(ctx, hubPeer); rep.Versions+rep.VersionsPushed+len(rep.Pulled)+len(rep.Pushed) != 0 {
		t.Fatalf("not idempotent: %+v", rep)
	}
	// Deletion history travels too.
	b.Store.Delete("n")
	b.Sync(ctx, hubPeer)
	a.Sync(ctx, hubPeer)
	if h := a.Store.Get("n").History; len(h) != 6 || !h[5].Deleted {
		t.Fatalf("a after delete: %+v", h)
	}
}
