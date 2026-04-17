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
)

type Spec struct {
	TailnetPort int
	Target      string // host:port
}

// ParseSpec parses a "PORT=HOST:PORT" string.
func ParseSpec(s string) (Spec, error) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 || eq == len(s)-1 {
		return Spec{}, fmt.Errorf("portfwd: expected PORT=HOST:PORT, got %q", s)
	}
	portStr, target := s[:eq], s[eq+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return Spec{}, fmt.Errorf("portfwd: invalid tailnet port %q", portStr)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return Spec{}, fmt.Errorf("portfwd: invalid target %q: %w", target, err)
	}
	return Spec{TailnetPort: port, Target: target}, nil
}

// Listener is the subset of the tsnet node we need.
type Listener interface {
	Listen(network, addr string) (net.Listener, error)
}

// Run starts one forwarder per spec. It returns when ctx is cancelled, closing
// all listeners. The returned error is the first fatal listener error, if any.
func Run(ctx context.Context, l Listener, specs []Spec) error {
	if len(specs) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(specs))
	listeners := make([]net.Listener, 0, len(specs))

	for _, s := range specs {
		ln, err := l.Listen("tcp", fmt.Sprintf(":%d", s.TailnetPort))
		if err != nil {
			// Close anything we opened already.
			for _, prev := range listeners {
				_ = prev.Close()
			}
			return fmt.Errorf("portfwd: listen on tailnet :%d: %w", s.TailnetPort, err)
		}
		listeners = append(listeners, ln)
		log.Printf("portfwd: tailnet :%d -> %s", s.TailnetPort, s.Target)

		wg.Add(1)
		go func(ln net.Listener, target string) {
			defer wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					if errors.Is(err, net.ErrClosed) {
						return
					}
					errCh <- err
					return
				}
				go handle(conn, target)
			}
		}(ln, s.Target)
	}

	<-ctx.Done()
	for _, ln := range listeners {
		_ = ln.Close()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
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

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(dst, src); done <- struct{}{} }()
	go func() { _, _ = io.Copy(src, dst); done <- struct{}{} }()
	<-done
}
