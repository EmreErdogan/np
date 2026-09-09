package web

import (
	"encoding/json"
	"html/template"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/EmreErdogan/np/internal/merge"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
)

// Files in the UI: a section on the list page with an upload box, a page per
// file that previews images/video/audio/text or offers a download, and a
// fetch button for files this node only knows about.

const maxUpload = 2 << 30

func mountFiles(u *ui, mux *http.ServeMux) {
	n := u.n
	mux.HandleFunc("GET /f/{name...}", n.Auth(u.file))
	mux.HandleFunc("GET /raw/{name...}", n.Auth(u.raw))
	mux.HandleFunc("POST /web/upload", n.Auth(u.upload))
	mux.HandleFunc("POST /web/fetch", n.Auth(u.fetch))
	mux.HandleFunc("POST /web/delete-file", n.Auth(u.deleteFile))
}

// fileRow is a file as shown in lists.
type fileRow struct {
	*store.FileMeta
	SizeText string
}

func fileRows(files []*store.FileMeta) []fileRow {
	out := make([]fileRow, len(files))
	for i, m := range files {
		out[i] = fileRow{m, store.FileSize(m.Size)}
	}
	return out
}

// kind classifies a file for the preview: image, video, audio, text, other.
func kind(name string, m *store.FileMeta, peek []byte) string {
	switch strings.ToLower(strings.TrimPrefix(path.Ext(name), ".")) {
	case "png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "heic":
		return "image"
	case "mp4", "webm", "mov", "m4v":
		return "video"
	case "mp3", "m4a", "ogg", "wav", "flac", "aac":
		return "audio"
	}
	if m.Size <= 256<<10 && len(peek) > 0 && merge.IsText(peek) {
		return "text"
	}
	return "other"
}

func (u *ui) file(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := r.PathValue("name")
	if err := store.ValidFileName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	u.n.Store.Scan()
	m := u.n.Store.File(name)
	var peek []byte
	if m != nil && m.Have && !m.Deleted {
		if f, err := u.n.Store.OpenFile(name); err == nil {
			peek, _ = io.ReadAll(io.LimitReader(f, 256<<10))
			f.Close()
		}
	}
	u.n.Unlock()
	if m == nil || m.Deleted {
		http.NotFound(w, r)
		return
	}
	k := "stub"
	text := ""
	if m.Have {
		k = kind(name, m, peek)
		if k == "text" {
			text = string(peek)
		}
	}
	page(w, fileTmpl, map[string]any{
		"Node": u.n.Self.Name, "Name": name, "File": fileRow{m, store.FileSize(m.Size)},
		"Kind": k, "Text": text, "Hub": u.n.Store.Config.Hub,
	})
}

func (u *ui) raw(w http.ResponseWriter, r *http.Request, _ ts.Peer) {
	name := r.PathValue("name")
	if err := store.ValidFileName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	m := u.n.Store.File(name)
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
	disp := "inline"
	if r.URL.Query().Has("dl") {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", disp+`; filename="`+path.Base(name)+`"`)
	http.ServeContent(w, r, "", m.ModTime, f)
}

// upload stores a multipart file field "file" under the optional "name"
// (defaults to the uploaded file's name, inside optional "dir"). Name and
// dir may come as query parameters or as form fields sent before the file.
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
				name = path.Base(part.FileName())
			}
			if dir != "" {
				name = dir + "/" + name
			}
			if err := store.ValidFileName(name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			u.n.Lock()
			exists := u.n.Store.File(name) != nil && !u.n.Store.File(name).Deleted
			var perr error
			if exists && !r.URL.Query().Has("replace") {
				perr = errExists
			} else {
				perr = u.n.Store.PutFile(name, part, peer.Name)
			}
			u.n.Unlock()
			if perr == errExists {
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

var errExists = &existsError{}

type existsError struct{}

func (*existsError) Error() string { return "exists" }

func (u *ui) fetch(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	if !guarded(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

func (u *ui) deleteFile(w http.ResponseWriter, r *http.Request, peer ts.Peer) {
	if !guarded(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Lock()
	err := u.n.Store.DeleteFile(req.Name, peer.Name)
	u.n.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.n.Logf("%s deleted file %s via web", peer.Name, req.Name)
	u.n.NotifyChanged()
	writeJSON(w, map[string]string{"ok": "1"})
}

var fileTmpl = template.Must(template.New("file").Parse(`<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>{{.Name}} · np</title><style>` + css + `</style><main>
<header><h1><a href="/">np</a></h1><small>{{.Node}}</small><span class=sp></span>
{{if .File.Have}}<a class=btn href="/raw/{{.Name}}?dl" download>Download</a>{{end}}</header>
<div class=banner><span>{{.Name}}</span><span class=sp></span><small>{{.File.SizeText}} · {{.File.ModBy}} · {{.File.ModTime.Local.Format "Jan 2 15:04"}}</small></div>
<div id=msg></div>
{{if eq .Kind "stub"}}<p class=empty>Not on this machine yet.</p>
<div class=row><button class=pri id=fb onclick="fetchIt()">Fetch{{if .Hub}} from {{.Hub}}{{end}}</button></div>
{{else if eq .Kind "image"}}<img class=prev src="/raw/{{.Name}}" alt="{{.Name}}">
{{else if eq .Kind "video"}}<video class=prev controls playsinline src="/raw/{{.Name}}"></video>
{{else if eq .Kind "audio"}}<audio controls src="/raw/{{.Name}}"></audio>
{{else if eq .Kind "text"}}<pre>{{.Text}}</pre>
{{else}}<p class=empty>No preview for this type.</p>{{end}}
<div class=row style="margin-top:24px"><span class=sp></span><button class=danger onclick="del()">Delete</button></div>
<script>
function post(u,b){return fetch(u,{method:'POST',headers:{'Content-Type':'application/json','X-Requested-With':'np'},body:JSON.stringify(b)}).then(function(r){return r.ok?r.json():r.text().then(function(t){throw new Error(t)})})}
function fetchIt(){var b=document.getElementById('fb');b.disabled=true;b.textContent='Fetching…';post('/web/fetch',{name:{{.Name}}}).then(function(){location.reload()}).catch(function(e){b.disabled=false;b.textContent='Fetch';document.getElementById('msg').textContent=e.message})}
function del(){if(!confirm('Delete {{.Name}} everywhere?'))return;post('/web/delete-file',{name:{{.Name}}}).then(function(){location.href='/'}).catch(function(e){document.getElementById('msg').textContent=e.message})}
</script>
</main>`))
