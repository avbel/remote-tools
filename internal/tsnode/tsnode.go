// Package tsnode embeds a userspace Tailscale node via tsnet.
//
// The node writes its state to a caller-supplied directory (meant to live
// under /tmp) and, on Shutdown, actively logs out so it is removed from the
// Tailscale admin list immediately. Ephemeral: true is set as a fallback for
// ungraceful exits.
package tsnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"tailscale.com/tsnet"
)

type Config struct {
	AuthKey  string
	Hostname string
	Dir      string
	Logf     func(string, ...any)
}

type Node struct {
	srv *tsnet.Server
}

// Start brings up the tsnet node and blocks until it is connected.
func Start(ctx context.Context, cfg Config) (*Node, error) {
	if cfg.AuthKey == "" {
		return nil, errors.New("tsnode: AuthKey is required")
	}
	if cfg.Dir == "" {
		return nil, errors.New("tsnode: Dir is required")
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "remote-tools"
	}

	logf := cfg.Logf
	if logf == nil {
		// Silence tsnet's chatty internal logger by default.
		logf = func(string, ...any) {}
	}

	srv := &tsnet.Server{
		Hostname:  cfg.Hostname,
		Dir:       cfg.Dir,
		AuthKey:   cfg.AuthKey,
		Ephemeral: true,
		Logf:      logf,
		UserLogf:  log.Printf,
	}

	if _, err := srv.Up(ctx); err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tsnode: bring up node: %w", err)
	}
	return &Node{srv: srv}, nil
}

// Listen opens a listener on the tailnet at the given address (e.g. ":8080").
func (n *Node) Listen(network, addr string) (net.Listener, error) {
	return n.srv.Listen(network, addr)
}

// Dial opens a TCP connection on the tailnet.
func (n *Node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.srv.Dial(ctx, network, addr)
}

// TailnetIPs returns the node's tailnet IPs, useful for logging.
func (n *Node) TailnetIPs() ([]string, error) {
	st, err := n.srv.Up(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(st.TailscaleIPs))
	for _, ip := range st.TailscaleIPs {
		out = append(out, ip.String())
	}
	return out, nil
}

// Hostname returns the tailnet hostname assigned to this node.
func (n *Node) Hostname() string {
	return n.srv.Hostname
}

// Shutdown deregisters the node from the tailnet (via Logout) and closes the
// server. The logout call is bounded by the provided timeout so a flaky
// network cannot block exit.
func (n *Node) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	lc, err := n.srv.LocalClient()
	if err == nil {
		// Best-effort: if Logout fails the ephemeral flag is our fallback.
		if logoutErr := lc.Logout(ctx); logoutErr != nil && !errors.Is(logoutErr, io.EOF) {
			log.Printf("tsnode: logout failed (ephemeral GC will clean up): %v", logoutErr)
		}
	}
	return n.srv.Close()
}
