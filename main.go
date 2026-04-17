// Command remote-tools is a single static binary for short-lived DevOps and
// testing work on remote VPSes.
//
// It embeds a userspace Tailscale node (via tsnet), so it needs no root, no
// TUN device, and no persistent system state outside a single /tmp directory
// that is wiped on exit. On shutdown it actively logs out of the tailnet so
// the node disappears from the admin list immediately; Ephemeral: true is
// the fallback for ungraceful exits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/avbel/remote-tools/internal/fileserver"
	"github.com/avbel/remote-tools/internal/portfwd"
	"github.com/avbel/remote-tools/internal/tsnode"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// envDefault returns the first non-empty value among the provided env vars,
// falling back to def.
func envDefault(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func main() {
	fs := flag.NewFlagSet("remote-tools", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `remote-tools %s

A portable, zero-trace helper for short-lived DevOps work on a remote VPS.
It joins your tailnet via an embedded userspace Tailscale node (no root, no
TUN device, state kept in /tmp only) and optionally exposes a read-only
file browser and/or TCP port forwards over that tailnet.

On stop (SIGINT/SIGTERM) it logs the node out of the tailnet immediately, so
it disappears from your admin list. Ephemeral node keys are the fallback if
the process dies ungracefully.

Usage:
  remote-tools [flags]

Flags:
`, Version)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Environment variable fallbacks:
  TS_AUTHKEY / REMOTE_TOOLS_TS_AUTHKEY    --ts-authkey
  REMOTE_TOOLS_TS_HOSTNAME                --ts-hostname
  REMOTE_TOOLS_SERVE_DIR                  --serve-dir
  REMOTE_TOOLS_SERVE_PORT                 --serve-port

Examples:
  remote-tools --ts-authkey=tskey-... --serve-dir=/var/log
  remote-tools --ts-authkey=tskey-... --expose=5432=localhost:5432
`)
	}

	authKey := fs.String("ts-authkey", envDefault("", "TS_AUTHKEY", "REMOTE_TOOLS_TS_AUTHKEY"),
		"Tailscale ephemeral auth key (required)")
	hostname := fs.String("ts-hostname", envDefault(defaultHostname(), "REMOTE_TOOLS_TS_HOSTNAME"),
		"Hostname on the tailnet")
	serveDir := fs.String("serve-dir", envDefault("", "REMOTE_TOOLS_SERVE_DIR"),
		"Directory to expose read-only over HTTP; empty disables the file server")
	servePortDefault := 8080
	if v := envDefault("", "REMOTE_TOOLS_SERVE_PORT"); v != "" {
		if p, err := atoiStrict(v); err == nil {
			servePortDefault = p
		}
	}
	servePort := fs.Int("serve-port", servePortDefault,
		"Port for the file server on the tailnet listener")
	var exposes stringSlice
	fs.Var(&exposes, "expose",
		"Port forward spec PORT=HOST:PORT (repeatable)")
	verbose := fs.Bool("verbose", false, "Enable verbose tsnet logging")
	showVersion := fs.Bool("version", false, "Print version and exit")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(Version)
		return
	}

	if *authKey == "" {
		fmt.Fprintln(os.Stderr, "error: --ts-authkey is required (or set TS_AUTHKEY)")
		os.Exit(2)
	}

	specs := make([]portfwd.Spec, 0, len(exposes))
	for _, raw := range exposes {
		s, err := portfwd.ParseSpec(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(2)
		}
		specs = append(specs, s)
	}

	if err := run(*authKey, *hostname, *serveDir, *servePort, specs, *verbose); err != nil {
		log.Fatalf("remote-tools: %v", err)
	}
}

func run(authKey, hostname, serveDir string, servePort int, specs []portfwd.Spec, verbose bool) error {
	// /tmp-only state dir, wiped on exit no matter how we leave.
	stateDir, err := os.MkdirTemp("", "remote-tools-*")
	if err != nil {
		return fmt.Errorf("create tmp state dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(stateDir); err != nil {
			log.Printf("warn: could not remove %s: %v", stateDir, err)
		}
	}()
	log.Printf("state dir: %s (will be removed on exit)", stateDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Catch SIGINT/SIGTERM so we trigger orderly shutdown (and Logout).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("received %s, shutting down...", s)
		cancel()
	}()

	cfg := tsnode.Config{
		AuthKey:  authKey,
		Hostname: hostname,
		Dir:      stateDir,
	}
	if verbose {
		cfg.Logf = log.Printf
	}

	bringUpCtx, bringUpCancel := context.WithTimeout(ctx, 60*time.Second)
	node, err := tsnode.Start(bringUpCtx, cfg)
	bringUpCancel()
	if err != nil {
		return fmt.Errorf("tailscale: %w", err)
	}
	if ips, err := node.TailnetIPs(); err == nil {
		log.Printf("tailnet up: hostname=%s ips=%s", node.Hostname(), strings.Join(ips, ","))
	}

	var wg sync.WaitGroup
	runErr := make(chan error, 2)

	if serveDir != "" {
		handler, err := fileserver.New(serveDir)
		if err != nil {
			_ = node.Shutdown(5 * time.Second)
			return err
		}
		ln, err := node.Listen("tcp", fmt.Sprintf(":%d", servePort))
		if err != nil {
			_ = node.Shutdown(5 * time.Second)
			return fmt.Errorf("listen on tailnet :%d: %w", servePort, err)
		}
		// http.Server dispatches each accepted connection in its own
		// goroutine, so parallel downloads from multiple clients (or a
		// single client opening multiple connections) run concurrently
		// without any extra work on our side. We intentionally do NOT
		// set WriteTimeout so long-running downloads aren't cut off.
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Printf("file server: http://%s:%d/ (dir=%s)", hostname, servePort, serveDir)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				runErr <- fmt.Errorf("file server: %w", err)
			}
		}()
		// Ensure the HTTP server is stopped when ctx is cancelled.
		go func() {
			<-ctx.Done()
			shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	if len(specs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := portfwd.Run(ctx, node, specs); err != nil {
				runErr <- fmt.Errorf("portfwd: %w", err)
			}
		}()
	}

	// Wait for either a fatal runtime error or shutdown signal.
	go func() {
		wg.Wait()
		close(runErr)
	}()

	var exitErr error
	for err := range runErr {
		if exitErr == nil {
			exitErr = err
		}
		cancel()
	}

	// Orderly Logout so the node is removed from the admin list immediately.
	log.Printf("logging out of tailnet...")
	if err := node.Shutdown(10 * time.Second); err != nil {
		log.Printf("tailscale shutdown: %v", err)
	}
	return exitErr
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "remote-tools"
	}
	// Keep only a safe subset for DNS-ish use.
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "remote-tools"
	}
	return "remote-tools-" + b.String()
}

func atoiStrict(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid integer %q", s)
		}
		n = n*10 + int(r-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty integer")
	}
	return n, nil
}
