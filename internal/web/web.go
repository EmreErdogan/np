// Package web serves a small phone-friendly UI on the daemon: list, read,
// edit, create and delete notes. Callers are identified by Tailscale WhoIs
// exactly like the sync API, so anyone who can open the page is a tailnet
// member the config allows.
package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"github.com/EmreErdogan/np/internal/proto"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

// Mount registers the UI routes on mux.
func Mount(n *proto.Node) func(mux *http.ServeMux) {
	w := &ui{n: n}
	return func(mux *http.ServeMux) {
		mux.HandleFunc("GET /{$}", n.Auth(w.list))
		mux.HandleFunc("GET /new", n.Auth(w.create))
		mux.HandleFunc("GET /n/{name...}", n.Auth(w.note))
		mux.HandleFunc("POST /web/save", n.Auth(w.save))
		mux.HandleFunc("POST /web/delete", n.Auth(w.del))
	}
}

type ui struct{ n *proto.Node }

// md renders GitHub-flavoured markdown. Raw HTML in notes is escaped, not
// rendered, so a note cannot inject script into the page.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Typographer),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

func renderMarkdown(src []byte) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	return template.HTML(buf.String())
}

func (u *ui) list(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	u.n.Lock()
	u.n.Store.Scan()
	notes := u.n.Store.List(false)
	u.n.Unlock()
	render(w, listTmpl, map[string]any{"Node": u.n.Self.Name, "Notes": notes, "Who": peer.Name})
}

// create renders an empty editor; the name is chosen on save.
func (u *ui) create(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	render(w, noteTmpl, map[string]any{"Node": u.n.Self.Name, "Name": "", "Content": "", "Hash": "", "New": true, "Edit": true})
}

func (u *ui) note(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := r.PathValue("name")
	if err := store.ValidName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	u.n.Store.Scan()
	m := u.n.Store.Get(name)
	var content []byte
	if m != nil && !m.Deleted {
		content, _ = u.n.Store.Read(name)
	}
	u.n.Unlock()
	hash := ""
	if m != nil && !m.Deleted {
		hash = m.Hash
	}
	edit := r.URL.Query().Has("edit") || hash == ""
	data := map[string]any{
		"Node": u.n.Self.Name, "Name": name, "Content": string(content),
		"Hash": hash, "New": hash == "", "Edit": edit,
	}
	if !edit {
		data["HTML"] = renderMarkdown(content)
	}
	render(w, noteTmpl, data)
}

type saveReq struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Base    string `json:"base"` // hash the editor started from; "" for new notes
}

// Browsers only send this header from same-origin fetch calls, so a form on
// a malicious site the phone visits cannot post edits here.
func guarded(r *http.Request) bool { return r.Header.Get("X-Requested-With") == "np" }

func (u *ui) save(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	if !guarded(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req saveReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := store.ValidName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	defer u.n.Unlock()
	u.n.Store.Scan()
	cur := u.n.Store.Get(req.Name)
	curHash := ""
	if cur != nil && !cur.Deleted {
		curHash = cur.Hash
	}
	if req.Base == "" && curHash != "" {
		http.Error(w, "a note named "+req.Name+" already exists", http.StatusConflict)
		return
	}
	if curHash != req.Base {
		http.Error(w, "note changed since you opened it; reload and retry", http.StatusConflict)
		return
	}
	if err := u.n.Store.WriteBy(req.Name, []byte(req.Content), peer.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.n.Logf("%s saved %s via web", peer.Name, req.Name)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"hash": u.n.Store.Get(req.Name).Hash})
}

func (u *ui) del(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	if !guarded(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req saveReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

const css = `
:root{color-scheme:light dark;--fg:#1a1a1a;--bg:#fafaf7;--mute:#777;--line:#e2e0da;--acc:#2f6f4f;--danger:#b23b3b}
@media(prefers-color-scheme:dark){:root{--fg:#ececea;--bg:#161616;--mute:#999;--line:#2c2c2c;--acc:#7fc8a0;--danger:#e07a7a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 -apple-system,system-ui,sans-serif}
main{max-width:720px;margin:0 auto;padding:16px}header{display:flex;align-items:baseline;gap:12px;margin-bottom:16px}
header h1{font-size:18px;margin:0}header small{color:var(--mute)}header a{color:inherit;text-decoration:none}
.sp{flex:1}a.btn,button{font:inherit;padding:8px 14px;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg);cursor:pointer;text-decoration:none}
button.pri{background:var(--acc);border-color:var(--acc);color:#fff}button.danger{color:var(--danger)}
ul{list-style:none;padding:0;margin:0}li a{display:flex;justify-content:space-between;gap:12px;padding:12px 4px;border-bottom:1px solid var(--line);color:inherit;text-decoration:none}
li small{color:var(--mute);white-space:nowrap}input,textarea{width:100%;font:inherit;padding:10px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit}
textarea{min-height:60vh;font-family:ui-monospace,Menlo,monospace;font-size:15px;resize:vertical}pre{white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,Menlo,monospace;font-size:15px}
.row{display:flex;gap:8px;margin:12px 0}#msg{color:var(--danger);min-height:1.5em}.empty{color:var(--mute);padding:24px 0;text-align:center}
.md{line-height:1.6;word-break:break-word}.md h1{font-size:24px}.md h2{font-size:20px}.md h3{font-size:17px}.md h1,.md h2,.md h3{margin:20px 0 8px;line-height:1.3}
.md p{margin:0 0 12px}.md ul,.md ol{padding-left:24px;margin:0 0 12px}.md li{margin:2px 0}.md li.task-list-item{list-style:none;margin-left:-20px}
.md code{font-family:ui-monospace,Menlo,monospace;font-size:14px;background:rgba(127,127,127,.15);padding:1px 5px;border-radius:4px}
.md pre{background:rgba(127,127,127,.12);padding:12px;border-radius:8px;overflow-x:auto;font-size:14px}.md pre code{background:none;padding:0}
.md blockquote{margin:0 0 12px;padding:4px 14px;border-left:3px solid var(--line);color:var(--mute)}.md a{color:var(--acc)}
.md table{border-collapse:collapse;margin:0 0 12px;display:block;overflow-x:auto}.md th,.md td{border:1px solid var(--line);padding:6px 10px;text-align:left}
.md img{max-width:100%}.md hr{border:0;border-top:1px solid var(--line);margin:16px 0}
`

var listTmpl = template.Must(template.New("list").Parse(`<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>np · {{.Node}}</title><style>` + css + `</style><main>
<header><h1>np</h1><small>{{.Node}} · you: {{.Who}}</small><span class=sp></span><a class=btn href="/new">New</a></header>
<input id=q placeholder="filter" autofocus oninput="f()">
<ul id=l>{{range .Notes}}<li><a href="/n/{{.Name}}"><span>{{.Name}}</span><small>{{.ModBy}} · {{.ModTime.Local.Format "Jan 2 15:04"}}</small></a></li>{{else}}<li class=empty>no notes yet</li>{{end}}</ul>
<script>function f(){var q=document.getElementById('q').value.toLowerCase();document.querySelectorAll('#l li').forEach(function(li){li.style.display=li.textContent.toLowerCase().includes(q)?'':'none'})}</script>
</main>`))

var noteTmpl = template.Must(template.New("note").Parse(`<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>{{if .New}}new{{else}}{{.Name}}{{end}} · np</title><style>` + css + `</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}}</small><span class=sp></span>
{{if not .Edit}}<a class=btn href="?edit">Edit</a>{{end}}</header>
{{if .Edit}}
<input id=name value="{{.Name}}" {{if not .New}}readonly{{else}}autofocus{{end}} placeholder="note name">
<div class=row></div>
<textarea id=c>{{.Content}}</textarea>
<div class=row><button class=pri onclick="save()">Save</button>{{if not .New}}<a class=btn href="/n/{{.Name}}">Cancel</a><span class=sp></span><button class=danger onclick="del()">Delete</button>{{end}}</div>
<div id=msg></div>
<script>
var base={{.Hash}};
function post(u,b){return fetch(u,{method:'POST',headers:{'Content-Type':'application/json','X-Requested-With':'np'},body:JSON.stringify(b)}).then(function(r){return r.ok?r.json():r.text().then(function(t){throw new Error(t)})})}
function save(){var n=document.getElementById('name').value.trim();post('/web/save',{name:n,content:document.getElementById('c').value,base:base}).then(function(){location.href='/n/'+n}).catch(function(e){document.getElementById('msg').textContent=e.message})}
function del(){if(!confirm('Delete {{.Name}}?'))return;post('/web/delete',{name:{{.Name}}}).then(function(){location.href='/'}).catch(function(e){document.getElementById('msg').textContent=e.message})}
</script>
{{else}}<div class=md>{{.HTML}}</div>{{end}}
</main>`))
