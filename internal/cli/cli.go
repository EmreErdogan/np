// Package cli implements the np command surface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/emre/np/internal/proto"
	"github.com/emre/np/internal/store"
	"github.com/emre/np/internal/ts"
)

const usage = `np - share notes across your tailnet

Notes:
  np new <name>          create a note in $EDITOR (or from stdin)
  np edit <name>         edit a note
  np ls                  list notes
  np cat <name>          print a note
  np rm <name>           delete a note (tombstone syncs to peers)
  np log <name>          version history
  np show <name> <seq>   print a historical version

Sync:
  np peers               tailnet machines and whether they run np
  np hub [<peer>|none]   show or set the default sync peer
  np sync [<peer>]       sync with hub (or the given peer)
  np serve               accept syncs from peers (foreground)
  np daemon              serve + auto-sync with hub every interval
  np status              local identity, hub, note count

Notes live in ~/.np/notes as plain markdown (override with NP_DIR).
Any editor works; np records external edits on the next command.
`

// Run executes the command line and returns an exit code.
func Run(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
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
		return cmdList(n)
	case "cat":
		return cmdCat(n, args)
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
		return cmdStatus(n)
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
	return &proto.Node{Store: s, Self: self, Log: log.New(os.Stderr, "", log.Ltime)}, nil
}

func oneArg(args []string, what string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("expected %s", what)
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

func cmdList(n *proto.Node) error {
	if _, err := n.Store.Scan(); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, m := range n.Store.List(false) {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Name, m.ModTime.Local().Format("2006-01-02 15:04"), m.ModBy)
	}
	return tw.Flush()
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
	m := n.Store.Get(args[0])
	if m == nil {
		return fmt.Errorf("no note %q", args[0])
	}
	seq, err := strconv.Atoi(args[1])
	if err != nil || seq < 1 || seq > len(m.History) {
		return fmt.Errorf("seq must be 1..%d", len(m.History))
	}
	v := m.History[seq-1]
	if v.Deleted {
		return errors.New("that version is a deletion")
	}
	data, err := n.Store.Snapshot(args[0], v.Hash)
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

func cmdDaemon(ctx context.Context, n *proto.Node) error {
	errc := make(chan error, 1)
	go func() { errc <- n.Serve(ctx) }()
	interval := time.Duration(n.Store.Config.Interval) * time.Second
	tick := time.NewTicker(interval)
	defer tick.Stop()
	syncOnce := func() {
		n.Lock()
		changed, err := n.Store.Scan()
		n.Unlock()
		if err != nil {
			n.Log.Println("scan:", err)
		}
		for _, c := range changed {
			n.Log.Println("local change:", c)
		}
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
			n.Log.Println("sync", rep.String())
		}
	}
	syncOnce()
	for {
		select {
		case <-ctx.Done():
			return <-errc
		case err := <-errc:
			return err
		case <-tick.C:
			syncOnce()
		}
	}
}

func cmdStatus(n *proto.Node) error {
	n.Store.Scan()
	hub := n.Store.Config.Hub
	if hub == "" {
		hub = "(none)"
	}
	fmt.Printf("node:   %s (%s)\nlogin:  %s\ndir:    %s\nhub:    %s\nport:   %d\nnotes:  %d\n",
		n.Self.Name, n.Self.IP, n.Self.Login, n.Store.Dir, hub, n.Store.Config.Port, len(n.Store.List(false)))
	return nil
}
