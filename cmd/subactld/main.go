// Command subactld is the subagent resource controller daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/backend/cgroup2"
	"github.com/zydo/subactl/internal/daemon"
	"github.com/zydo/subactl/internal/hostmem"
)

func main() {
	// Re-executed inside a lease's cgroup, subactld is the pre-exec shim.
	if len(os.Args) > 1 && os.Args[1] == cgroup2.ShimArg {
		fmt.Fprintln(os.Stderr, "subactld:", cgroup2.ShimMain(os.Args[2:]))
		os.Exit(126)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "subactld:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() error {
	socket := flag.String("socket", client.SocketFromEnv(), "Unix socket to listen on")
	backendName := flag.String("backend", "cgroup2", "enforcement backend: cgroup2 or fake")
	capacityFlag := flag.String("capacity", "", "cap on the effective capacity, e.g. 24G (default: total memory minus the reserve)")
	reserveFlag := flag.String("reserve", "", "memory kept for the system, e.g. 8G (default: the larger of 15% and 8G)")
	cgroupBase := flag.String("cgroup", "/sys/fs/cgroup/subactl", "cgroup under which every lease lives (cgroup2 backend)")
	socketGroup := flag.String("socket-group", "subactl", "group that may connect to the socket")
	stateDir := flag.String("state", envOr("SUBACTL_STATE", "/var/lib/subactl"), "state directory: scratch directories and, later, the audit log")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var be backend.Backend
	switch *backendName {
	case "fake":
		be = backend.NewFake()
		log.Warn("the fake backend starts no processes and enforces nothing; it is for development only")
	case "cgroup2":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		be, err = cgroup2.New(cgroup2.Config{Base: *cgroupBase, StateDir: *stateDir, Socket: *socket, Shim: self})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown backend %q: want cgroup2 or fake", *backendName)
	}

	total, err := hostmem.Total()
	if err != nil {
		return fmt.Errorf("read total memory: %w", err)
	}
	reserve := hostmem.DefaultReserve(total)
	if *reserveFlag != "" {
		if reserve, err = api.ParseBytes(*reserveFlag); err != nil {
			return fmt.Errorf("--reserve: %w", err)
		}
	}
	// TODO(M3): subtract external usage and recompute every second.
	capacity := max(0, total-reserve)
	if *capacityFlag != "" {
		limit, err := api.ParseBytes(*capacityFlag)
		if err != nil {
			return fmt.Errorf("--capacity: %w", err)
		}
		capacity = min(capacity, limit)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	m, err := daemon.NewManager(ctx, daemon.Config{Backend: be, Capacity: capacity, MemTotal: total, Reserve: reserve, Log: log})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(*socket), 0o755); err != nil {
		return err
	}
	// A socket file left by a dead daemon would make Listen fail.
	if c, err := net.DialTimeout("unix", *socket, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("another subactld is already listening on %s", *socket)
	}
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer os.Remove(*socket)
	// The socket is for root and one group. Agents and operators must be
	// in that group to reach the daemon at all.
	if g, err := user.LookupGroup(*socketGroup); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(*socket, -1, gid); err != nil {
			return err
		}
	} else if os.Geteuid() == 0 {
		log.Warn("the socket group does not exist; only root can reach the daemon", "group", *socketGroup)
	}
	if err := os.Chmod(*socket, 0o660); err != nil {
		return err
	}

	srv := &http.Server{Handler: daemon.NewServer(m, nil), ConnContext: daemon.ConnContext}
	log.Info("subactld listening", "socket", *socket, "backend", be.Name(),
		"effective_capacity", api.FormatBytes(capacity), "reserve", api.FormatBytes(reserve))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down: reaping every lease")
	m.Shutdown()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return srv.Close()
	}
	return nil
}
