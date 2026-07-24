package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NVMeConnectOptions carries the parameters for an `nvme connect`. HostNQN/HostID
// are the per-attach initiator identity (ADR-0011); DHChapKey is the optional
// in-band DH-CHAP secret. All three are empty for an unauthenticated connect.
type NVMeConnectOptions struct {
	Transport string
	Addr      string
	Port      string
	NQN       string
	HostNQN   string
	HostID    string
	DHChapKey string
}

// NodeMounter abstracts the privileged host operations the node plugin performs:
// NFS mounts, NVMe-oF connect/disconnect, and filesystem/block publishing. It is
// an interface so the NodeServer routing logic can be unit-tested without a real
// host (see fakeMounter in node_test.go).
type NodeMounter interface {
	// IsMountPoint reports whether path is currently a mount point. A missing
	// path is not a mount point and returns (false, nil).
	IsMountPoint(path string) (bool, error)
	// MakeDir creates a directory (and parents) to serve as a mount target.
	MakeDir(path string) error
	// MakeFile creates an empty file to serve as a bind-mount target for block
	// volumes.
	MakeFile(path string) error
	// RemovePath removes a file or empty directory, ignoring absence.
	RemovePath(path string) error
	// MountNFS mounts source ("ip:/export/path") at target with the given
	// options.
	MountNFS(source, target string, options []string) error
	// FormatAndMount formats device with fsType if it has no filesystem, then
	// mounts it at target.
	FormatAndMount(device, target, fsType string, options []string) error
	// BindMountDevice bind-mounts a block device node at target (block volumes).
	BindMountDevice(device, target string, readOnly bool) error
	// Unmount unmounts target, ignoring an already-unmounted target.
	Unmount(target string) error
	// NVMeConnect connects to the NVMe-oF subsystem and returns the resulting
	// block device path (e.g. "/dev/nvme1n1"). It is idempotent: an existing
	// connection returns the current device.
	NVMeConnect(ctx context.Context, opts NVMeConnectOptions) (string, error)
	// NVMeDisconnect disconnects the NVMe-oF subsystem, ignoring absence.
	NVMeDisconnect(ctx context.Context, nqn string) error
	// NVMeDevice returns the current block device path for a connected NQN, or
	// "" when the subsystem is not connected.
	NVMeDevice(ctx context.Context, nqn string) (string, error)
	// RescanNVMe asks the controller(s) backing nqn to re-read the namespace size
	// after the backing zvol has been grown, so the block device reflects the new
	// capacity.
	RescanNVMe(ctx context.Context, nqn string) error
	// ResizeFS grows the filesystem on device (mounted at volumePath) to fill the
	// device. It is a no-op when the device carries no filesystem (raw block).
	ResizeFS(device, volumePath string) error
}

// hostMounter is the real NodeMounter. It shells out to mount(8), nvme(1) and
// mkfs via a Runner (host-exec aware) and inspects /proc/mounts and sysfs
// directly. The plugin container runs privileged in the host mount namespace, so
// the paths and devices it sees are the host's.
type hostMounter struct {
	// run executes a command and returns combined stdout. Reuses the host-exec
	// Runner shape from the zpool package so the node plugin can invoke the
	// host's nvme/mount binaries.
	run func(ctx context.Context, name string, args ...string) (string, error)
	// unmountTimeout bounds how long Unmount waits for a plain `umount` before
	// giving up (or falling back, if forceLazyUnmount is set). Defaults to
	// defaultUnmountTimeout; overridable in tests.
	unmountTimeout time.Duration
	// forceLazyUnmount, when true, falls back to `umount -f -l` (force +
	// lazy/MNT_DETACH) after a plain umount fails or times out. Opt-in: see
	// HostMounterOptions.ForceLazyUnmount.
	forceLazyUnmount bool
}

// defaultUnmountTimeout is the production default for
// HostMounterOptions.UnmountTimeout. It matches systemd's own
// `DefaultTimeoutStopSec` (90s) — the same order of magnitude operators
// already expect a service/unit to be given before a stop is considered stuck.
// See docs/known-pitfalls.md class 16: a hard NFS mount to a vanished server
// can block `umount` in uninterruptible (D-state) sleep, which this bound
// works around by not waiting on the stuck call rather than by killing it
// (SIGKILL cannot interrupt D-state).
const defaultUnmountTimeout = 90 * time.Second

// HostMounterOptions configures optional hostMounter behavior beyond the
// required command runner. The zero value is safe: it uses
// defaultUnmountTimeout and leaves the lazy-fallback behavior disabled.
type HostMounterOptions struct {
	// UnmountTimeout bounds how long Unmount waits for a plain `umount` before
	// giving up (or falling back, if ForceLazyUnmount is set). Zero uses
	// defaultUnmountTimeout. See docs/known-pitfalls.md class 16.
	UnmountTimeout time.Duration
	// ForceLazyUnmount, when true, makes Unmount fall back to `umount -f -l`
	// (force + lazy/MNT_DETACH) after a plain umount fails or times out, so
	// NodeUnpublishVolume can no longer be blocked indefinitely by a dead
	// NFS/NVMe-oF server. This is opt-in (default false) rather than always-on
	// because MNT_DETACH detaches the mount point before outstanding I/O to the
	// target has necessarily finished draining, which is a real (if narrow)
	// data-safety tradeoff operators should choose explicitly. Without it,
	// Unmount still never blocks forever — it just returns the timeout/error to
	// the caller instead of forcing the detach. See docs/known-pitfalls.md
	// class 16.
	ForceLazyUnmount bool
}

// NewHostMounter returns a NodeMounter backed by run (a host-exec-aware command
// runner). run must not be nil.
func NewHostMounter(run func(ctx context.Context, name string, args ...string) (string, error), opts HostMounterOptions) NodeMounter {
	timeout := opts.UnmountTimeout
	if timeout <= 0 {
		timeout = defaultUnmountTimeout
	}
	return &hostMounter{run: run, unmountTimeout: timeout, forceLazyUnmount: opts.ForceLazyUnmount}
}

func (m *hostMounter) IsMountPoint(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, fmt.Errorf("read /proc/mounts: %w", err)
	}
	clean := filepath.Clean(path)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == clean {
			return true, nil
		}
	}
	return false, nil
}

func (m *hostMounter) MakeDir(path string) error {
	return os.MkdirAll(path, 0o750)
}

func (m *hostMounter) MakeFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE, 0o640)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return f.Close()
}

func (m *hostMounter) RemovePath(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *hostMounter) MountNFS(source, target string, options []string) error {
	args := []string{"-t", "nfs"}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, source, target)
	_, err := m.run(context.Background(), "mount", args...)
	return err
}

func (m *hostMounter) FormatAndMount(device, target, fsType string, options []string) error {
	if fsType == "" {
		fsType = "ext4"
	}
	existing, err := m.detectFS(device)
	if err != nil {
		return err
	}
	if existing == "" {
		mkfsArgs := []string{}
		if fsType == "ext4" || fsType == "ext3" {
			mkfsArgs = append(mkfsArgs, "-F")
		}
		if fsType == "xfs" {
			mkfsArgs = append(mkfsArgs, "-f")
		}
		mkfsArgs = append(mkfsArgs, device)
		if _, err := m.run(context.Background(), "mkfs."+fsType, mkfsArgs...); err != nil {
			return fmt.Errorf("mkfs.%s %s: %w", fsType, device, err)
		}
	}
	args := []string{"-t", fsType}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, device, target)
	_, err = m.run(context.Background(), "mount", args...)
	return err
}

func (m *hostMounter) BindMountDevice(device, target string, readOnly bool) error {
	opts := "bind"
	if readOnly {
		opts = "bind,ro"
	}
	_, err := m.run(context.Background(), "mount", "-o", opts, device, target)
	return err
}

func (m *hostMounter) Unmount(target string) error {
	timeout := m.unmountTimeout
	if timeout <= 0 {
		timeout = defaultUnmountTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Run the plain umount in a goroutine rather than waiting on it directly:
	// if the target is a hard NFS mount to a server that has vanished, the
	// call can block in uninterruptible (D-state) sleep, where not even
	// SIGKILL (which ctx's cancellation would send via exec.CommandContext)
	// can unblock it. Racing the result against ctx.Done() bounds *our* wait
	// without depending on the child process ever actually dying.
	result := make(chan error, 1)
	go func() {
		_, err := m.run(ctx, "umount", target)
		result <- err
	}()

	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}

	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "not mounted") {
		return nil
	}
	if !m.forceLazyUnmount {
		return err
	}

	// Plain umount failed, or did not return within timeout, and the lazy
	// fallback is enabled. Fall back to a lazy force-unmount: MNT_DETACH
	// (`-l`) detaches the mount point from the namespace immediately, without
	// waiting for outstanding I/O to a dead server to drain, so this call
	// succeeds even when the plain umount above is permanently stuck. `-f`
	// additionally forces an NFS unmount that would otherwise be rejected as
	// unreachable. See docs/known-pitfalls.md class 16.
	_, lazyErr := m.run(context.Background(), "umount", "-f", "-l", target)
	if lazyErr != nil && strings.Contains(lazyErr.Error(), "not mounted") {
		return nil
	}
	return lazyErr
}

// nvmeConnectTimeout / nvmeDevicePoll bound the wait for the namespace block
// device to appear after `nvme connect` returns (controller enumeration + udev
// are slightly asynchronous).
const (
	nvmeConnectTimeout = 10 * time.Second
	nvmeDevicePoll     = 500 * time.Millisecond
)

func (m *hostMounter) NVMeConnect(ctx context.Context, o NVMeConnectOptions) (string, error) {
	if dev, _ := m.nvmeDevice(ctx, o.NQN); dev != "" && nvmeDeviceReady(sysBlock, dev) {
		return dev, nil
	}
	args := []string{"connect", "-t", o.Transport, "-a", o.Addr, "-s", o.Port, "-n", o.NQN}
	if o.HostNQN != "" {
		args = append(args, "--hostnqn", o.HostNQN)
	}
	if o.HostID != "" {
		args = append(args, "--hostid", o.HostID)
	}
	if o.DHChapKey != "" {
		args = append(args, "--dhchap-secret", o.DHChapKey)
	}
	if _, err := m.run(ctx, "nvme", args...); err != nil {
		// "already connected" means a previous attempt created the controller but we
		// failed to resolve its block device afterwards; don't get wedged — fall
		// through and resolve the existing connection idempotently.
		if !strings.Contains(strings.ToLower(err.Error()), "already connected") {
			return "", fmt.Errorf("nvme connect %s: %w", o.NQN, err)
		}
	}
	// The namespace block device can appear a moment after `nvme connect` returns,
	// so poll rather than looking exactly once.
	dev, err := m.waitNVMeDevice(ctx, o.NQN, nvmeConnectTimeout)
	if err != nil {
		return "", err
	}
	if dev == "" {
		return "", fmt.Errorf("nvme device for %s not found after connect", o.NQN)
	}
	return dev, nil
}

// waitNVMeDevice polls for the namespace block device backing nqn until it
// appears AND is usable or the timeout elapses (returns "" on timeout). Each
// miss nudges the controller with `nvme ns-rescan`: a client can connect just
// before the target enables the namespace, or a live controller may have missed
// the add-namespace notification, leaving it with zero namespaces until
// rescanned. Readiness (nvmeDeviceReady) is required because the sysfs name can
// resolve a moment before the head block device is created and sized; returning
// it early makes the caller's format/mount fail with ENOENT or EIO.
func (m *hostMounter) waitNVMeDevice(ctx context.Context, nqn string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dev, _ := m.nvmeDevice(ctx, nqn); dev != "" && nvmeDeviceReady(sysBlock, dev) {
			return dev, nil
		}
		if ctrl := nvmeControllerFromSysfs(sysClassNVMe, nqn); ctrl != "" {
			_, _ = m.run(ctx, "nvme", "ns-rescan", ctrl)
		}
		if time.Now().After(deadline) {
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(nvmeDevicePoll):
		}
	}
}

func (m *hostMounter) NVMeDisconnect(ctx context.Context, nqn string) error {
	_, err := m.run(ctx, "nvme", "disconnect", "-n", nqn)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}

// NVMeDevice exposes the connected device lookup for node-side expansion.
func (m *hostMounter) NVMeDevice(ctx context.Context, nqn string) (string, error) {
	return m.nvmeDevice(ctx, nqn)
}

// RescanNVMe re-reads the namespace size after a zvol grow. `nvme ns-rescan`
// requires a controller char device (e.g. /dev/nvme0); the multipath namespace
// head block device (/dev/nvme0n1) is not a valid target ("Block device
// required"). A subsystem may be reached through several controllers/paths, so
// rescan every controller backing nqn.
func (m *hostMounter) RescanNVMe(ctx context.Context, nqn string) error {
	if nqn == "" {
		return fmt.Errorf("nqn is empty")
	}
	ctrls := nvmeControllersForNQN(sysClassNVMe, nqn)
	if len(ctrls) == 0 {
		return fmt.Errorf("no nvme controller connected for nqn %q", nqn)
	}
	for _, ctrl := range ctrls {
		if _, err := m.run(ctx, "nvme", "ns-rescan", "/dev/"+ctrl); err != nil {
			return err
		}
	}
	return nil
}

// ResizeFS grows the filesystem on device to fill it. ext* is grown by device
// (resize2fs), xfs by mountpoint (xfs_growfs); an unformatted device is a no-op.
func (m *hostMounter) ResizeFS(device, volumePath string) error {
	fsType, err := m.detectFS(device)
	if err != nil {
		return err
	}
	switch {
	case fsType == "":
		return nil
	case strings.HasPrefix(fsType, "ext"):
		_, err := m.run(context.Background(), "resize2fs", device)
		return err
	case fsType == "xfs":
		_, err := m.run(context.Background(), "xfs_growfs", volumePath)
		return err
	default:
		_, err := m.run(context.Background(), "resize2fs", device)
		return err
	}
}

// detectFS returns the filesystem type on device, or "" if the device is
// unformatted. It uses blkid, treating a non-zero exit (no signature) as empty.
func (m *hostMounter) detectFS(device string) (string, error) {
	out, err := m.run(context.Background(), "blkid", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		// blkid exits 2 when no filesystem signature is found.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// sysClassNVMe is the sysfs directory listing connected NVMe controllers. It is
// the source of truth for NQN->device resolution: version-independent, unlike
// `nvme list -o json`, whose schema varies across nvme-cli releases (2.13 emits
// a flat list with DevicePath but no SubsystemNQN once a namespace is present).
const sysClassNVMe = "/sys/class/nvme"

// sysBlock is the sysfs directory of block devices. It backs nvmeDeviceReady:
// a namespace's usable size is published at /sys/block/<dev>/size.
const sysBlock = "/sys/block"

// nvmeDeviceReady reports whether the namespace block device dev (e.g.
// "/dev/nvme0n1") is present and usable, i.e. its sysfs block entry exists and
// reports a non-zero size (in 512-byte sectors). Right after `nvme connect` the
// sysfs *name* can resolve a moment before the head block device is created and
// its size populated; formatting or mounting in that window fails with ENOENT
// or EIO. Gating on a non-zero size closes both races.
func nvmeDeviceReady(sysBlockRoot, dev string) bool {
	base := strings.TrimPrefix(dev, "/dev/")
	if base == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(sysBlockRoot, base, "size"))
	if err != nil {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return err == nil && n > 0
}

// nvmeDevice returns the namespace block device (e.g. "/dev/nvme1n1") exported
// by the connected subsystem nqn, or "" if not connected / no namespace yet. It
// reads sysfs (authoritative) and only falls back to parsing `nvme list -o
// json` when sysfs is unavailable.
func (m *hostMounter) nvmeDevice(ctx context.Context, nqn string) (string, error) {
	if dev := nvmeNamespaceFromSysfs(sysClassNVMe, nqn); dev != "" {
		return dev, nil
	}
	out, err := m.run(ctx, "nvme", "list", "-o", "json")
	if err != nil {
		return "", nil
	}
	return parseNVMeListDevice([]byte(out), nqn), nil
}

// nvmeControllersForNQN lists controller directory names under root (e.g.
// "nvme0") whose subsystem NQN matches nqn.
func nvmeControllersForNQN(root, nqn string) []string {
	ctrls, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range ctrls {
		b, err := os.ReadFile(filepath.Join(root, c.Name(), "subsysnqn"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == nqn {
			out = append(out, c.Name())
		}
	}
	return out
}

// nvmePathDeviceRe matches a multipath "path" namespace device as it appears
// under a controller in sysfs (e.g. "nvme0c0n1") and captures the shared head
// block device name ("nvme0" + "n1" = "nvme0n1"). The leading instance number is
// the subsystem/head instance, so dropping the "c<controller>" segment yields the
// usable head device regardless of how many paths a controller has.
var nvmePathDeviceRe = regexp.MustCompile(`^(nvme\d+)c\d+(n\d+)$`)

// nvmeNamespaceFromSysfs returns the namespace block device exported by nqn by
// scanning the matching controllers' children, or "" if none is present yet.
// It handles both layouts:
//   - non-multipath: the namespace head is a direct child of the controller
//     (e.g. nvme0 -> nvme0n1).
//   - multipath (CONFIG_NVME_MULTIPATH, the modern default): the controller only
//     carries a per-path device (e.g. nvme0c0n1); the usable block device is the
//     shared subsystem head (nvme0n1), derived by dropping the "c<controller>"
//     path segment.
func nvmeNamespaceFromSysfs(root, nqn string) string {
	for _, ctrl := range nvmeControllersForNQN(root, nqn) {
		entries, err := os.ReadDir(filepath.Join(root, ctrl))
		if err != nil {
			continue
		}
		directPrefix := ctrl + "n" // "nvme0n" matches the non-multipath head nvme0n1
		var mpathHead string
		for _, e := range entries {
			name := e.Name()
			// Non-multipath: namespace head is a direct child (nvme0n1). This also
			// excludes the multipath path form, which starts with "nvme0c".
			if strings.HasPrefix(name, directPrefix) {
				return "/dev/" + name
			}
			// Multipath: the controller only exposes a path device (nvme0c0n1);
			// resolve it to the shared head block device (nvme0n1).
			if m := nvmePathDeviceRe.FindStringSubmatch(name); m != nil {
				mpathHead = "/dev/" + m[1] + m[2]
			}
		}
		if mpathHead != "" {
			return mpathHead
		}
	}
	return ""
}

// nvmeControllerFromSysfs returns the controller char device (e.g. "/dev/nvme0")
// serving nqn, for `nvme ns-rescan`; "" if none is connected.
func nvmeControllerFromSysfs(root, nqn string) string {
	for _, ctrl := range nvmeControllersForNQN(root, nqn) {
		return "/dev/" + ctrl
	}
	return ""
}

// parseNVMeListDevice extracts the block device backing nqn from `nvme list -o
// json` output. It tolerates both the flat schema (nvme-cli 1.x, one entry per
// namespace) and the nested schema (nvme-cli 2.x, one entry per host with
// subsystems/namespaces within). Returns "" when the subsystem is not present.
func parseNVMeListDevice(out []byte, nqn string) string {
	var parsed struct {
		Devices []struct {
			// Flat schema (nvme-cli 1.x): one entry per namespace block device.
			DevicePath   string `json:"DevicePath"`
			SubsystemNQN string `json:"SubsystemNQN"`
			// Nested schema (nvme-cli 2.x): one entry per host, subsystems within.
			Subsystems []struct {
				SubsystemNQN string `json:"SubsystemNQN"`
				Namespaces   []struct {
					NameSpace string `json:"NameSpace"`
					Namespace string `json:"Namespace"`
				} `json:"Namespaces"`
			} `json:"Subsystems"`
		} `json:"Devices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ""
	}
	for _, d := range parsed.Devices {
		// Flat schema.
		if d.SubsystemNQN == nqn && d.DevicePath != "" {
			return d.DevicePath
		}
		// Nested schema: the subsystem-level namespace name is the block device
		// (e.g. "nvme0n1") — prefix it with /dev/.
		for _, s := range d.Subsystems {
			if s.SubsystemNQN != nqn {
				continue
			}
			for _, ns := range s.Namespaces {
				name := ns.NameSpace
				if name == "" {
					name = ns.Namespace
				}
				if name != "" {
					return "/dev/" + name
				}
			}
		}
	}
	return ""
}
