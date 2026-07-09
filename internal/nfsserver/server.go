package nfsserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

// ServerConfig configures the in-container NFS server bring-up.
type ServerConfig struct {
	// Threads is the number of kernel nfsd threads to start.
	Threads int
	// StatePath is the NFS state dir mounted as rpc_pipefs, default /var/lib/nfs/rpc_pipefs.
	StatePath string
	// RpcbindReadyTimeout bounds the wait for rpcbind's port 111 to accept.
	RpcbindReadyTimeout time.Duration
	// V4Only serves NFSv4 exclusively. NFSv2/v3 are disabled, which removes the
	// need for the portmapper (rpcbind) and the network lock manager (statd):
	// v4 uses a single well-known port (2049) and in-protocol locking. rpc.mountd
	// is still started because the kernel relies on it for export/auth upcalls.
	V4Only bool
}

func (c *ServerConfig) applyDefaults() {
	if c.Threads <= 0 {
		c.Threads = 8
	}
	if c.StatePath == "" {
		c.StatePath = "/var/lib/nfs/rpc_pipefs"
	}
	if c.RpcbindReadyTimeout == 0 {
		c.RpcbindReadyTimeout = 10 * time.Second
	}
}

// Server brings up and supervises the NFS server daemons as child processes so
// their logs, health, and crashes are observable through the single container
// main process. A death of any critical daemon causes Run to return an error,
// which propagates as a non-zero container exit (pod restart).
type Server struct {
	cfg ServerConfig
	log logr.Logger
}

// NewServer returns a supervisor with defaults applied.
func NewServer(cfg ServerConfig, log logr.Logger) *Server {
	cfg.applyDefaults()
	return &Server{cfg: cfg, log: log}
}

// Prepare mounts the kernel filesystems required by the NFS server. It is
// idempotent (EBUSY is treated as success).
func (s *Server) Prepare() error {
	if err := mountFS("nfsd", "nfsd", "/proc/fs/nfsd"); err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.StatePath, 0o755); err != nil {
		return err
	}
	if err := mountFS("rpc_pipefs", "sunrpc", s.cfg.StatePath); err != nil {
		return err
	}
	return nil
}

// Run brings up the NFS server daemons as supervised children, starts the nfsd
// kernel threads, and blocks until ctx is cancelled or a critical daemon exits.
// In V4Only mode rpcbind is skipped and NFSv2/v3 are disabled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Prepare(); err != nil {
		return fmt.Errorf("prepare nfs kernel filesystems: %w", err)
	}

	exited := make(chan error, 2)

	// rpcbind (portmapper) is only needed for NFSv2/v3 service registration.
	rpcbindPID := 0
	if !s.cfg.V4Only {
		pid, err := s.startChild(ctx, "rpcbind", exited, "rpcbind", "-w", "-f")
		if err != nil {
			return fmt.Errorf("start rpcbind: %w", err)
		}
		if err := s.waitForTCP("127.0.0.1:111", s.cfg.RpcbindReadyTimeout); err != nil {
			return fmt.Errorf("rpcbind did not become ready: %w", err)
		}
		rpcbindPID = pid
	}

	// nfsd kernel threads. rpc.nfsd starts the threads and exits immediately;
	// the threads keep running in the kernel.
	nfsdArgs := append(s.versionArgs(), strconv.Itoa(s.cfg.Threads))
	if out, err := exec.CommandContext(ctx, "rpc.nfsd", nfsdArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("rpc.nfsd %v failed: %v: %s", nfsdArgs, err, string(out))
	}
	s.log.Info("nfsd kernel threads started", "threads", s.cfg.Threads, "v4only", s.cfg.V4Only)

	// Sync the (initially empty) export table before serving.
	if out, err := exec.CommandContext(ctx, "exportfs", "-ra").CombinedOutput(); err != nil {
		s.log.Info("initial exportfs failed (continuing)", "error", err, "output", string(out))
	}

	// rpc.mountd services the kernel's export/auth upcalls (required for v4 too)
	// and, unless disabled, the NFSv3 MOUNT protocol; run foreground.
	mountdArgs := append([]string{"--foreground"}, s.versionArgs()...)
	mountd, err := s.startChild(ctx, "rpc.mountd", exited, append([]string{"rpc.mountd"}, mountdArgs...)...)
	if err != nil {
		return fmt.Errorf("start rpc.mountd: %w", err)
	}

	s.log.Info("NFS server is up", "rpcbind.pid", rpcbindPID, "mountd.pid", mountd)

	select {
	case <-ctx.Done():
		return nil
	case err := <-exited:
		return fmt.Errorf("critical NFS daemon exited: %w", err)
	}
}

// versionArgs returns the -N flags disabling NFSv2/v3 when V4Only is set. The
// same flags are understood by both rpc.nfsd and rpc.mountd.
func (s *Server) versionArgs() []string {
	if !s.cfg.V4Only {
		return nil
	}
	return []string{"-N", "2", "-N", "3"}
}

// startChild launches a foreground daemon, streams its output to the logger,
// and reports its (unexpected) exit on the exited channel.
func (s *Server) startChild(ctx context.Context, name string, exited chan<- error, argv ...string) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go s.pipeLogs(name, stdout)
	go s.pipeLogs(name, stderr)
	go func() {
		err := cmd.Wait()
		select {
		case <-ctx.Done():
			// Expected shutdown.
		default:
			if err == nil {
				err = fmt.Errorf("%s exited unexpectedly", name)
			} else {
				err = fmt.Errorf("%s: %w", name, err)
			}
			exited <- err
		}
	}()
	return cmd.Process.Pid, nil
}

func (s *Server) pipeLogs(name string, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		s.log.Info(sc.Text(), "daemon", name)
	}
}

func (s *Server) waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}

// mountFS mounts fstype at target unless already mounted. EBUSY / EEXIST from an
// existing mount are treated as success.
func mountFS(fstype, source, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if mounted, _ := isMountPoint(target); mounted {
		return nil
	}
	if err := syscall.Mount(source, target, fstype, 0, ""); err != nil {
		if err == syscall.EBUSY {
			return nil
		}
		return fmt.Errorf("mount %s at %s: %w", fstype, target, err)
	}
	return nil
}

func isMountPoint(target string) (bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := splitFields(sc.Text())
		if len(fields) >= 2 && fields[1] == target {
			return true, nil
		}
	}
	return false, nil
}

func splitFields(line string) []string {
	var out []string
	field := make([]rune, 0, len(line))
	flush := func() {
		if len(field) > 0 {
			out = append(out, string(field))
			field = field[:0]
		}
	}
	for _, r := range line {
		if r == ' ' || r == '\t' {
			flush()
			continue
		}
		field = append(field, r)
	}
	flush()
	return out
}
