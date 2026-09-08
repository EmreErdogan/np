package proto

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/emre/np/internal/store"
	"github.com/emre/np/internal/ts"
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
