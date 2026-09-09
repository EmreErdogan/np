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
  np ls [--local]        list notes with sync state against the hub
  np cat <name>          print a note
  np view <name>         render a note in the terminal (markdown or code)
  np search <text>       find notes whose name or content contains text
  np rm <name>           delete a note (tombstone syncs to peers)
  np log <name>          version history
  np show <name> <seq>   print a historical version

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

Notes live in ~/.np/notes as plain files (override with NP_DIR). A name
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
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	c := exec.Command(parts[0], append(parts[1:], p)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", editor, err)
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
func hubStates(ctx context.Context, n *proto.Node) (map[string]proto.SyncState, string) {
	if n.Store.Config.Hub == "" {
		return nil, "no hub set"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p, err := targetPeer(ctx, n, nil)
	if err != nil {
		return nil, err.Error()
	}
	remote, err := n.RemoteIndex(ctx, p)
	if err != nil {
		return nil, fmt.Sprintf("hub %s unreachable", p.Name)
	}
	return proto.Compare(n.Store.List(true), remote), ""
}

func cmdList(ctx context.Context, n *proto.Node, args []string) error {
	if _, err := n.Store.Scan(); err != nil {
		return err
	}
	var states map[string]proto.SyncState
	reason := "skipped"
	if !(len(args) == 1 && args[0] == "--local") {
		states, reason = hubStates(ctx, n)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	listed := map[string]bool{}
	for _, m := range n.Store.List(false) {
		listed[m.Name] = true
		st := ""
		if states != nil {
			st = string(states[m.Name])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Name, m.ModTime.Local().Format("2006-01-02 15:04"), m.ModBy, st)
	}
	for name, st := range states { // notes only the hub has
		if !listed[name] && st == proto.Behind {
			fmt.Fprintf(tw, "%s\t\t\t%s (hub only)\n", name, st)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if states == nil && reason != "skipped" {
		fmt.Fprintln(os.Stderr, "sync state unavailable:", reason)
	}
	return nil
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
	name, err := oneArg(args, "<name>")
	if err != nil {
		return err
	}
	return n.Store.Delete(name)
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
	fmt.Printf("node:   %s (%s)\nlogin:  %s\ndir:    %s\nhub:    %s\nweb:    http://%s:%d/\nnotes:  %d\n",
		n.Self.Name, n.Self.IP, n.Self.Login, n.Store.Dir, hub, n.Self.IP, n.Store.Config.Port, len(n.Store.List(false)))
	states, reason := hubStates(ctx, n)
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
