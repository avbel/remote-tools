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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/util/dnsname"

	"github.com/avbel/remote-tools/internal/fileserver"
	"github.com/avbel/remote-tools/internal/portfwd"
	"github.com/avbel/remote-tools/internal/tsnode"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func envDefault(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func envDefaultInt(def int, keys ...string) int {
	if v := envDefault("", keys...); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type config struct {
	AuthKey   string
	Hostname  string
	ServeDir  string
	ServePort int
	Specs     []portfwd.Spec
	Verbose   bool
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
	servePort := fs.Int("serve-port", envDefaultInt(8080, "REMOTE_TOOLS_SERVE_PORT"),
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

	cfg := config{
		AuthKey:   *authKey,
		Hostname:  *hostname,
		ServeDir:  *serveDir,
		ServePort: *servePort,
		Specs:     specs,
		Verbose:   *verbose,
	}
	if err := run(cfg); err != nil {
		log.Fatalf("remote-tools: %v", err)
	}
}

func run(cfg config) error {
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tsCfg := tsnode.Config{
		AuthKey:  cfg.AuthKey,
		Hostname: cfg.Hostname,
		Dir:      stateDir,
	}
	if cfg.Verbose {
		tsCfg.Logf = log.Printf
	}

	bringUpCtx, bringUpCancel := context.WithTimeout(ctx, 60*time.Second)
	node, err := tsnode.Start(bringUpCtx, tsCfg)
	bringUpCancel()
	if err != nil {
		return fmt.Errorf("tailscale: %w", err)
	}
	if ips := node.TailnetIPs(); len(ips) > 0 {
		log.Printf("tailnet up: hostname=%s ips=%s", cfg.Hostname, strings.Join(ips, ","))
	}

	var wg sync.WaitGroup
	runErr := make(chan error, 2)

	if cfg.ServeDir != "" {
		handler, err := fileserver.New(cfg.ServeDir)
		if err != nil {
			_ = node.Shutdown(5 * time.Second)
			return err
		}
		ln, err := node.Listen("tcp", fmt.Sprintf(":%d", cfg.ServePort))
		if err != nil {
			_ = node.Shutdown(5 * time.Second)
			return fmt.Errorf("listen on tailnet :%d: %w", cfg.ServePort, err)
		}
		// No WriteTimeout so long downloads aren't cut off; http.Server
		// already dispatches each connection in its own goroutine.
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Printf("file server: http://%s:%d/ (dir=%s)", cfg.Hostname, cfg.ServePort, cfg.ServeDir)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				runErr <- fmt.Errorf("file server: %w", err)
			}
		}()
		// Graceful shutdown waits for in-flight downloads so Ctrl-C in the
		// middle of `curl -O` doesn't truncate the file. After the grace
		// period, Close() forces any stragglers so we don't block the
		// tailnet Logout that follows.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ctx.Done()
			shutdownCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
			defer c()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				_ = srv.Close()
			}
		}()
	}

	if len(cfg.Specs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := portfwd.Run(ctx, node, cfg.Specs); err != nil {
				runErr <- fmt.Errorf("portfwd: %w", err)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(runErr)
	}()

	var exitErr error
	for err := range runErr {
		if exitErr == nil {
			exitErr = err
		}
		stop() // cancel the signal-linked ctx so the other goroutines wind down
	}

	log.Printf("logging out of tailnet...")
	if err := node.Shutdown(10 * time.Second); err != nil {
		log.Printf("tailscale shutdown: %v", err)
	}
	return exitErr
}

// defaultHostname builds the default tailnet hostname from the machine's
// hostname, sanitised into a DNS label.
func defaultHostname() string {
	h, _ := os.Hostname()
	h = dnsname.SanitizeHostname(h)
	if h == "" {
		return "remote-tools"
	}
	return "remote-tools-" + h
}
