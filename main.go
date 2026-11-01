// Command gpodder-sync is a small gpodder.net-compatible sync server for
// AntennaPod. It does no authentication itself: a reverse proxy (Caddy,
// Authelia, ...) authenticates clients and forwards the username in a header.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gpodder-sync/internal/api"
	"gpodder-sync/internal/store"
)

var version = "dev"

const usage = `gpodder-sync: a gpodder.net-compatible sync server for AntennaPod

Usage:
  gpodder-sync serve -trusted-proxies IPS [-listen ADDR] [-db PATH] [-proxy-user-header NAME]
  gpodder-sync user list [-db PATH]
  gpodder-sync user delete [-db PATH] NAME
  gpodder-sync healthcheck
  gpodder-sync version

Environment (used as flag defaults):
  GPODDER_SYNC_TRUSTED_PROXIES    comma-separated IPs/CIDRs of the reverse proxy (required)
  GPODDER_SYNC_PROXY_USER_HEADER  header with the authenticated username (default "Remote-User")
  GPODDER_SYNC_LISTEN             listen address (default ":8080")
  GPODDER_SYNC_DB                 SQLite database path (default "gpodder-sync.db")
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "user":
		err = cmdUser(os.Args[2:])
	case "healthcheck":
		err = cmdHealthcheck()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", envOr("GPODDER_SYNC_DB", "gpodder-sync.db"), "SQLite database path")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", envOr("GPODDER_SYNC_LISTEN", ":8080"), "listen address")
	dbPath := dbFlag(fs)
	proxies := fs.String("trusted-proxies", os.Getenv("GPODDER_SYNC_TRUSTED_PROXIES"),
		"comma-separated IPs/CIDRs of the reverse proxy; all other peers are refused")
	userHeader := fs.String("proxy-user-header", envOr("GPODDER_SYNC_PROXY_USER_HEADER", "Remote-User"),
		"header carrying the username authenticated by the proxy")
	fs.Parse(args)

	trusted, err := parsePrefixes(*proxies)
	if err != nil {
		return err
	}
	if len(trusted) == 0 {
		return errors.New("set GPODDER_SYNC_TRUSTED_PROXIES (or -trusted-proxies) to your reverse proxy's IP")
	}
	if *userHeader == "" {
		return errors.New("-proxy-user-header must not be empty")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &http.Server{
		Addr: *listen,
		Handler: api.New(api.Config{
			Store:           st,
			TrustedProxies:  trusted,
			ProxyUserHeader: *userHeader,
			Logger:          logger,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", *listen, "db", *dbPath, "version", version,
		"trusted_proxies", *proxies, "user_header", *userHeader)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// cmdUser manages stored data. Users are created automatically on their
// first request; credentials live in the reverse proxy, not here.
func cmdUser(args []string) error {
	if len(args) == 0 {
		return errors.New("missing user subcommand (list, delete)")
	}
	sub := args[0]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	dbPath := dbFlag(fs)
	fs.Parse(args[1:])

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Println(u)
		}
	case "delete":
		if fs.NArg() != 1 {
			return errors.New("usage: gpodder-sync user delete [-db PATH] NAME")
		}
		name := fs.Arg(0)
		if err := st.DeleteUser(ctx, name); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("user %q does not exist", name)
		} else if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "user %q and all their data deleted\n", name)
	default:
		return fmt.Errorf("unknown user subcommand %q", sub)
	}
	return nil
}

// cmdHealthcheck lets the distroless image (no shell, no curl) run a Docker
// HEALTHCHECK against its own /healthz endpoint.
func cmdHealthcheck() error {
	_, port, err := net.SplitHostPort(envOr("GPODDER_SYNC_LISTEN", ":8080"))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}
