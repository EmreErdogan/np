// Package ts wraps the Tailscale local API: who we are, who our peers are,
// and who is on the other end of an incoming connection.
package ts

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
)

// Peer is a machine in the tailnet.
type Peer struct {
	Name   string // first DNS label, e.g. "laptop"
	Host   string
	IP     netip.Addr
	OS     string
	Login  string
	Online bool
	Self   bool
}

// Identity is the local machine.
type Identity struct {
	Name  string
	IP    netip.Addr
	Login string
}

var lc local.Client

func nameOf(dnsName, host string) string {
	if i := strings.IndexByte(dnsName, '.'); i > 0 {
		return strings.ToLower(dnsName[:i])
	}
	if dnsName != "" {
		return strings.ToLower(dnsName)
	}
	return strings.ToLower(host)
}

func v4(addrs []netip.Addr) netip.Addr {
	for _, a := range addrs {
		if a.Is4() {
			return a
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return netip.Addr{}
}

func status(ctx context.Context) (*ipnstate.Status, error) {
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscale: %w (is tailscaled running?)", err)
	}
	if st.BackendState != "Running" {
		return nil, fmt.Errorf("tailscale is %s, not Running", st.BackendState)
	}
	if st.Self == nil {
		return nil, errors.New("tailscale: no self node")
	}
	return st, nil
}

// Self returns the local identity.
func Self(ctx context.Context) (Identity, error) {
	st, err := status(ctx)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Name:  nameOf(st.Self.DNSName, st.Self.HostName),
		IP:    v4(st.Self.TailscaleIPs),
		Login: st.User[st.Self.UserID].LoginName,
	}, nil
}

// Peers lists every node in the tailnet, self included, sorted by name.
func Peers(ctx context.Context) ([]Peer, error) {
	st, err := status(ctx)
	if err != nil {
		return nil, err
	}
	conv := func(p *ipnstate.PeerStatus, self bool) Peer {
		return Peer{
			Name:   nameOf(p.DNSName, p.HostName),
			Host:   p.HostName,
			IP:     v4(p.TailscaleIPs),
			OS:     p.OS,
			Login:  st.User[p.UserID].LoginName,
			Online: self || p.Online,
			Self:   self,
		}
	}
	out := []Peer{conv(st.Self, true)}
	for _, p := range st.Peer {
		out = append(out, conv(p, false))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Resolve turns a peer name, hostname or IP into a Peer.
func Resolve(ctx context.Context, target string) (Peer, error) {
	peers, err := Peers(ctx)
	if err != nil {
		return Peer{}, err
	}
	t := strings.ToLower(target)
	if ip, err := netip.ParseAddr(t); err == nil {
		for _, p := range peers {
			if p.IP == ip {
				return p, nil
			}
		}
		return Peer{Name: t, IP: ip, Online: true}, nil
	}
	for _, p := range peers {
		if p.Name == t || strings.ToLower(p.Host) == t {
			return p, nil
		}
	}
	return Peer{}, fmt.Errorf("no tailnet peer named %q (see `np peers`)", target)
}

// WhoIs identifies the remote side of a connection by its tailnet address.
func WhoIs(ctx context.Context, remoteAddr string) (Peer, error) {
	w, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Peer{}, err
	}
	p := Peer{Online: true}
	if w.Node != nil {
		p.Name = nameOf(w.Node.Name, w.Node.Hostinfo.Hostname())
		p.Host = w.Node.Hostinfo.Hostname()
		if len(w.Node.Addresses) > 0 {
			p.IP = w.Node.Addresses[0].Addr()
		}
	}
	if w.UserProfile != nil {
		p.Login = w.UserProfile.LoginName
	}
	return p, nil
}
