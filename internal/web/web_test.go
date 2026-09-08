package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EmreErdogan/np/internal/proto"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

func setup(t *testing.T) (*proto.Node, *httptest.Server) {
	t.Helper()
	s, err := store.Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	n := &proto.Node{Store: s, Self: ts.Identity{Name: "n1", Login: "me"}, Changed: make(chan struct{}, 1)}
	n.WhoIs = func(context.Context, string) (ts.Peer, error) { return ts.Peer{Name: "phone", Login: "me"}, nil }
	n.Mount = []func(*http.ServeMux){Mount(n)}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	return n, srv
}

func post(t *testing.T, url, body string, guarded bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if guarded {
		req.Header.Set("X-Requested-With", "np")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebFlow(t *testing.T) {
	n, srv := setup(t)
	// Create.
	if r := post(t, srv.URL+"/web/save", `{"name":"todo","content":"milk","base":""}`, true); r.StatusCode != 200 {
		t.Fatalf("create: %d", r.StatusCode)
	}
	got, _ := n.Store.Read("todo")
	if string(got) != "milk" {
		t.Fatal(string(got))
	}
	select {
	case <-n.Changed:
	default:
		t.Fatal("save should signal Changed")
	}
	// List and note pages render.
	for _, path := range []string{"/", "/n/todo", "/n/todo?edit"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %d", path, err, resp.StatusCode)
		}
	}
	// Stale base is rejected, fresh base accepted.
	if r := post(t, srv.URL+"/web/save", `{"name":"todo","content":"eggs","base":"stale"}`, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("stale: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/web/save", `{"name":"todo","content":"eggs","base":"`+n.Store.Get("todo").Hash+`"}`, true); r.StatusCode != 200 {
		t.Fatalf("update: %d", r.StatusCode)
	}
	if n.Store.Get("todo").Clock.String() != "n1:2" {
		t.Fatal(n.Store.Get("todo").Clock)
	}
	// Cross-site style POST without the header is refused.
	if r := post(t, srv.URL+"/web/save", `{"name":"todo","content":"x","base":""}`, false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("unguarded: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/web/save", `{"name":"../x","content":"x","base":""}`, true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: %d", r.StatusCode)
	}
	// Delete.
	if r := post(t, srv.URL+"/web/delete", `{"name":"todo"}`, true); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if !n.Store.Get("todo").Deleted {
		t.Fatal("expected tombstone")
	}
}
