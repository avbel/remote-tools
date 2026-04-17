// Package portfwd exposes a local TCP service on the tailnet.
//
// A Spec looks like "5432=localhost:5432": listen on port 5432 of the tsnet
// node, and for each accepted connection dial localhost:5432 on the host and
// copy bytes in both directions.
package portfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Spec struct {
	TailnetPort int
	Target      string // host:port
}

// ParseSpec parses a "PORT=HOST:PORT" string.
func ParseSpec(s string) (Spec, error) {
	portStr, target, ok := strings.Cut(s, "=")
	if !ok || portStr == "" || target == "" {
		return Spec{}, fmt.Errorf("portfwd: expected PORT=HOST:PORT, got %q", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return Spec{}, fmt.Errorf("portfwd: invalid tailnet port %q", portStr)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return Spec{}, fmt.Errorf("portfwd: invalid target %q: %w", target, err)
	}
	return Spec{TailnetPort: port, Target: target}, nil
}

// TailnetListener is the subset of the tsnet node we need.
type TailnetListener interface {
	Listen(network, addr string) (net.Listener, error)
}

// Run starts one forwarder per spec. It returns when ctx is cancelled, after
// all accept loops and in-flight connections have shut down. The returned
// error, if any, is the first fatal setup error (Listen failure).
func Run(ctx context.Context, l TailnetListener, specs []Spec) error {
	if len(specs) == 0 {
		return nil
	}

	listeners := make([]net.Listener, 0, len(specs))
	for _, s := range specs {
		ln, err := l.Listen("tcp", fmt.Sprintf(":%d", s.TailnetPort))
		if err != nil {
			for _, prev := range listeners {
				_ = prev.Close()
			}
			return fmt.Errorf("portfwd: listen on tailnet :%d: %w", s.TailnetPort, err)
		}
		listeners = append(listeners, ln)
		log.Printf("portfwd: tailnet :%d -> %s", s.TailnetPort, s.Target)
	}

	var wg sync.WaitGroup
	for i, ln := range listeners {
		wg.Add(1)
		go func(ln net.Listener, target string) {
			defer wg.Done()
			acceptLoop(ln, target)
		}(ln, specs[i].Target)
	}

	<-ctx.Done()
	for _, ln := range listeners {
		_ = ln.Close()
	}
	wg.Wait()
	return nil
}

func acceptLoop(ln net.Listener, target string) {
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept errors (e.g. EMFILE): back off instead of
			// tearing the forwarder down. Backoff caps at 1s and only
			// resets on a successful accept, so a permanently broken
			// listener will log at most once per second until the
			// process exits.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
			}
			log.Printf("portfwd: accept on %s: %v (retrying in %s)", target, err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		go handle(conn, target)
	}
}

func handle(src net.Conn, target string) {
	defer src.Close()
	dst, err := net.Dial("tcp", target)
	if err != nil {
		log.Printf("portfwd: dial %s: %v", target, err)
		return
	}
	defer dst.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(dst, src); _ = closeWrite(dst) }()
	go func() { defer wg.Done(); _, _ = io.Copy(src, dst); _ = closeWrite(src) }()
	wg.Wait()
}

// closeWrite half-closes a TCP connection so the peer sees EOF on its read
// side, letting it drain and close its own direction cleanly.
func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
