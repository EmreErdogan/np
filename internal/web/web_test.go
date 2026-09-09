package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
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
	if m := n.Store.Get("todo"); m.ModBy != "phone" || m.Clock.String() != "n1:1" {
		t.Fatalf("web edit should be attributed to the caller, clock to the node: %+v", m)
	}
	select {
	case <-n.Changed:
	default:
		t.Fatal("save should signal Changed")
	}
	// Creating under an existing name is refused with a clear message.
	if r := post(t, srv.URL+"/web/save", `{"name":"todo","content":"x","base":""}`, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create: %d", r.StatusCode)
	}
	// List, new and note pages render.
	for _, path := range []string{"/", "/new", "/n/todo", "/n/todo?edit"} {
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
	if m := n.Store.Get("todo"); !m.Deleted || m.ModBy != "phone" {
		t.Fatalf("expected tombstone by phone: %+v", m)
	}
}

func TestMarkdownRender(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("doc", []byte("# Title\n\n- [x] done\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"))
	resp, err := http.Get(srv.URL + "/n/doc")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %d", err, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{"<h1 id=\"title\">Title</h1>", "type=\"checkbox\"", "<table>"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("raw html must not be rendered")
	}
	// Edit view still shows the source.
	resp, _ = http.Get(srv.URL + "/n/doc?edit")
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "# Title") {
		t.Error("edit view should contain raw markdown")
	}
}

func TestCodeRender(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("cfg.json", []byte(`{"a": 1}`))
	resp, _ := http.Get(srv.URL + "/n/cfg.json")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `class="chroma"`) || strings.Contains(string(body), "<p>") {
		t.Errorf("json should render as highlighted code, got: %.300s", body)
	}
	// "todo.md" in a URL is the same note as "todo".
	n.Store.Write("todo", []byte("x"))
	if resp, _ = http.Get(srv.URL + "/n/todo.md"); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestAppend(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("list", []byte("- a\n"))
	if r := post(t, srv.URL+"/web/append", `{"name":"list","content":"- b"}`, true); r.StatusCode != 200 {
		t.Fatalf("append: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/web/append", `{"name":"list","content":"note"}`, true); r.StatusCode != 200 {
		t.Fatalf("append: %d", r.StatusCode)
	}
	got, _ := n.Store.Read("list")
	if string(got) != "- a\n- b\n\nnote\n" || n.Store.Get("list").ModBy != "phone" {
		t.Fatalf("%q by %s", got, n.Store.Get("list").ModBy)
	}
	if r := post(t, srv.URL+"/web/append", `{"name":"list","content":"x"}`, false); r.StatusCode != http.StatusForbidden {
		t.Fatal("unguarded append accepted")
	}
}

func TestHistoryAndRestore(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("doc", []byte("one"))
	n.Store.Write("doc", []byte("two"))
	resp, _ := http.Get(srv.URL + "/h/doc")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "v2") || !strings.Contains(string(body), "v1") {
		t.Fatalf("history page: %d %.200s", resp.StatusCode, body)
	}
	resp, _ = http.Get(srv.URL + "/h/doc?v=1")
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "one") || !strings.Contains(string(body), "Restore") {
		t.Fatalf("version page: %.300s", body)
	}
	if r := post(t, srv.URL+"/web/restore", `{"name":"doc","seq":1}`, true); r.StatusCode != 200 {
		t.Fatalf("restore: %d", r.StatusCode)
	}
	got, _ := n.Store.Read("doc")
	if string(got) != "one" || len(n.Store.Get("doc").History) != 3 || n.Store.Get("doc").ModBy != "phone" {
		t.Fatalf("after restore: %q %+v", got, n.Store.Get("doc"))
	}
	if r := post(t, srv.URL+"/web/restore", `{"name":"doc","seq":9}`, true); r.StatusCode != http.StatusBadRequest {
		t.Fatal("bad seq accepted")
	}
}

func upload(t *testing.T, url, field, filename, content string, guarded bool) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(field, filename)
	fw.Write([]byte(content))
	mw.Close()
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if guarded {
		req.Header.Set("X-Requested-With", "np")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFileUploadViewDelete(t *testing.T) {
	n, srv := setup(t)
	if r := upload(t, srv.URL+"/web/upload", "file", "cat.png", "PNG\x00img", false); r.StatusCode != 403 {
		t.Fatalf("unguarded upload: %d", r.StatusCode)
	}
	r := upload(t, srv.URL+"/web/upload", "file", "cat.png", "PNG\x00img", true)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("upload: %d %s", r.StatusCode, b)
	}
	m := n.Store.File("cat.png")
	if m == nil || !m.Have || m.ModBy != "phone" || m.Size != 7 {
		t.Fatalf("meta=%+v", m)
	}
	if r := upload(t, srv.URL+"/web/upload", "file", "cat.png", "other", true); r.StatusCode != 409 {
		t.Fatalf("duplicate upload: %d", r.StatusCode)
	}
	// List shows it; file page previews it; raw serves it.
	resp, _ := http.Get(srv.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `href="/f/cat.png"`) {
		t.Fatal("list should link the file")
	}
	resp, _ = http.Get(srv.URL + "/f/cat.png")
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<img class=prev src="/raw/cat.png"`) {
		t.Fatalf("file page: %s", body)
	}
	resp, _ = http.Get(srv.URL + "/raw/cat.png")
	body, _ = io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "image/png" || string(body) != "PNG\x00img" {
		t.Fatalf("raw: %s %q", resp.Header.Get("Content-Type"), body)
	}
	// Text files get an inline preview.
	upload(t, srv.URL+"/web/upload", "file", "notes.txt", "hello", true)
	resp, _ = http.Get(srv.URL + "/f/notes.txt")
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<pre>hello</pre>") {
		t.Fatalf("text preview: %s", body)
	}
	// Delete.
	if r := post(t, srv.URL+"/web/delete-file", `{"name":"cat.png"}`, true); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if m := n.Store.File("cat.png"); !m.Deleted {
		t.Fatal("should be a tombstone")
	}
	if resp, _ := http.Get(srv.URL + "/raw/cat.png"); resp.StatusCode != 404 {
		t.Fatalf("raw after delete: %d", resp.StatusCode)
	}
}

func TestFileStubPage(t *testing.T) {
	n, srv := setup(t)
	n.Store.ApplyFile(store.FileMeta{Name: "big.mov", Hash: "abc", Size: 5 << 20, ModBy: "laptop", Have: true}, nil)
	resp, _ := http.Get(srv.URL + "/f/big.mov")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Not on this machine yet") || !strings.Contains(string(body), "5.0 MB") {
		t.Fatalf("stub page: %s", body)
	}
	resp, _ = http.Get(srv.URL + "/")
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not fetched") {
		t.Fatal("list should mark stubs")
	}
}

func TestHistoryDiff(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("d", []byte("one\ntwo\n"))
	n.Store.Write("d", []byte("one\n2\nthree\n"))
	resp, _ := http.Get(srv.URL + "/h/d")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<span class=add>+2</span>`) || !strings.Contains(string(body), `<span class=del>−1</span>`) {
		t.Fatalf("history stats: %s", body)
	}
	resp, _ = http.Get(srv.URL + "/h/d?v=2")
	body, _ = io.ReadAll(resp.Body)
	for _, want := range []string{"Changes from v1", `<span class=del>-two</span>`, `<span class=add>+2</span>`, `<span class=add>+three</span>`, "@@ -1,2 +1,3 @@"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	// First version has nothing to compare against.
	resp, _ = http.Get(srv.URL + "/h/d?v=1")
	body, _ = io.ReadAll(resp.Body)
	if strings.Contains(string(body), "Changes from") {
		t.Fatal("v1 should not show a diff section")
	}
}
