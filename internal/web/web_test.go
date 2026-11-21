package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// get fetches a page and returns its status and body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWebFlow(t *testing.T) {
	n, srv := setup(t)
	// Create: a name without an extension becomes markdown.
	r := post(t, srv.URL+"/web/save", `{"name":"todo","content":"milk","base":""}`, true)
	if b, _ := io.ReadAll(r.Body); r.StatusCode != 200 || !strings.Contains(string(b), `"name":"todo.md"`) {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	got, _ := n.Store.Read("todo.md")
	if string(got) != "milk" {
		t.Fatal(string(got))
	}
	if m := n.Store.Get("todo.md"); m.ModBy != "phone" || m.Clock.String() != "n1:1" {
		t.Fatalf("web edit should be attributed to the caller, clock to the node: %+v", m)
	}
	select {
	case <-n.Changed:
	default:
		t.Fatal("save should signal Changed")
	}
	// Creating under an existing name is refused with a clear message,
	// whether typed with or without the extension.
	for _, name := range []string{"todo", "todo.md"} {
		if r := post(t, srv.URL+"/web/save", `{"name":"`+name+`","content":"x","base":""}`, true); r.StatusCode != http.StatusConflict {
			t.Fatalf("duplicate create %s: %d", name, r.StatusCode)
		}
	}
	// List, new and file pages render; old note links and the shorthand redirect.
	for _, path := range []string{"/", "/new", "/new?d=work", "/f/todo.md", "/f/todo.md?edit", "/n/todo", "/f/todo", "/n/todo?edit", "/search?q=milk"} {
		if code, body := get(t, srv.URL+path); code != 200 || !strings.Contains(body, "np") {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if code, _ := get(t, srv.URL+"/f/nope.md"); code != 404 {
		t.Fatalf("missing file: %d", code)
	}
	// Stale base is rejected, fresh base accepted.
	if r := post(t, srv.URL+"/web/save", `{"name":"todo.md","content":"eggs","base":"stale"}`, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("stale: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/web/save", `{"name":"todo.md","content":"eggs","base":"`+n.Store.Get("todo.md").Hash+`"}`, true); r.StatusCode != 200 {
		t.Fatalf("update: %d", r.StatusCode)
	}
	if n.Store.Get("todo.md").Clock.String() != "n1:2" {
		t.Fatal(n.Store.Get("todo.md").Clock)
	}
	// Cross-site style POST without the header is refused.
	if r := post(t, srv.URL+"/web/save", `{"name":"todo.md","content":"x","base":""}`, false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("unguarded: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/web/save", `{"name":"../x","content":"x","base":""}`, true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: %d", r.StatusCode)
	}
	// Delete.
	if r := post(t, srv.URL+"/web/delete", `{"name":"todo.md"}`, true); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if m := n.Store.Get("todo.md"); !m.Deleted || m.ModBy != "phone" {
		t.Fatalf("expected tombstone by phone: %+v", m)
	}
	if r := post(t, srv.URL+"/web/delete", `{"name":"todo.md"}`, true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("deleting twice: %d", r.StatusCode)
	}
	// A deleted file's history is still reachable.
	if code, body := get(t, srv.URL+"/h/todo.md"); code != 200 || !strings.Contains(body, "deleted") {
		t.Fatalf("history of a deleted file: %d", code)
	}
}

func TestMarkdownRender(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("doc.md", []byte("# Title\n\n- [x] done\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"))
	code, html := get(t, srv.URL+"/f/doc.md")
	if code != 200 {
		t.Fatal(code)
	}
	for _, want := range []string{"<h1 id=\"title\">Title</h1>", "type=\"checkbox\"", "<table>", ">Edit<", "add a line"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("raw html must not be rendered")
	}
	// Edit view still shows the source.
	if _, body := get(t, srv.URL+"/f/doc.md?edit"); !strings.Contains(body, "# Title") || !strings.Contains(body, "<textarea") {
		t.Error("edit view should contain raw markdown")
	}
}

func TestCodeRender(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("cfg.json", []byte(`{"a": 1}`))
	if _, body := get(t, srv.URL+"/f/cfg.json"); !strings.Contains(body, `class="chroma"`) || strings.Contains(body, "<p>") {
		t.Errorf("json should render as highlighted code, got: %.300s", body)
	}
}

func TestAppend(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("list.md", []byte("- a\n"))
	if r := post(t, srv.URL+"/web/append", `{"name":"list.md","content":"- b"}`, true); r.StatusCode != 200 {
		t.Fatalf("append: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/web/append", `{"name":"list","content":"note"}`, true); r.StatusCode != 200 { // shorthand
		t.Fatalf("append: %d", r.StatusCode)
	}
	got, _ := n.Store.Read("list.md")
	if string(got) != "- a\n- b\n\nnote\n" || n.Store.Get("list.md").ModBy != "phone" {
		t.Fatalf("%q by %s", got, n.Store.Get("list.md").ModBy)
	}
	if r := post(t, srv.URL+"/web/append", `{"name":"list.md","content":"x"}`, false); r.StatusCode != http.StatusForbidden {
		t.Fatal("unguarded append accepted")
	}
	// Appending to a name nobody has creates a markdown file.
	post(t, srv.URL+"/web/append", `{"name":"inbox","content":"first"}`, true)
	if got, _ := n.Store.Read("inbox.md"); string(got) != "first\n" {
		t.Fatalf("inbox=%q", got)
	}
}

func TestHistoryAndRestore(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("doc.md", []byte("one"))
	n.Store.Write("doc.md", []byte("two"))
	if code, body := get(t, srv.URL+"/h/doc.md"); code != 200 || !strings.Contains(body, "v2") || !strings.Contains(body, "v1") {
		t.Fatalf("history page: %d %.200s", code, body)
	}
	if _, body := get(t, srv.URL+"/h/doc.md?v=1"); !strings.Contains(body, "one") || !strings.Contains(body, "Restore") {
		t.Fatalf("version page: %.300s", body)
	}
	if r := post(t, srv.URL+"/web/restore", `{"name":"doc.md","seq":1}`, true); r.StatusCode != 200 {
		t.Fatalf("restore: %d", r.StatusCode)
	}
	got, _ := n.Store.Read("doc.md")
	if string(got) != "one" || len(n.Store.Get("doc.md").History) != 3 || n.Store.Get("doc.md").ModBy != "phone" {
		t.Fatalf("after restore: %q %+v", got, n.Store.Get("doc.md"))
	}
	if r := post(t, srv.URL+"/web/restore", `{"name":"doc.md","seq":9}`, true); r.StatusCode != http.StatusBadRequest {
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

func TestUploadViewDelete(t *testing.T) {
	n, srv := setup(t)
	if r := upload(t, srv.URL+"/web/upload", "file", "cat.png", "PNG\x00img", false); r.StatusCode != 403 {
		t.Fatalf("unguarded upload: %d", r.StatusCode)
	}
	r := upload(t, srv.URL+"/web/upload", "file", "cat.png", "PNG\x00img", true)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("upload: %d %s", r.StatusCode, b)
	}
	m := n.Store.Get("cat.png")
	if m == nil || !m.Have || !m.Blob || m.ModBy != "phone" || m.Size != 7 {
		t.Fatalf("meta=%+v", m)
	}
	if r := upload(t, srv.URL+"/web/upload", "file", "cat.png", "other", true); r.StatusCode != 409 {
		t.Fatalf("duplicate upload: %d", r.StatusCode)
	}
	if r := upload(t, srv.URL+"/web/upload?replace", "file", "cat.png", "PNG\x00new", true); r.StatusCode != 200 {
		t.Fatalf("replace: %d", r.StatusCode)
	}
	// List shows it; file page previews it; raw serves it, sandboxed.
	if _, body := get(t, srv.URL+"/"); !strings.Contains(body, `href="/f/cat.png"`) {
		t.Fatal("list should link the file")
	}
	if _, body := get(t, srv.URL+"/f/cat.png"); !strings.Contains(body, `<img class=prev src="/raw/cat.png"`) {
		t.Fatalf("file page: %s", body)
	}
	resp, _ := http.Get(srv.URL + "/raw/cat.png")
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "image/png" || string(body) != "PNG\x00new" {
		t.Fatalf("raw: %s %q", resp.Header.Get("Content-Type"), body)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("raw must be sandboxed: %v", resp.Header)
	}
	// An uploaded markdown file is a text file like any other: rendered,
	// editable, with history from its first version.
	upload(t, srv.URL+"/web/upload?dir=inbox", "file", "notes.md", "# hello", true)
	if m := n.Store.Get("inbox/notes.md"); m == nil || m.Blob || !n.Store.HasSnapshot("inbox/notes.md", m.Hash) {
		t.Fatalf("uploaded text=%+v", m)
	}
	if _, body := get(t, srv.URL+"/f/inbox/notes.md"); !strings.Contains(body, `<h1 id="hello">hello</h1>`) || !strings.Contains(body, ">Edit<") {
		t.Fatalf("text page: %s", body)
	}
	// Binary files offer no editor, even when asked.
	if _, body := get(t, srv.URL+"/f/cat.png?edit"); strings.Contains(body, "<textarea") {
		t.Fatal("a binary file must not open in the editor")
	}
	// Delete.
	if r := post(t, srv.URL+"/web/delete", `{"name":"cat.png"}`, true); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if m := n.Store.Get("cat.png"); !m.Deleted {
		t.Fatal("should be a tombstone")
	}
	if code, _ := get(t, srv.URL+"/raw/cat.png"); code != 404 {
		t.Fatalf("raw after delete: %d", code)
	}
}

func TestStubPage(t *testing.T) {
	n, srv := setup(t)
	n.Store.Apply(store.Meta{Name: "big.mov", Hash: "abc", Size: 5 << 20, ModBy: "laptop", Have: true, Blob: true}, nil)
	if _, body := get(t, srv.URL+"/f/big.mov"); !strings.Contains(body, "Not on this machine yet") || !strings.Contains(body, "5.0 MB") {
		t.Fatalf("stub page: %s", body)
	}
	if _, body := get(t, srv.URL+"/"); !strings.Contains(body, "not fetched") {
		t.Fatal("list should mark stubs")
	}
}

func TestHistoryDiff(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("d.md", []byte("one\ntwo\n"))
	n.Store.Write("d.md", []byte("one\n2\nthree\n"))
	if _, body := get(t, srv.URL+"/h/d.md"); !strings.Contains(body, `<span class=add>+2</span>`) || !strings.Contains(body, `<span class=del>−1</span>`) {
		t.Fatalf("history stats: %s", body)
	}
	_, body := get(t, srv.URL+"/h/d.md?v=2")
	for _, want := range []string{"Changes from v1", `<span class=del>-two</span>`, `<span class=add>+2</span>`, `<span class=add>+three</span>`, "@@ -1,2 +1,3 @@"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	// First version has nothing to compare against.
	if _, body := get(t, srv.URL+"/h/d.md?v=1"); strings.Contains(body, "Changes from") {
		t.Fatal("v1 should not show a diff section")
	}
}

func TestBlobHistory(t *testing.T) {
	n, srv := setup(t)
	n.Store.Put("p.png", strings.NewReader("\x00one"), "n1")
	n.Store.Put("p.png", strings.NewReader("\x00three"), "n1")
	if code, body := get(t, srv.URL+"/h/p.png"); code != 200 || !strings.Contains(body, "v2") || !strings.Contains(body, "6 B") {
		t.Fatalf("blob history: %d %s", code, body)
	}
	if _, body := get(t, srv.URL+"/h/p.png?v=1"); !strings.Contains(body, "was not kept") || strings.Contains(body, "Restore") {
		t.Fatalf("blob version page: %s", body)
	}
	if r := post(t, srv.URL+"/web/restore", `{"name":"p.png","seq":1}`, true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("restoring a blob version: %d", r.StatusCode)
	}
}

func TestWebRename(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("a.md", []byte("x"))
	// A new name without an extension keeps the old one.
	if r := post(t, srv.URL+"/web/rename", `{"name":"a.md","to":"b/c"}`, true); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("rename: %d %s", r.StatusCode, b)
	}
	if m := n.Store.Get("b/c.md"); m == nil || m.ModBy != "phone" || m.RenamedFrom != "a.md" {
		t.Fatalf("meta=%+v", m)
	}
	upload(t, srv.URL+"/web/upload", "file", "p.png", "\x00img", true)
	if r := post(t, srv.URL+"/web/rename", `{"name":"p.png","to":"pics/"}`, true); r.StatusCode != 200 {
		t.Fatalf("rename into a folder: %d", r.StatusCode)
	}
	if m := n.Store.Get("pics/p.png"); m == nil || !m.Have {
		t.Fatalf("file=%+v", m)
	}
	if _, body := get(t, srv.URL+"/h/b/c.md"); !strings.Contains(body, "renamed from a.md") {
		t.Fatalf("history page: %s", body)
	}
}

func TestFoldersAndSearch(t *testing.T) {
	n, srv := setup(t)
	n.Store.Write("root.md", []byte("at the top"))
	n.Store.Write("work/plan.md", []byte("ship the thing\nthen rest"))
	n.Store.Write("work/ideas/next.md", []byte("an idea about SHIPPING"))
	n.Store.Put("work/logo.png", strings.NewReader("\x00png"), "n1")

	_, root := get(t, srv.URL+"/")
	if !strings.Contains(root, `href="/?d=work"`) || !strings.Contains(root, `href="/f/root.md"`) || strings.Contains(root, "plan.md") {
		t.Fatalf("root should show the work folder and only its own files: %s", root)
	}
	_, work := get(t, srv.URL+"/?d=work")
	for _, want := range []string{`href="/?d=work%2fideas"`, `href="/f/work/plan.md"`, `href="/f/work/logo.png"`, `href="/new?d=work"`} {
		if !strings.Contains(work, want) {
			t.Fatalf("work folder: missing %q in %s", want, work)
		}
	}
	if strings.Contains(work, "root.md") || strings.Contains(work, `href="/f/work/ideas/next.md"`) {
		t.Fatal("a folder lists only what is directly in it")
	}

	// Search covers every folder, names and content, and skips binaries.
	_, res := get(t, srv.URL+"/search?q=ship")
	for _, want := range []string{`href="/f/work/plan.md"`, "ship the thing", `href="/f/work/ideas/next.md"`, "an idea about SHIPPING"} {
		if !strings.Contains(res, want) {
			t.Fatalf("search: missing %q in %s", want, res)
		}
	}
	if strings.Contains(res, "root.md") || strings.Contains(res, "logo.png") {
		t.Fatal("search matched too much")
	}
	if _, res := get(t, srv.URL+"/search?q=logo"); !strings.Contains(res, `href="/f/work/logo.png"`) {
		t.Fatal("search should match file names of binaries")
	}
	if _, res := get(t, srv.URL+"/search?q=zzz"); !strings.Contains(res, "nothing matches") {
		t.Fatal("empty result page")
	}
}

func TestLargeFolderIsCappedAndRecentShown(t *testing.T) {
	n, srv := setup(t)
	for i := 0; i < folderCap+25; i++ {
		n.Store.Write(fmt.Sprintf("log/e%04d.md", i), []byte("x"))
	}
	n.Store.Write("newest.md", []byte("x"))
	time.Sleep(5 * time.Millisecond)
	n.Store.Write("newest.md", []byte("y"))
	_, root := get(t, srv.URL+"/")
	if !strings.Contains(root, "recently changed") || !strings.Contains(root, `href="/f/newest.md"`) {
		t.Fatal("root should list recent files once the store is big")
	}
	if strings.Count(root, "<li ") > recentCount+3 {
		t.Fatalf("root renders %d rows for %d files", strings.Count(root, "<li "), folderCap+26)
	}
	_, log := get(t, srv.URL+"/?d=log")
	if strings.Count(log, `href="/f/log/`) != folderCap || !strings.Contains(log, "show 25 more") {
		t.Fatalf("folder should be capped at %d rows", folderCap)
	}
	if _, all := get(t, srv.URL+"/?d=log&all"); strings.Count(all, `href="/f/log/`) != folderCap+25 {
		t.Fatal("?all should show every row")
	}
}

func TestUnusualNamesMakeWorkingLinks(t *testing.T) {
	n, srv := setup(t)
	name := "trip photos/İstanbul #1?.md"
	if err := n.Store.Write(name, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, list := get(t, srv.URL+"/?d="+"trip%20photos")
	i := strings.Index(list, `href="/f/trip`)
	if i < 0 {
		t.Fatalf("no link in %s", list)
	}
	href := list[i+6 : i+6+strings.Index(list[i+6:], `"`)]
	if strings.ContainsAny(href, "#? ") {
		t.Fatalf("link not escaped: %s", href)
	}
	if code, body := get(t, srv.URL+href); code != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("following %s: %d", href, code)
	}
}
