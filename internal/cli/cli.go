// Package cli implements the np command surface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/EmreErdogan/np/internal/proto"
	"github.com/EmreErdogan/np/internal/render"
	"github.com/EmreErdogan/np/internal/service"
	"github.com/EmreErdogan/np/internal/store"
	"github.com/EmreErdogan/np/internal/ts"
	"github.com/EmreErdogan/np/internal/upgrade"
	"github.com/EmreErdogan/np/internal/web"
)

const usage = `np - share notes across your tailnet

Notes:
  np new <name>          create a note in $EDITOR (or from stdin)
  np edit <name>         edit a note
  np add [-t] <name> [text]
                         append text (or stdin, or an editor buffer) to a note;
                         -t prefixes a timestamp. Creates the note if missing.
  np ls [dir] [--local]  list notes as a tree, optionally under dir; --local skips the hub
  np ls --flat           plain list
  np cat <name>          print a note
  np view <name>         render a note in the terminal (markdown or code)
  np search <text>       find notes whose name or content contains text
  np rm <name>           delete a note (tombstone syncs to peers)
  np log <name>          version history
  np show <name> <seq>   print a historical version

Files (any type; synced lazily):
  np put <path> [name]   copy a file in (name defaults to the file's base name;
                         use dir/name to place it in a folder)
  np get <name> [-o <path>]
                         fetch a file's content from the hub or a peer that has
                         it; -o also copies it to path
  np drop <name>         remove the local copy but keep knowing about the file
  np rm <name>           delete a file (when no note has that name)
  Files are listed at the end of np ls. Every machine learns about every
  file; content below auto_fetch_bytes (2 MB, config) follows automatically,
  larger files only on np get, in the web UI, or on nodes with keep_all.

Sync:
  np peers               tailnet machines and whether they run np
  np hub [<peer>|none]   show or set the default sync peer
  np sync [<peer>]       sync with hub (or the given peer)
  np sync --all          sync with every online peer running np
  np serve               accept syncs and serve the web UI (foreground)
  np daemon              serve, auto-sync with hub (or all peers), fan out changes
  np status              local identity, hub, note count
  np version
  np upgrade [--check]   install the latest release from GitHub

The daemon also serves a phone-friendly web UI at http://<tailscale-ip>:7373/
for reading and editing notes from any device on the tailnet.

Service (runs "np daemon" in the background at login):
  np service install | uninstall | status

Notes live in ~/.np/notes and files in ~/.np/files (override with NP_DIR). A name
without an extension is markdown ("todo" -> todo.md); names like config.json
or deploy.sh are kept as-is and shown as code. Any editor works; np records
external edits on the next command.
`

// Version is set at build time via -ldflags "-X .../cli.Version=v1.2.3".
var Version = "dev"

// Run executes the command line and returns an exit code.
func Run(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("np", Version)
		return 0
	}
	if args[0] == "upgrade" { // works without tailscale
		if err := cmdUpgrade(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "np:", err)
			return 1
		}
		return 0
	}
	if err := run(args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "np:", err)
		return 1
	}
	return 0
}

func run(cmd string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := openNode(ctx)
	if err != nil {
		return err
	}
	switch cmd {
	case "new", "edit":
		return cmdEdit(n, args, cmd == "new")
	case "add", "append":
		return cmdAdd(n, args)
	case "ls", "list":
		return cmdList(ctx, n, args)
	case "search", "grep":
		return cmdSearch(n, args)
	case "cat":
		return cmdCat(n, args)
	case "view", "show-rendered":
		return cmdView(n, args)
	case "rm", "delete":
		return cmdRm(n, args)
	case "put":
		return cmdPut(n, args)
	case "get":
		return cmdGet(ctx, n, args)
	case "drop":
		return cmdDrop(n, args)
	case "log":
		return cmdLog(n, args)
	case "show":
		return cmdShow(n, args)
	case "peers":
		return cmdPeers(ctx, n)
	case "hub":
		return cmdHub(ctx, n, args)
	case "sync":
		return cmdSync(ctx, n, args)
	case "serve":
		return n.Serve(ctx)
	case "daemon":
		return cmdDaemon(ctx, n)
	case "status":
		return cmdStatus(ctx, n)
	case "service":
		return cmdService(n, args)
	default:
		return fmt.Errorf("unknown command %q (try `np help`)", cmd)
	}
}

func openNode(ctx context.Context) (*proto.Node, error) {
	dir, err := store.DefaultDir()
	if err != nil {
		return nil, err
	}
	self, err := ts.Self(ctx)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(dir, self.Name)
	if err != nil {
		return nil, err
	}
	n := &proto.Node{Store: s, Self: self, Log: log.New(os.Stderr, "", log.Ltime)}
	n.Mount = []func(*http.ServeMux){web.Mount(n)}
	return n, nil
}

func oneArg(args []string, what string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("expected %s", what)
	}
	if what == "<name>" {
		return store.Canon(args[0]), nil
	}
	return args[0], nil
}

func cmdEdit(n *proto.Node, args []string, create bool) error {
	name, err := oneArg(args, "<name>")
	if err != nil {
		return err
	}
	if err := store.ValidName(name); err != nil {
		return err
	}
	n.Store.Scan()
	m := n.Store.Get(name)
	exists := m != nil && !m.Deleted
	if !create && !exists {
		return fmt.Errorf("no note %q (use `np new %s`)", name, name)
	}
	if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice == 0 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		return n.Store.Write(name, data)
	}
	p, err := n.Store.Path(name)
	if err != nil {
		return err
	}
	if !exists {
		if err := os.WriteFile(p, []byte("# "+name+"\n\n"), 0o644); err != nil {
			return err
		}
	}
	if err := runEditor(p); err != nil {
		return err
	}
	changed, err := n.Store.Scan()
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Println("no changes")
	} else {
		fmt.Printf("saved %s (%s)\n", name, n.Store.Get(name).Clock)
	}
	return nil
}

// hubStates fetches the hub's index and classifies local notes against it.
// Returns nil (and a reason) when there is no hub or it cannot be reached.
func hubStates(ctx context.Context, n *proto.Node) (notes, files map[string]proto.SyncState, reason string) {
	if n.Store.Config.Hub == "" {
		return nil, nil, "no hub set"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p, err := targetPeer(ctx, n, nil)
	if err != nil {
		return nil, nil, err.Error()
	}
	remote, err := n.RemoteIndex(ctx, p)
	if err != nil {
		return nil, nil, fmt.Sprintf("hub %s unreachable", p.Name)
	}
	remoteFiles, err := n.RemoteFiles(ctx, p)
	if err != nil {
		return nil, nil, fmt.Sprintf("hub %s unreachable", p.Name)
	}
	return proto.Compare(n.Store.List(true), remote), proto.CompareFiles(n.Store.Files(true), remoteFiles), ""
}

// stdinPiped reports whether stdin carries data rather than a terminal.
func stdinPiped() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}

func editorCmd() string {
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if e := os.Getenv(v); e != "" {
			return e
		}
	}
	return "vi"
}

func runEditor(path string) error {
	parts := strings.Fields(editorCmd())
	c := exec.Command(parts[0], append(parts[1:], path)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", parts[0], err)
	}
	return nil
}

func cmdAdd(n *proto.Node, args []string) error {
	stamp := false
	if len(args) > 0 && (args[0] == "-t" || args[0] == "--time") {
		stamp, args = true, args[1:]
	}
	if len(args) == 0 {
		return errors.New("expected <name> [text]")
	}
	name := store.Canon(args[0])
	if err := store.ValidName(name); err != nil {
		return err
	}
	var text string
	switch {
	case len(args) > 1:
		text = strings.Join(args[1:], " ")
	case stdinPiped():
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	default:
		tmp, err := os.CreateTemp("", "np-add-*.md")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		if err := runEditor(tmp.Name()); err != nil {
			return err
		}
		b, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		fmt.Println("nothing added")
		return nil
	}
	if stamp {
		text = time.Now().Format("2006-01-02 15:04") + " " + strings.TrimLeft(text, " ")
	}
	if err := n.Store.Append(name, text); err != nil {
		return err
	}
	fmt.Printf("added to %s (%s)\n", name, n.Store.Get(name).Clock)
	return nil
}

func cmdList(ctx context.Context, n *proto.Node, args []string) error {
	if _, err := n.Store.Scan(); err != nil {
		return err
	}
	local, flat, prefix := false, false, ""
	for _, a := range args {
		switch a {
		case "--local":
			local = true
		case "--flat":
			flat = true
		default:
			prefix = strings.TrimSuffix(a, "/")
		}
	}
	var states, fileStates map[string]proto.SyncState
	reason := "skipped"
	if !local {
		states, fileStates, reason = hubStates(ctx, n)
	}
	// Build the row set: local notes plus hub-only notes.
	type row struct {
		name, when, by, state string
	}
	var rows []row
	listed := map[string]bool{}
	for _, m := range n.Store.List(false) {
		listed[m.Name] = true
		st := ""
		if states != nil {
			st = string(states[m.Name])
		}
		rows = append(rows, row{m.Name, m.ModTime.Local().Format("2006-01-02 15:04"), m.ModBy, st})
	}
	for name, st := range states {
		if !listed[name] && st == proto.Behind {
			rows = append(rows, row{name, "", "", "behind (hub only)"})
		}
	}
	if prefix != "" {
		var kept []row
		for _, r := range rows {
			if r.name == prefix || strings.HasPrefix(r.name, prefix+"/") {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	// Directories first, then notes, at every level.
	sort.Slice(rows, func(i, j int) bool { return treeLess(rows[i].name, rows[j].name) })

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	lastDir := ""
	for _, r := range rows {
		name, indent := r.name, ""
		if !flat {
			dir := ""
			if i := strings.LastIndex(r.name, "/"); i >= 0 {
				dir, name = r.name[:i], r.name[i+1:]
			}
			if dir != lastDir {
				printDirHeaders(tw, lastDir, dir)
				lastDir = dir
			}
			indent = strings.Repeat("  ", strings.Count(dir, "/")+boolInt(dir != ""))
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\n", indent, name, r.when, r.by, r.state)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := listFiles(n, prefix, flat, fileStates, states != nil || local); err != nil {
		return err
	}
	if states == nil && reason != "skipped" {
		fmt.Fprintln(os.Stderr, "sync state unavailable:", reason)
	}
	return nil
}

// listFiles prints the files block of np ls: name, size, date, author, and
// the hub state ("not fetched" when only the metadata is here).
func listFiles(n *proto.Node, prefix string, flat bool, states map[string]proto.SyncState, haveStates bool) error {
	type row struct {
		name, size, when, by, state string
	}
	var rows []row
	listed := map[string]bool{}
	for _, m := range n.Store.Files(false) {
		listed[m.Name] = true
		st := ""
		if states != nil {
			st = string(states[m.Name])
		}
		if !m.Have {
			if st != "" {
				st += ", "
			}
			st += "not fetched"
		}
		rows = append(rows, row{m.Name, store.FileSize(m.Size), m.ModTime.Local().Format("2006-01-02 15:04"), m.ModBy, st})
	}
	for name, st := range states {
		if !listed[name] && st == proto.Behind {
			rows = append(rows, row{name, "", "", "", "behind (hub only)"})
		}
	}
	if prefix != "" {
		var kept []row
		for _, r := range rows {
			if r.name == prefix || strings.HasPrefix(r.name, prefix+"/") {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return treeLess(rows[i].name, rows[j].name) })
	fmt.Println()
	fmt.Println("files:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	lastDir := ""
	for _, r := range rows {
		name, indent := r.name, ""
		if !flat {
			dir := ""
			if i := strings.LastIndex(r.name, "/"); i >= 0 {
				dir, name = r.name[:i], r.name[i+1:]
			}
			if dir != lastDir {
				printDirHeaders(tw, lastDir, dir)
				lastDir = dir
			}
			indent = strings.Repeat("  ", strings.Count(dir, "/")+boolInt(dir != ""))
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t%s\n", indent, name, r.size, r.when, r.by, r.state)
	}
	return tw.Flush()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// printDirHeaders prints the directory lines needed to get from prev to dir.
func printDirHeaders(w io.Writer, prev, dir string) {
	if dir == "" {
		return
	}
	parts := strings.Split(dir, "/")
	prevParts := strings.Split(prev, "/")
	if prev == "" {
		prevParts = nil
	}
	common := 0
	for common < len(parts) && common < len(prevParts) && parts[common] == prevParts[common] {
		common++
	}
	for i := common; i < len(parts); i++ {
		fmt.Fprintf(w, "%s%s/\t\t\t\n", strings.Repeat("  ", i), parts[i])
	}
}

// treeLess orders paths so that, within each directory, subdirectories come
// before notes and everything is alphabetical.
func treeLess(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		aDir, bDir := i < len(as)-1, i < len(bs)-1
		if as[i] == bs[i] && aDir && bDir {
			continue
		}
		if aDir != bDir {
			return aDir
		}
		return as[i] < bs[i]
	}
	return len(as) < len(bs)
}

func cmdSearch(n *proto.Node, args []string) error {
	if len(args) == 0 {
		return errors.New("expected <text>")
	}
	needle := strings.ToLower(strings.Join(args, " "))
	if _, err := n.Store.Scan(); err != nil {
		return err
	}
	found := 0
	for _, m := range n.Store.List(false) {
		data, err := n.Store.Read(m.Name)
		if err != nil {
			return err
		}
		nameHit := strings.Contains(strings.ToLower(m.Name), needle)
		var hits []string
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), needle) {
				hits = append(hits, fmt.Sprintf("  %d: %s", i+1, strings.TrimSpace(line)))
			}
		}
		if !nameHit && len(hits) == 0 {
			continue
		}
		found++
		fmt.Println(m.Name)
		for _, h := range hits {
			fmt.Println(h)
		}
	}
	if found == 0 {
		fmt.Fprintln(os.Stderr, "no matches")
	}
	return nil
}

func cmdCat(n *proto.Node, args []string) error {
	name, err := oneArg(args, "<name>")
	if err != nil {
		return err
	}
	data, err := n.Store.Read(name)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func cmdView(n *proto.Node, args []string) error {
	name, err := oneArg(args, "<name>")
	if err != nil {
		return err
	}
	data, err := n.Store.Read(name)
	if err != nil {
		return err
	}
	if fi, _ := os.Stdout.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice == 0 {
		_, err = os.Stdout.Write(data) // piped: no escape codes
		return err
	}
	width := 100
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 20 {
		width = c
	}
	dark := os.Getenv("NP_THEME") != "light"
	return render.Terminal(os.Stdout, name, data, width, dark)
}

func cmdRm(n *proto.Node, args []string) error {
	if len(args) != 1 {
		return errors.New("expected <name>")
	}
	n.Store.Scan()
	name := store.Canon(args[0])
	if m := n.Store.Get(name); m != nil && !m.Deleted {
		return n.Store.Delete(name)
	}
	if m := n.Store.File(args[0]); m != nil && !m.Deleted {
		return n.Store.DeleteFile(args[0], n.Self.Name)
	}
	return fmt.Errorf("no note or file %q", args[0])
}

func cmdPut(n *proto.Node, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("expected <path> [name]")
	}
	src, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	name := filepath.Base(args[0])
	if len(args) == 2 {
		if strings.HasSuffix(args[1], "/") { // a folder: keep the base name
			name = strings.Trim(args[1], "/") + "/" + name
		} else {
			name = strings.Trim(args[1], "/")
		}
	}
	if err := store.ValidFileName(name); err != nil {
		return err
	}
	if err := n.Store.PutFile(name, src, n.Self.Name); err != nil {
		return err
	}
	m := n.Store.File(name)
	fmt.Printf("%s (%s)\n", name, store.FileSize(m.Size))
	return nil
}

func cmdGet(ctx context.Context, n *proto.Node, args []string) error {
	name, out := "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o" && i+1 < len(args):
			out = args[i+1]
			i++
		case name == "":
			name = args[i]
		default:
			return errors.New("expected <name> [-o <path>]")
		}
	}
	if name == "" {
		return errors.New("expected <name> [-o <path>]")
	}
	n.Store.Scan()
	m := n.Store.File(name)
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if !m.Have {
		from, err := n.FetchFile(ctx, name)
		if err != nil {
			return err
		}
		fmt.Printf("fetched %s (%s) from %s\n", name, store.FileSize(m.Size), from.Name)
	}
	if out == "" {
		if m.Have {
			fmt.Println(filepath.Join(n.Store.Dir, "files", filepath.FromSlash(name)))
		}
		return nil
	}
	if st, err := os.Stat(out); err == nil && st.IsDir() {
		out = filepath.Join(out, filepath.Base(name))
	}
	f, err := n.Store.OpenFile(name)
	if err != nil {
		return err
	}
	defer f.Close()
	dst, err := os.Create(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, f); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func cmdDrop(n *proto.Node, args []string) error {
	name, err := oneArg(args, "<file>")
	if err != nil {
		return err
	}
	n.Store.Scan()
	m := n.Store.File(name)
	if m == nil || m.Deleted {
		return fmt.Errorf("no file %q", name)
	}
	if !m.Have {
		fmt.Println(name, "is not on this machine")
		return nil
	}
	return n.Store.DropFile(name)
}

func cmdLog(n *proto.Node, args []string) error {
	name, err := oneArg(args, "<name>")
	if err != nil {
		return err
	}
	n.Store.Scan()
	m := n.Store.Get(name)
	if m == nil {
		return fmt.Errorf("no note %q", name)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for i := len(m.History) - 1; i >= 0; i-- {
		v := m.History[i]
		what := v.Hash[:8]
		if v.Deleted {
			what = "deleted"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", v.Seq, v.ModTime.Local().Format("2006-01-02 15:04:05"), v.ModBy, what, v.Clock)
	}
	return tw.Flush()
}

func cmdShow(n *proto.Node, args []string) error {
	if len(args) != 2 {
		return errors.New("expected <name> <seq>")
	}
	name := store.Canon(args[0])
	m := n.Store.Get(name)
	if m == nil {
		return fmt.Errorf("no note %q", name)
	}
	seq, err := strconv.Atoi(args[1])
	if err != nil || seq < 1 || seq > len(m.History) {
		return fmt.Errorf("seq must be 1..%d", len(m.History))
	}
	v := m.History[seq-1]
	if v.Deleted {
		return errors.New("that version is a deletion")
	}
	data, err := n.Store.Snapshot(name, v.Hash)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func cmdPeers(ctx context.Context, n *proto.Node) error {
	peers, err := ts.Peers(ctx)
	if err != nil {
		return err
	}
	type row struct {
		p  ts.Peer
		np string
	}
	rows := make([]row, len(peers))
	done := make(chan struct{}, len(peers))
	for i, p := range peers {
		rows[i].p = p
		go func(i int, p ts.Peer) {
			defer func() { done <- struct{}{} }()
			switch {
			case p.Self:
				rows[i].np = "(self)"
			case !p.Online:
				rows[i].np = "-"
			default:
				if pg, err := n.PingPeer(ctx, p); err == nil {
					rows[i].np = fmt.Sprintf("np %d notes", pg.Notes)
				} else {
					rows[i].np = "no np"
				}
			}
		}(i, p)
	}
	for range peers {
		<-done
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		state := "online"
		if !r.p.Online {
			state = "offline"
		}
		mark := " "
		if r.p.Name == n.Store.Config.Hub {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\n", mark, r.p.Name, r.p.IP, r.p.OS, state, r.np)
	}
	return tw.Flush()
}

func cmdHub(ctx context.Context, n *proto.Node, args []string) error {
	if len(args) == 0 {
		if n.Store.Config.Hub == "" {
			fmt.Println("no hub set (np hub <peer>)")
		} else {
			fmt.Println(n.Store.Config.Hub)
		}
		return nil
	}
	if args[0] == "none" {
		n.Store.Config.Hub = ""
		return n.Store.SaveConfig()
	}
	p, err := ts.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	if p.Self {
		return errors.New("hub must be another machine")
	}
	n.Store.Config.Hub = p.Name
	if err := n.Store.SaveConfig(); err != nil {
		return err
	}
	fmt.Printf("hub set to %s (%s)\n", p.Name, p.IP)
	return nil
}

func targetPeer(ctx context.Context, n *proto.Node, args []string) (ts.Peer, error) {
	target := n.Store.Config.Hub
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" {
		return ts.Peer{}, errors.New("no peer given and no hub set (np hub <peer>)")
	}
	p, err := ts.Resolve(ctx, target)
	if err != nil {
		return p, err
	}
	if !p.Online {
		return p, fmt.Errorf("%s is offline", p.Name)
	}
	return p, nil
}

func cmdSync(ctx context.Context, n *proto.Node, args []string) error {
	if len(args) == 1 && (args[0] == "--all" || args[0] == "-a") {
		reps, err := n.SyncAll(ctx)
		if err != nil {
			return err
		}
		if len(reps) == 0 {
			fmt.Println("no online peers running np")
		}
		for _, rep := range reps {
			printReport(rep)
		}
		return nil
	}
	p, err := targetPeer(ctx, n, args)
	if err != nil {
		return err
	}
	rep, err := n.Sync(ctx, p)
	if err != nil {
		return err
	}
	printReport(rep)
	return nil
}

func printReport(rep proto.SyncReport) {
	fmt.Println(rep)
	for _, s := range rep.Pulled {
		fmt.Println("  <-", s)
	}
	for _, s := range rep.Pushed {
		fmt.Println("  ->", s)
	}
	for _, s := range rep.NotFetched {
		fmt.Println("  .. not fetched:", s, "(np get to download)")
	}
	for _, s := range rep.Merged {
		fmt.Println("  == merged:", s, "(both edits combined)")
	}
	for _, s := range rep.Conflicts {
		fmt.Println("  !! conflict:", s, "(loser kept as *.conflict-* note)")
	}
	for _, s := range rep.Errors {
		fmt.Println("  error:", s)
	}
}

// cmdDaemon serves, syncs with the hub on every tick, and, whenever a peer
// pushes a change to us, fans it out to every other online np peer after a
// short debounce. On the hub that turns one machine's push into everyone's
// pull; on a leaf it just keeps the mesh converged faster.
func cmdDaemon(ctx context.Context, n *proto.Node) error {
	n.Changed = make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() { errc <- n.Serve(ctx) }()
	interval := time.Duration(n.Store.Config.Interval) * time.Second
	tick := time.NewTicker(interval)
	defer tick.Stop()
	const debounce = 2 * time.Second
	var fanout <-chan time.Time

	scan := func() {
		n.Lock()
		changed, err := n.Store.Scan()
		n.Unlock()
		if err != nil {
			n.Log.Println("scan:", err)
		}
		for _, c := range changed {
			n.Log.Println("local change:", c)
		}
	}
	syncHub := func() {
		if n.Store.Config.Hub == "" {
			return
		}
		p, err := targetPeer(ctx, n, nil)
		if err != nil {
			n.Log.Println("sync:", err)
			return
		}
		rep, err := n.Sync(ctx, p)
		if err != nil {
			n.Log.Println("sync:", err)
			return
		}
		if len(rep.Pulled)+len(rep.Pushed)+len(rep.Errors) > 0 {
			n.Log.Println("sync", rep.Detail())
		}
	}
	// syncAll syncs with every online np peer. Fan-outs log everything;
	// the periodic hub-less mesh sync logs only when something moved.
	syncAll := func(label string, verbose bool) {
		reps, err := n.SyncAll(ctx)
		if err != nil {
			n.Log.Println(label+":", err)
			return
		}
		if len(reps) == 0 && verbose {
			n.Log.Println(label + ": no other online np peers")
		}
		for _, rep := range reps {
			if verbose || len(rep.Pulled)+len(rep.Pushed)+len(rep.Errors) > 0 {
				n.Log.Println(label, rep.Detail())
			}
		}
	}
	periodic := func() {
		if n.Store.Config.Hub != "" {
			syncHub()
		} else {
			syncAll("mesh", false)
		}
	}

	scan()
	periodic()
	for {
		select {
		case <-ctx.Done():
			return <-errc
		case err := <-errc:
			return err
		case <-tick.C:
			scan()
			periodic()
		case <-n.Changed:
			if fanout == nil {
				fanout = time.After(debounce)
			}
		case <-fanout:
			fanout = nil
			syncAll("fan-out", true)
		}
	}
}

func cmdService(n *proto.Node, args []string) error {
	sub, err := oneArg(args, "install | uninstall | status")
	if err != nil {
		return err
	}
	switch sub {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		p, err := service.Install(exe, filepath.Join(n.Store.Dir, "daemon.log"))
		if err != nil {
			return err
		}
		fmt.Printf("installed %s\nrunning: %s daemon\n", p, exe)
		if _, err := os.Stat("/run/systemd/system"); err == nil {
			fmt.Println("tip: `loginctl enable-linger` keeps it running when you are logged out")
		}
		return nil
	case "uninstall":
		return service.Uninstall()
	case "status":
		st, err := service.Status()
		if err != nil {
			return err
		}
		fmt.Println(st)
		return nil
	default:
		return fmt.Errorf("unknown service command %q", sub)
	}
}

func cmdStatus(ctx context.Context, n *proto.Node) error {
	n.Store.Scan()
	hub := n.Store.Config.Hub
	if hub == "" {
		hub = "(none)"
	}
	files := n.Store.Files(false)
	stubs := 0
	for _, m := range files {
		if !m.Have {
			stubs++
		}
	}
	fmt.Printf("node:   %s (%s)\nlogin:  %s\ndir:    %s\nhub:    %s\nweb:    http://%s:%d/\nnotes:  %d\nfiles:  %d (%d not fetched; auto-fetch up to %s%s)\n",
		n.Self.Name, n.Self.IP, n.Self.Login, n.Store.Dir, hub, n.Self.IP, n.Store.Config.Port, len(n.Store.List(false)),
		len(files), stubs, autoFetchText(n.Store.Config), keepAllText(n.Store.Config))
	states, _, reason := hubStates(ctx, n)
	if states == nil {
		fmt.Printf("sync:   %s\n", reason)
		return nil
	}
	counts := map[proto.SyncState]int{}
	for _, st := range states {
		counts[st]++
	}
	fmt.Printf("sync:   %d synced, %d ahead, %d new, %d behind, %d conflict\n",
		counts[proto.Synced], counts[proto.Ahead], counts[proto.New], counts[proto.Behind], counts[proto.Diverged])
	return nil
}

func autoFetchText(c store.Config) string {
	if c.AutoFetchLimit() < 0 {
		return "never"
	}
	return store.FileSize(c.AutoFetchLimit())
}

func keepAllText(c store.Config) string {
	if c.KeepAll {
		return ", keep_all"
	}
	return ""
}

func cmdUpgrade(args []string) error {
	check := len(args) == 1 && args[0] == "--check"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rel, err := upgrade.Latest(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("current %s, latest %s\n", Version, rel.Tag)
	if strings.TrimPrefix(rel.Tag, "v") == strings.TrimPrefix(Version, "v") {
		fmt.Println("already up to date")
		return nil
	}
	if check {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if err := upgrade.Install(ctx, rel, exe); err != nil {
		return err
	}
	fmt.Printf("installed %s to %s\n", rel.Tag, exe)
	restarted, err := service.Restart()
	if err != nil {
		return fmt.Errorf("binary upgraded but service restart failed: %w", err)
	}
	if restarted {
		fmt.Println("service restarted")
	}
	return nil
}
