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
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/client"
	"github.com/zydo/subactl/internal/audit"
	"github.com/zydo/subactl/internal/backend"
	"github.com/zydo/subactl/internal/backend/cgroup2"
	"github.com/zydo/subactl/internal/backend/darwinmon"
	"github.com/zydo/subactl/internal/capacity"
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

// defaultCgroupBase puts the leases below the daemon's own cgroup when the
// daemon runs as a systemd service with Delegate=yes, where that subtree
// is its to manage; otherwise directly below the root.
func defaultCgroupBase() string {
	if goruntime.GOOS != "linux" || os.Getenv("INVOCATION_ID") == "" {
		return "/sys/fs/cgroup/subactl"
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/sys/fs/cgroup/subactl"
	}
	own, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "0::")
	if !ok || own == "/" {
		return "/sys/fs/cgroup/subactl"
	}
	return filepath.Join("/sys/fs/cgroup", own, "leases")
}

// defaultBackend is the real backend of the platform.
func defaultBackend() string {
	if goruntime.GOOS == "darwin" {
		return "darwin"
	}
	return "cgroup2"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() error {
	socket := flag.String("socket", client.SocketFromEnv(), "Unix socket to listen on")
	backendName := flag.String("backend", defaultBackend(), "enforcement backend: cgroup2 (Linux), darwin (macOS, monitoring only) or fake")
	capacityFlag := flag.String("capacity", "", "cap on the effective capacity, e.g. 24G (default: total memory minus the reserve)")
	reserveFlag := flag.String("reserve", "", "memory kept for the system, e.g. 8G (default: the larger of 15% and 8G)")
	cgroupBase := flag.String("cgroup", defaultCgroupBase(), "cgroup under which every lease lives (cgroup2 backend)")
	socketGroup := flag.String("socket-group", "subactl", "group that may connect to the socket")
	capacityCgroup := flag.String("capacity-cgroup", "", "treat this cgroup's memory.max and usage as the machine, for a daemon inside a capped cgroup")
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
	case "darwin":
		var err error
		be, err = darwinmon.New(darwinmon.Config{StateDir: *stateDir, Socket: *socket})
		if err != nil {
			return err
		}
		log.Warn("the macOS backend only monitors: limits are applied after the fact and a process can escape them")
	default:
		return fmt.Errorf("unknown backend %q: want cgroup2, darwin or fake", *backendName)
	}

	// What "the machine" is: an outer cgroup when told so, the whole host
	// on Linux, and a fixed total elsewhere.
	var err error
	var host capacity.Reader
	switch {
	case *capacityCgroup != "":
		host = capacity.CgroupReader{Dir: *capacityCgroup}
	case goruntime.GOOS == "linux":
		host = capacity.MeminfoReader{Path: "/proc/meminfo"}
	case goruntime.GOOS == "darwin":
		host = darwinHost()
	}
	var total int64
	if host != nil {
		h, err := host.Read()
		if err != nil {
			return fmt.Errorf("read host memory: %w", err)
		}
		total = h.Total
	} else if total, err = hostmem.Total(); err != nil {
		return fmt.Errorf("read total memory: %w", err)
	}
	reserve := hostmem.DefaultReserve(total)
	if be.Capabilities().Memory == api.LevelMonitorOnly {
		reserve = hostmem.MonitorReserve(total)
	}
	if *reserveFlag != "" {
		if reserve, err = api.ParseBytes(*reserveFlag); err != nil {
			return fmt.Errorf("--reserve: %w", err)
		}
	}
	// With a host reader the capacity is recomputed every second and the
	// flag is only a cap on it; without one it is fixed.
	var capacityLimit int64
	if *capacityFlag != "" {
		if capacityLimit, err = api.ParseBytes(*capacityFlag); err != nil {
			return fmt.Errorf("--capacity: %w", err)
		}
	}
	if host == nil {
		fixed := max(0, total-reserve)
		if capacityLimit > 0 {
			fixed = min(fixed, capacityLimit)
		}
		capacityLimit = fixed
	}
	auditDir := filepath.Join(*stateDir, "audit")
	auditLog := audit.Open(auditDir)
	defer auditLog.Close()
	suggest := audit.NewSuggester(2<<30, be.MemBasis())
	if err := suggest.Load(auditDir); err != nil {
		log.Warn("cannot load the quota history", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	m, err := daemon.NewManager(ctx, daemon.Config{
		Backend: be, Capacity: capacityLimit, MemTotal: total, Reserve: reserve, Host: host,
		Audit: auditLog, AuditDir: auditDir, Suggest: suggest, Log: log,
	})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(*socket), 0o755); err != nil {
		return err
	}
	if err := api.CheckSocketPath(*socket); err != nil {
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

	server := daemon.NewServer(m, nil)
	srv := &http.Server{Handler: server, ConnContext: server.ConnContext}
	if os.Geteuid() == 0 {
		if err := daemon.ProtectSelf(); err != nil {
			log.Warn("could not lower the daemon's own OOM score", "err", err)
		}
	}
	log.Info("subactld listening", "socket", *socket, "backend", be.Name(),
		"effective_capacity", api.FormatBytes(m.Host().EffectiveCapacity), "reserve", api.FormatBytes(reserve))
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
