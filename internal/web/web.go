// Package web serves a small phone-friendly UI on the daemon: browse
// folders, search, read, edit, upload and delete files. Callers are
// identified by Tailscale WhoIs exactly like the sync API, so anyone who
// can open the page is a tailnet member the config allows.
package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/EmreErdogan/np/internal/merge"
	"github.com/EmreErdogan/np/internal/proto"
	"github.com/EmreErdogan/np/internal/render"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

const (
	maxUpload   = 2 << 30
	folderCap   = 200 // rows shown per folder before "show more"
	recentCount = 8
	recentFrom  = 12 // the root page lists recent files once there are more than this
	searchCap   = 50
	searchLines = 3
)

// Mount registers the UI routes on mux.
func Mount(n *proto.Node) func(mux *http.ServeMux) {
	w := &ui{n: n}
	return func(mux *http.ServeMux) {
		mux.HandleFunc("GET /{$}", n.Auth(w.list))
		mux.HandleFunc("GET /search", n.Auth(w.search))
		mux.HandleFunc("GET /new", n.Auth(w.create))
		mux.HandleFunc("GET /f/{name...}", n.Auth(w.file))
		mux.HandleFunc("GET /n/{name...}", n.Auth(w.oldNote))
		mux.HandleFunc("GET /h/{name...}", n.Auth(w.history))
		mux.HandleFunc("GET /raw/{name...}", n.Auth(w.raw))
		mux.HandleFunc("POST /web/save", n.Auth(w.save))
		mux.HandleFunc("POST /web/append", n.Auth(w.appendText))
		mux.HandleFunc("POST /web/delete", n.Auth(w.del))
		mux.HandleFunc("POST /web/rename", n.Auth(w.rename))
		mux.HandleFunc("POST /web/restore", n.Auth(w.restore))
		mux.HandleFunc("POST /web/upload", n.Auth(w.upload))
		mux.HandleFunc("POST /web/fetch", n.Auth(w.fetch))
	}
}

type ui struct{ n *proto.Node }

// row is a file as shown in lists. Rows are copied out of the store under
// the node lock, so templates never read live metadata.
type row struct {
	Name, Label string
	Have, Blob  bool
	Size        string
	By          string
	When        time.Time
}

func rowOf(m *store.Meta, label string) row {
	return row{Name: m.Name, Label: label, Have: m.Have, Blob: m.Blob, Size: store.FileSize(m.Size), By: m.ModBy, When: m.ModTime}
}

type crumb struct{ Name, Path string }

type folder struct {
	Name, Path string
	Count      int
}

func crumbs(dir string) []crumb {
	var out []crumb
	if dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i, p := range parts {
		out = append(out, crumb{p, strings.Join(parts[:i+1], "/")})
	}
	return out
}

// list shows one folder: its sub-folders and the files directly in it.
func (u *ui) list(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	dir := strings.Trim(r.URL.Query().Get("d"), "/")
	u.n.Lock()
	u.n.Store.Scan()
	metas := u.n.Store.List(false)
	var rows, all []row
	counts := map[string]int{}
	for _, m := range metas {
		rest := m.Name
		if dir != "" {
			if !strings.HasPrefix(m.Name, dir+"/") {
				continue
			}
			rest = m.Name[len(dir)+1:]
		}
		if dir == "" {
			all = append(all, rowOf(m, m.Name))
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			counts[rest[:i]]++
			continue
		}
		rows = append(rows, rowOf(m, rest))
	}
	u.n.Unlock()

	var dirs []folder
	for name, c := range counts {
		dirs = append(dirs, folder{name, path.Join(dir, name), c})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	var recent []row
	if len(all) > recentFrom {
		sort.SliceStable(all, func(i, j int) bool { return all[i].When.After(all[j].When) })
		recent = all[:recentCount]
	}
	more := 0
	if len(rows) > folderCap && !r.URL.Query().Has("all") {
		more = len(rows) - folderCap
		rows = rows[:folderCap]
	}
	page(w, listTmpl, map[string]any{
		"Node": u.n.Self.Name, "Who": peer.Name, "Dir": dir, "Crumbs": crumbs(dir),
		"Dirs": dirs, "Rows": rows, "Recent": recent, "More": more,
		"Empty": len(rows)+len(dirs) == 0,
	})
}

type hit struct {
	Name  string
	Lines []store.LineHit
	More  int
}

// search looks through names and text content on the server, so it covers
// every folder and does not depend on what the list page happens to show.
func (u *ui) search(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	u.n.Lock()
	u.n.Store.Scan()
	found, cut := u.n.Store.Search(q, searchCap, searchLines)
	u.n.Unlock()
	hits := make([]hit, len(found))
	for i, h := range found {
		hits[i] = hit{Name: h.Name, Lines: h.Lines, More: h.More}
		for j := range hits[i].Lines {
			if rs := []rune(hits[i].Lines[j].Text); len(rs) > 200 {
				hits[i].Lines[j].Text = string(rs[:200]) + "…"
			}
		}
	}
	page(w, searchTmpl, map[string]any{"Node": u.n.Self.Name, "Q": q, "Hits": hits, "Cut": cut, "Cap": searchCap})
}

// create renders an empty editor; the name is chosen on save.
func (u *ui) create(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := ""
	if d := strings.Trim(r.URL.Query().Get("d"), "/"); d != "" {
		name = d + "/"
	}
	page(w, fileTmpl, map[string]any{"Node": u.n.Self.Name, "Name": name, "Content": "", "Hash": "", "New": true, "Edit": true})
}

// oldNote keeps links from before notes and files shared one namespace.
func (u *ui) oldNote(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	target := "/f/" + escPath(r.PathValue("name"))
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// kind classifies a held file for the preview: image, video, audio or other.
func kind(name string) string {
	switch store.Ext(name) {
	case "png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "heic":
		return "image"
	case "mp4", "webm", "mov", "m4v":
		return "video"
	case "mp3", "m4a", "ogg", "wav", "flac", "aac":
		return "audio"
	}
	return "other"
}

// file is the page of one file: rendered text with an editor, a media
// preview, a download, or a fetch button for content this node lacks.
func (u *ui) file(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := r.PathValue("name")
	if err := store.ValidName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	u.n.Store.Scan()
	m := u.n.Store.Get(name)
	if m == nil || m.Deleted {
		// The shorthand works in the address bar too: /f/todo -> /f/todo.md.
		if alt := u.n.Store.Resolve(name); alt != "" && alt != name && !u.n.Store.Get(alt).Deleted {
			u.n.Unlock()
			http.Redirect(w, r, "/f/"+escPath(alt), http.StatusFound)
			return
		}
		u.n.Unlock()
		http.NotFound(w, r)
		return
	}
	info := rowOf(m, path.Base(name))
	hash, versions := m.Hash, len(m.History)
	text, isText := u.n.Store.ReadText(name)
	u.n.Unlock()

	k := "stub"
	switch {
	case !info.Have:
	case kind(name) != "other":
		k = kind(name)
	case isText:
		k = "text"
	default:
		k = "other"
	}
	edit := r.URL.Query().Has("edit") && k == "text"
	data := map[string]any{
		"Node": u.n.Self.Name, "Name": name, "File": info, "Kind": k, "Edit": edit, "New": false,
		"Hash": hash, "Versions": versions, "Hub": u.n.Store.Config.Hub,
		"Crumbs": crumbs(path.Dir(name)), "Dir": strings.TrimPrefix(path.Dir(name), "."),
	}
	if path.Dir(name) == "." {
		data["Crumbs"] = []crumb(nil)
	}
	if k == "text" {
		if edit {
			data["Content"] = string(text)
		} else {
			data["HTML"] = render.HTML(name, text)
		}
	}
	page(w, fileTmpl, data)
}

// raw serves the bytes of a file. The response is sandboxed: a stored HTML
// or SVG file opened directly must not run script with this origin's
// rights (it could call the write endpoints).
func (u *ui) raw(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := r.PathValue("name")
	if err := store.ValidName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	var mod time.Time
	if m := u.n.Store.Get(name); m != nil {
		mod = m.ModTime
	}
	f, err := u.n.Store.OpenFile(name)
	u.n.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; media-src 'self'")
	disp := "inline"
	if r.URL.Query().Has("dl") {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": path.Base(name)}))
	http.ServeContent(w, r, "", mod, f)
}

// history lists a file's versions, or shows one when ?v=<seq> is given.
func (u *ui) history(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := r.PathValue("name")
	if err := store.ValidName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	u.n.Store.Scan()
	m := u.n.Store.Get(name)
	var ledger []store.Version
	deleted := false
	if m != nil {
		ledger = append(ledger, m.History...)
		deleted = m.Deleted
	}
	u.n.Unlock()
	if m == nil || len(ledger) == 0 {
		http.NotFound(w, r)
		return
	}
	// content returns what a version held; ok is false when it was not kept.
	content := func(i int) (data []byte, ok bool) {
		if i < 0 || ledger[i].Deleted {
			return nil, true
		}
		if ledger[i].Blob {
			return nil, false
		}
		data, err := u.n.Store.Snapshot(name, ledger[i].Hash)
		return data, err == nil
	}
	if v := r.URL.Query().Get("v"); v != "" {
		seq, err := strconv.Atoi(v)
		if err != nil || seq < 1 || seq > len(ledger) {
			http.NotFound(w, r)
			return
		}
		ver := ledger[seq-1]
		cur, kept := content(seq - 1)
		var diff template.HTML
		if prev, ok := content(seq - 2); ok && kept && seq > 1 {
			diff = diffHTML(prev, cur)
		}
		data := map[string]any{
			"Node": u.n.Self.Name, "Name": name, "Ver": ver, "Total": len(ledger), "Kept": kept && !ver.Deleted,
			"Current": seq == len(ledger) && !deleted, "Diff": diff, "Prev": seq - 1, "Size": store.FileSize(ver.Size),
		}
		if kept && !ver.Deleted {
			data["HTML"] = render.HTML(name, cur)
		}
		page(w, versionTmpl, data)
		return
	}
	// Newest first, with the size of each change where both sides are text.
	type verRow struct {
		store.Version
		Added, Removed int
		Size           string
	}
	vers := make([]verRow, 0, len(ledger))
	next, nextOK := content(len(ledger) - 1)
	for i := len(ledger) - 1; i >= 0; i-- {
		prev, prevOK := content(i - 1)
		vr := verRow{Version: ledger[i]}
		if ledger[i].Blob {
			vr.Size = store.FileSize(ledger[i].Size)
		}
		if nextOK && prevOK {
			vr.Added, vr.Removed = merge.Stat(prev, next)
		}
		vers = append(vers, vr)
		next, nextOK = prev, prevOK
	}
	page(w, historyTmpl, map[string]any{"Node": u.n.Self.Name, "Name": name, "Versions": vers, "Deleted": deleted})
}

// Browsers only send this header from same-origin fetch calls, so a form on
// a malicious site the phone visits cannot post edits here.
func guarded(r *http.Request) bool { return r.Header.Get("X-Requested-With") == "np" }

// body decodes a guarded JSON request; it answers the error itself.
func body(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if !guarded(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// restore makes a historical version the current content, as a new version.
func (u *ui) restore(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	var req struct {
		Name string `json:"name"`
		Seq  int    `json:"seq"`
	}
	if !body(w, r, 1<<20, &req) {
		return
	}
	u.n.Lock()
	defer u.n.Unlock()
	m := u.n.Store.Get(req.Name)
	if m == nil || req.Seq < 1 || req.Seq > len(m.History) || m.History[req.Seq-1].Deleted || m.History[req.Seq-1].Blob {
		http.Error(w, "no such version, or its content was not kept", http.StatusBadRequest)
		return
	}
	content, err := u.n.Store.Snapshot(req.Name, m.History[req.Seq-1].Hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := u.n.Store.WriteBy(req.Name, content, peer.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.n.Logf("%s restored %s to version %d via web", peer.Name, req.Name, req.Seq)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"ok": "1"})
}

type saveReq struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Base    string `json:"base"` // hash the editor started from; "" for new files
}

func (u *ui) save(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	var req saveReq
	if !body(w, r, 8<<20, &req) {
		return
	}
	name := strings.Trim(strings.TrimSpace(req.Name), "/")
	if req.Base == "" && name != "" {
		name = store.DefaultName(name) // a new file: "todo" becomes todo.md
	}
	if err := store.ValidName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	defer u.n.Unlock()
	u.n.Store.Scan()
	cur := u.n.Store.Get(name)
	curHash := ""
	if cur != nil && !cur.Deleted {
		curHash = cur.Hash
	}
	if req.Base == "" && curHash != "" {
		http.Error(w, "a file named "+name+" already exists", http.StatusConflict)
		return
	}
	if curHash != req.Base {
		http.Error(w, "the file changed since you opened it; reload and retry", http.StatusConflict)
		return
	}
	if err := u.n.Store.WriteBy(name, []byte(req.Content), peer.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.n.Logf("%s saved %s via web", peer.Name, name)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"name": name, "hash": u.n.Store.Get(name).Hash})
}

func (u *ui) appendText(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	var req saveReq
	if !body(w, r, 1<<20, &req) {
		return
	}
	name := strings.Trim(strings.TrimSpace(req.Name), "/")
	u.n.Lock()
	defer u.n.Unlock()
	u.n.Store.Scan()
	if found := u.n.Store.Resolve(name); found != "" {
		name = found
	} else if name != "" {
		name = store.DefaultName(name)
	}
	if err := u.n.Store.AppendBy(name, req.Content, peer.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Logf("%s appended to %s via web", peer.Name, name)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"name": name})
}

// rename moves a file to a new name.
func (u *ui) rename(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	var req struct {
		Name string `json:"name"`
		To   string `json:"to"`
	}
	if !body(w, r, 1<<20, &req) {
		return
	}
	to := store.RenameTarget(req.Name, strings.TrimSpace(req.To))
	u.n.Lock()
	defer u.n.Unlock()
	if err := u.n.Store.Rename(req.Name, to, peer.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Logf("%s renamed %s to %s via web", peer.Name, req.Name, to)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"name": to})
}

func (u *ui) del(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	var req saveReq
	if !body(w, r, 1<<20, &req) {
		return
	}
	u.n.Lock()
	defer u.n.Unlock()
	if err := u.n.Store.DeleteBy(req.Name, peer.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Logf("%s deleted %s via web", peer.Name, req.Name)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"ok": "1"})
}

// upload stores a multipart file field "file" under the optional "name"
// (defaults to the uploaded file's name, inside optional "dir"). Name and
// dir may come as query parameters or as form fields sent before the file.
// An existing file is only replaced when ?replace is given.
func (u *ui) upload(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	if !guarded(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	dir := strings.Trim(strings.TrimSpace(r.URL.Query().Get("dir")), "/")
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			http.Error(w, "no file in upload", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "name":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			name = strings.TrimSpace(string(b))
		case "dir":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			dir = strings.Trim(strings.TrimSpace(string(b)), "/")
		case "file":
			if name == "" {
				name = path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
			}
			if dir != "" {
				name = dir + "/" + name
			}
			if err := store.ValidName(name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			u.n.Lock()
			cur := u.n.Store.Get(name)
			exists := cur != nil && !cur.Deleted
			var perr error
			if !exists || r.URL.Query().Has("replace") {
				perr = u.n.Store.Put(name, part, peer.Name)
			}
			u.n.Unlock()
			if exists && !r.URL.Query().Has("replace") {
				http.Error(w, "a file named "+name+" already exists", http.StatusConflict)
				return
			}
			if perr != nil {
				http.Error(w, perr.Error(), http.StatusInternalServerError)
				return
			}
			u.n.Logf("%s uploaded %s via web", peer.Name, name)
			u.n.NotifyChanged()
			writeJSON(w, map[string]string{"name": name})
			return
		}
	}
}

func (u *ui) fetch(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	var req struct {
		Name string `json:"name"`
	}
	if !body(w, r, 1<<20, &req) {
		return
	}
	from, err := u.n.FetchFile(r.Context(), req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	u.n.Logf("%s fetched %s from %s via web", peer.Name, req.Name, from.Name)
	writeJSON(w, map[string]string{"from": from.Name})
}

// diffHTML renders the unified diff between a and b as coloured lines, or
// "" when they are equal.
func diffHTML(a, b []byte) template.HTML {
	hunks := merge.Hunks(a, b, 3)
	if len(hunks) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<pre class=diff>`)
	for _, h := range hunks {
		sb.WriteString(`<span class=hunk>` + template.HTMLEscapeString(h.Header()) + "</span>\n")
		for _, e := range h.Edits {
			cls, sign := "", " "
			switch e.Op {
			case merge.Add:
				cls, sign = "add", "+"
			case merge.Del:
				cls, sign = "del", "-"
			}
			if cls != "" {
				sb.WriteString(`<span class=` + cls + `>`)
			}
			sb.WriteString(sign + template.HTMLEscapeString(e.Text))
			if cls != "" {
				sb.WriteString(`</span>`)
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString(`</pre>`)
	return template.HTML(sb.String())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func page(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// escPath escapes a file name for the path of a URL, keeping the slashes,
// so names with spaces, '#', '?' or '%' make working links.
func escPath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

var funcs = template.FuncMap{"path": escPath}

func tmpl(name, body string) *template.Template {
	return template.Must(template.New(name).Funcs(funcs).Parse(head + body))
}

var css = `
:root{color-scheme:light dark;--fg:#1a1a1a;--bg:#fafaf7;--mute:#777;--line:#e2e0da;--acc:#2f6f4f;--danger:#b23b3b}
@media(prefers-color-scheme:dark){:root{--fg:#ececea;--bg:#161616;--mute:#999;--line:#2c2c2c;--acc:#7fc8a0;--danger:#e07a7a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 -apple-system,system-ui,sans-serif}
main{max-width:720px;margin:0 auto;padding:16px}header{display:flex;align-items:baseline;gap:12px;margin-bottom:16px}
header h1{font-size:18px;margin:0}header small{color:var(--mute)}header a{color:inherit;text-decoration:none}
.sp{flex:1}a.btn,button{font:inherit;padding:8px 14px;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg);cursor:pointer;text-decoration:none}
button.pri{background:var(--acc);border-color:var(--acc);color:#fff}button.danger{color:var(--danger)}
ul{list-style:none;padding:0;margin:0}li>a{display:flex;flex-wrap:wrap;align-items:baseline;gap:2px 12px;padding:12px 4px;border-bottom:1px solid var(--line);color:inherit;text-decoration:none}
li>a>span{overflow-wrap:anywhere;flex:1 1 11em}li small{color:var(--mute);white-space:nowrap;margin-left:auto}h3.dir{font-size:13px;color:var(--mute);margin:18px 0 0;padding:0 4px;font-weight:600}input,textarea{width:100%;font:inherit;padding:10px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit}
textarea{min-height:60vh;font-family:ui-monospace,Menlo,monospace;font-size:15px;resize:vertical}pre{white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,Menlo,monospace;font-size:15px}
.row{display:flex;gap:8px;margin:12px 0;flex-wrap:wrap}form.row{margin-top:24px;padding-top:16px;border-top:1px solid var(--line);flex-wrap:nowrap}#msg{color:var(--danger);min-height:1.5em}.empty{color:var(--mute);padding:24px 0;text-align:center}.empty a{color:var(--acc)}
.crumbs{margin:12px 4px 0;color:var(--mute);overflow-wrap:anywhere}.crumbs a{color:var(--acc);text-decoration:none}.crumbs b{color:var(--fg);font-weight:600}
.hit{padding:2px 4px 2px 16px;font-family:ui-monospace,Menlo,monospace;font-size:13px;color:var(--mute);overflow-wrap:anywhere}.hit small{margin-right:8px}li.res{padding-bottom:8px;border-bottom:1px solid var(--line)}li.res>a{border-bottom:0;padding-bottom:4px}
.md{line-height:1.6;word-break:break-word}.md h1{font-size:24px}.md h2{font-size:20px}.md h3{font-size:17px}.md h1,.md h2,.md h3{margin:20px 0 8px;line-height:1.3}
.md p{margin:0 0 12px}.md ul,.md ol{padding-left:24px;margin:0 0 12px}.md li{margin:2px 0}.md ul{list-style:disc}.md li.task-list-item{list-style:none;margin-left:-20px}
.md code{font-family:ui-monospace,Menlo,monospace;font-size:14px;background:rgba(127,127,127,.15);padding:1px 5px;border-radius:4px}
.md pre{background:rgba(127,127,127,.12);padding:12px;border-radius:8px;overflow-x:auto;font-size:14px}.md pre code{background:none;padding:0}
.md blockquote{margin:0 0 12px;padding:4px 14px;border-left:3px solid var(--line);color:var(--mute)}.md a{color:var(--acc)}
.md table{border-collapse:collapse;margin:0 0 12px;display:block;overflow-x:auto}.md th,.md td{border:1px solid var(--line);padding:6px 10px;text-align:left}
.md img{max-width:100%}.md hr{border:0;border-top:1px solid var(--line);margin:16px 0}
.ver{display:flex;gap:12px;padding:12px 4px;border-bottom:1px solid var(--line);align-items:baseline;flex-wrap:wrap}.ver a{color:inherit;text-decoration:none;flex:1}.ver small{color:var(--mute)}
.banner{background:rgba(127,127,127,.12);border-radius:8px;padding:10px 14px;margin-bottom:16px;display:flex;gap:12px;align-items:center;flex-wrap:wrap}.banner .sp{flex:1}.banner .crumbs{margin:0}.banner small{color:var(--mute)}
pre.diff{background:rgba(127,127,127,.1);padding:12px;border-radius:8px;overflow-x:auto;font-size:13px;line-height:1.45}pre.diff .add{color:#1a7f37;background:rgba(46,160,67,.15);display:inline-block;width:100%}pre.diff .del{color:#b23b3b;background:rgba(248,81,73,.15);display:inline-block;width:100%}pre.diff .hunk{color:var(--mute)}
@media(prefers-color-scheme:dark){pre.diff .add{color:#7ee787}pre.diff .del{color:#ffa198}}.stat{white-space:nowrap}.stat .add{color:#1a7f37}.stat .del{color:#b23b3b}
.prev{max-width:100%;border-radius:8px;display:block}.stub{color:var(--mute);font-size:13px}.up{margin-top:16px;display:flex;gap:8px;align-items:center}.up input[type=file]{flex:1;padding:6px}
.md .chroma{padding:12px;border-radius:8px;overflow-x:auto;font-family:ui-monospace,Menlo,monospace;font-size:14px}
` + render.CSS()

const head = `<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
{{define "row"}}<li data-n="{{.Label}}"><a href="/f/{{path .Name}}"><span>{{.Label}}{{if not .Have}} <span class=stub>not fetched</span>{{end}}</span><small>{{if .Blob}}{{.Size}} · {{end}}{{.By}} · {{.When.Local.Format "Jan 2 15:04"}}</small></a></li>{{end}}
{{define "js"}}function fu(p,n){return p+n.split('/').map(encodeURIComponent).join('/')}
function post(u,b){return fetch(u,{method:'POST',headers:{'Content-Type':'application/json','X-Requested-With':'np'},body:JSON.stringify(b)}).then(function(r){return r.ok?r.json():r.text().then(function(t){throw new Error(t)})})}
function fail(e){document.getElementById('msg').textContent=e.message}{{end}}`

var listTmpl = tmpl("list", `<title>np · {{.Node}}</title><style>`+css+`</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}} · you: {{.Who}}</small><span class=sp></span><a class=btn href="/new{{if .Dir}}?d={{.Dir}}{{end}}">New</a></header>
<form action="/search"><input id=q name=q type=search placeholder="filter · Enter searches content too" autocomplete=off oninput="f()"></form>
{{if .Dir}}<p class=crumbs><a href="/">all</a>{{range .Crumbs}} / <a href="/?d={{.Path}}">{{.Name}}</a>{{end}}</p>{{end}}
<div id=l>{{if .Recent}}<h3 class=dir>recently changed</h3><ul>{{range .Recent}}{{template "row" .}}{{end}}</ul><h3 class=dir>all files</h3>{{end}}
<ul>{{range .Dirs}}<li data-n="{{.Name}}"><a href="/?d={{.Path}}"><span>{{.Name}}/</span><small>{{.Count}}</small></a></li>{{end}}{{range .Rows}}{{template "row" .}}{{end}}</ul>
{{if .More}}<p class=empty><a href="/?d={{.Dir}}&all">show {{.More}} more</a></p>{{end}}{{if .Empty}}<p class=empty>nothing here yet</p>{{end}}</div>
<form class=up onsubmit="return up()"><input type=file id=uf multiple><button class=pri>Upload</button></form><div id=msg></div>
<script>{{template "js"}}
var dir={{.Dir}};
function up(){var fs=[].slice.call(document.getElementById('uf').files);if(!fs.length)return false;var m=document.getElementById('msg'),last='',i=0;
function next(){if(i>=fs.length){location.href=fs.length==1?fu('/f/',last):location.href;return}var f=fs[i++];m.textContent='uploading '+f.name+'…';var fd=new FormData();fd.append('file',f,f.name);
fetch('/web/upload?dir='+encodeURIComponent(dir),{method:'POST',headers:{'X-Requested-With':'np'},body:fd}).then(function(r){return r.ok?r.json():r.text().then(function(t){throw new Error(t)})}).then(function(j){last=j.name;next()}).catch(fail)}
next();return false}
function f(){var q=document.getElementById('q').value.toLowerCase();document.querySelectorAll('#l li').forEach(function(li){li.style.display=li.dataset.n.toLowerCase().includes(q)?'':'none'});document.querySelectorAll('#l h3').forEach(function(h){var ul=h.nextElementSibling;h.style.display=[].some.call(ul.children,function(li){return li.style.display!=='none'})?'':'none'})}</script>
</main>`)

var searchTmpl = tmpl("search", `<title>{{.Q}} · np</title><style>`+css+`</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}}</small></header>
<form action="/search"><input name=q type=search value="{{.Q}}" placeholder="search names and content" autofocus autocomplete=off></form>
<ul>{{range .Hits}}<li class=res><a href="/f/{{path .Name}}"><span>{{.Name}}</span></a>{{range .Lines}}<div class=hit><small>{{.N}}</small>{{.Text}}</div>{{end}}{{if .More}}<div class=hit><small>+{{.More}} more lines</small></div>{{end}}</li>{{end}}</ul>
{{if not .Hits}}<p class=empty>{{if .Q}}nothing matches{{else}}type something to search{{end}}</p>{{end}}{{if .Cut}}<p class=empty>showing the first {{.Cap}} files; narrow the search to see the rest</p>{{end}}
</main>`)

var fileTmpl = tmpl("file", `<title>{{if .New}}new{{else}}{{.Name}}{{end}} · np</title><style>`+css+`</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}}</small><span class=sp></span>
{{if not .Edit}}<a class=btn href="/h/{{path .Name}}">History{{if gt .Versions 1}} ({{.Versions}}){{end}}</a>{{if eq .Kind "text"}}<a class=btn href="?edit">Edit</a>{{end}}{{end}}</header>
{{if .Edit}}
<input id=name value="{{.Name}}" {{if not .New}}readonly{{else}}autofocus{{end}} placeholder="name: todo becomes todo.md; add .json, .sh, .yaml… for other types">
<div class=row></div>
<textarea id=c>{{.Content}}</textarea>
<div class=row><button class=pri onclick="save()">Save</button>{{if not .New}}<a class=btn href="/f/{{path .Name}}">Cancel</a>{{else}}<a class=btn href="/">Cancel</a>{{end}}</div>
<div id=msg></div>
<script>{{template "js"}}
var base={{.Hash}};
function save(){post('/web/save',{name:document.getElementById('name').value.trim(),content:document.getElementById('c').value,base:base}).then(function(j){location.href=fu('/f/',j.name)}).catch(fail)}
</script>
{{else}}
<div class=banner><span class=crumbs><a href="/">all</a>{{range .Crumbs}} / <a href="/?d={{.Path}}">{{.Name}}</a>{{end}} / <b>{{.File.Label}}</b></span><span class=sp></span><small>{{.File.Size}} · {{.File.By}} · {{.File.When.Local.Format "Jan 2 15:04"}}</small></div>
<div id=msg></div>
{{if eq .Kind "stub"}}<p class=empty>Not on this machine yet.</p>
<div class=row><button class=pri id=fb onclick="fetchIt()">Fetch{{if .Hub}} from {{.Hub}}{{end}}</button></div>
{{else if eq .Kind "text"}}<div class=md>{{.HTML}}</div>
<form class=row onsubmit="return add()"><input id=a placeholder="add a line…" autocomplete=off><button class=pri>Add</button></form>
{{else if eq .Kind "image"}}<img class=prev src="/raw/{{path .Name}}" alt="{{.Name}}">
{{else if eq .Kind "video"}}<video class=prev controls playsinline src="/raw/{{path .Name}}"></video>
{{else if eq .Kind "audio"}}<audio controls src="/raw/{{path .Name}}"></audio>
{{else}}<p class=empty>No preview for this type.</p>{{end}}
<div class=row style="margin-top:24px"><button onclick="mv()">Rename</button>{{if .File.Have}}<a class=btn href="/raw/{{path .Name}}?dl" download>Download</a>{{end}}<span class=sp></span><button class=danger onclick="del()">Delete</button></div>
<script>{{template "js"}}
var name={{.Name}},dir={{.Dir}};
function fetchIt(){var b=document.getElementById('fb');b.disabled=true;b.textContent='Fetching…';post('/web/fetch',{name:name}).then(function(){location.reload()}).catch(function(e){b.disabled=false;b.textContent='Fetch';fail(e)})}
function mv(){var to=prompt('New name for '+name+':',name);if(!to||to===name)return;post('/web/rename',{name:name,to:to}).then(function(j){location.href=fu('/f/',j.name)}).catch(fail)}
function del(){if(!confirm('Delete '+name+' on every machine?'))return;post('/web/delete',{name:name}).then(function(){location.href=dir?'/?d='+encodeURIComponent(dir):'/'}).catch(fail)}
function add(){var t=document.getElementById('a').value;if(!t.trim())return false;post('/web/append',{name:name,content:t}).then(function(){location.reload()}).catch(fail);return false}
</script>{{end}}
</main>`)

var historyTmpl = tmpl("history", `<title>{{.Name}} history · np</title><style>`+css+`</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}}</small><span class=sp></span>{{if not .Deleted}}<a class=btn href="/f/{{path .Name}}">Back</a>{{end}}</header>
<h2 style="margin:0 0 8px;font-size:20px;overflow-wrap:anywhere">{{.Name}}{{if .Deleted}} <small style="color:var(--mute);font-weight:400">deleted</small>{{end}}</h2>
{{range .Versions}}<div class=ver><a href="/h/{{path $.Name}}?v={{.Seq}}">v{{.Seq}}{{if .RenamedTo}} · renamed to {{.RenamedTo}}{{else if .Deleted}} · deleted{{else if .RenamedFrom}} · renamed from {{.RenamedFrom}}{{end}}</a><small class=stat>{{if .Size}}{{.Size}}{{end}}{{if .Added}}<span class=add>+{{.Added}}</span>{{end}} {{if .Removed}}<span class=del>−{{.Removed}}</span>{{end}}</small><small>{{.ModBy}}</small><small>{{.ModTime.Local.Format "Jan 2 15:04:05"}}</small><small>{{.Clock}}</small></div>{{end}}
</main>`)

var versionTmpl = tmpl("version", `<title>{{.Name}} v{{.Ver.Seq}} · np</title><style>`+css+`</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}}</small><span class=sp></span><a class=btn href="/h/{{path .Name}}">History</a><a class=btn href="/f/{{path .Name}}">Current</a></header>
<div class=banner><span>v{{.Ver.Seq}} of {{.Total}} · {{.Ver.ModBy}} · {{.Ver.ModTime.Local.Format "Jan 2 15:04:05"}}</span><span class=sp></span>
{{if .Ver.Deleted}}<span>deleted</span>{{else if .Current}}<span>current</span>{{else if .Kept}}<button class=pri onclick="restore()">Restore this version</button>{{end}}</div>
<div id=msg></div>
{{if .Diff}}<details {{if not .Current}}open{{end}}><summary style="cursor:pointer;color:var(--mute);margin:0 0 8px">Changes from v{{.Prev}}</summary>{{.Diff}}</details>{{end}}
{{if .Kept}}<div class=md>{{.HTML}}</div>{{else if not .Ver.Deleted}}<p class=empty>The content of this version was not kept ({{.Size}}; binary or large files have no history).</p>{{end}}
<script>{{template "js"}}
function restore(){if(!confirm('Make v{{.Ver.Seq}} the current version of '+{{.Name}}+'?'))return;post('/web/restore',{name:{{.Name}},seq:{{.Ver.Seq}}}).then(function(){location.href=fu('/f/',{{.Name}})}).catch(fail)}</script>
</main>`)
