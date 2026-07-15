package zpool

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// DefaultHostPathDirs mirrors the candidate directories democratic-csi probes
// when locating the host's zpool/zfs: the standard sbin/bin dirs plus the Talos
// system-extension path (/usr/local/sbin) and the NixOS profile path
// (/run/current-system/sw/bin).
var DefaultHostPathDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
	"/run/current-system/sw/bin",
}

// Host-exec modes.
const (
	// HostExecNone runs the in-image tools directly (no host redirection).
	HostExecNone = ""
	// HostExecChroot enters the host root filesystem (mounted at HostRoot).
	HostExecChroot = "chroot"
	// HostExecNsenter enters the target PID's namespaces.
	HostExecNsenter = "nsenter"
)

// HostExec describes how the discovery pod reaches the host's own
// version-matched zpool/zfs binaries. The resolution logic mirrors
// democratic-csi's zfs/zpool wrappers: probe a list of candidate host paths for
// the binary and, if none is found, fall back to a clean-environment invocation
// so the lookup never depends on the container's inherited PATH.
type HostExec struct {
	// Mode is HostExecNone, HostExecChroot or HostExecNsenter.
	Mode string
	// HostRoot is the container-visible path where the host root filesystem is
	// mounted (chroot mode), e.g. "/host". Defaults to "/host".
	HostRoot string
	// TargetPID is the host PID whose namespaces are entered (nsenter mode).
	// Defaults to 1.
	TargetPID int
	// PathDirs are the in-host directories probed for the binary. Empty uses
	// DefaultHostPathDirs.
	PathDirs []string
}

// BuildRunner returns a Runner that redirects each zpool/zfs invocation to the
// host per the configured mode. base is the underlying exec runner (nil uses the
// default). For HostExecNone it returns base unchanged.
func (h HostExec) BuildRunner(base Runner) Runner {
	if base == nil {
		base = execRunner
	}
	if h.Mode == HostExecNone || h.Mode == "none" {
		return base
	}

	dirs := h.PathDirs
	if len(dirs) == 0 {
		dirs = DefaultHostPathDirs
	}
	hostRoot := h.HostRoot
	if hostRoot == "" {
		hostRoot = "/host"
	}
	pid := h.TargetPID
	if pid <= 0 {
		pid = 1
	}
	pathEnv := "PATH=" + strings.Join(dirs, ":")

	// probeRoot is the container-visible prefix under which the host filesystem
	// is readable, used only to test which candidate path holds the binary.
	var probeRoot string
	switch h.Mode {
	case HostExecChroot:
		probeRoot = hostRoot
	case HostExecNsenter:
		probeRoot = fmt.Sprintf("/proc/%d/root", pid)
	}

	return func(ctx context.Context, name string, args ...string) (string, error) {
		hostBin, found := resolveHostBinary(probeRoot, name, dirs)

		var cmd []string
		switch h.Mode {
		case HostExecChroot:
			if found {
				cmd = append([]string{"chroot", hostRoot, hostBin}, args...)
			} else {
				cmd = append([]string{"chroot", hostRoot, "/usr/bin/env", "-i", pathEnv, name}, args...)
			}
		case HostExecNsenter:
			pfx := []string{"nsenter", "--target", strconv.Itoa(pid), "--mount", "--uts", "--ipc", "--net", "--"}
			if found {
				cmd = append(pfx, append([]string{hostBin}, args...)...)
			} else {
				cmd = append(pfx, append([]string{"/usr/bin/env", "-i", pathEnv, name}, args...)...)
			}
		default:
			return "", fmt.Errorf("unknown host-exec mode %q", h.Mode)
		}
		return base(ctx, cmd[0], cmd[1:]...)
	}
}

// resolveHostBinary finds the in-host path of a binary. If name is already
// absolute it is returned as-is when present under probeRoot. Otherwise each dir
// is probed; the returned path is expressed relative to the host root (i.e. the
// path as seen after entering the host context via chroot/nsenter).
func resolveHostBinary(probeRoot, name string, dirs []string) (string, bool) {
	if strings.Contains(name, "/") {
		if fileExists(path.Join(probeRoot, name)) {
			return name, true
		}
		return "", false
	}
	for _, d := range dirs {
		if fileExists(path.Join(probeRoot, d, name)) {
			return path.Join(d, name), true
		}
	}
	return "", false
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
