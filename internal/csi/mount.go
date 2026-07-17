package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	NVMeConnect(ctx context.Context, transport, addr, port, nqn string) (string, error)
	// NVMeDisconnect disconnects the NVMe-oF subsystem, ignoring absence.
	NVMeDisconnect(ctx context.Context, nqn string) error
	// NVMeDevice returns the current block device path for a connected NQN, or
	// "" when the subsystem is not connected.
	NVMeDevice(ctx context.Context, nqn string) (string, error)
	// RescanNVMe asks the kernel to re-read a namespace's size after the backing
	// zvol has been grown, so the block device reflects the new capacity.
	RescanNVMe(ctx context.Context, device string) error
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
}

// NewHostMounter returns a NodeMounter backed by run (a host-exec-aware command
// runner). run must not be nil.
func NewHostMounter(run func(ctx context.Context, name string, args ...string) (string, error)) NodeMounter {
	return &hostMounter{run: run}
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
	_, err := m.run(context.Background(), "umount", target)
	if err != nil && strings.Contains(err.Error(), "not mounted") {
		return nil
	}
	return err
}

func (m *hostMounter) NVMeConnect(ctx context.Context, transport, addr, port, nqn string) (string, error) {
	if dev, _ := m.nvmeDevice(ctx, nqn); dev != "" {
		return dev, nil
	}
	if _, err := m.run(ctx, "nvme", "connect", "-t", transport, "-a", addr, "-s", port, "-n", nqn); err != nil {
		return "", fmt.Errorf("nvme connect %s: %w", nqn, err)
	}
	dev, err := m.nvmeDevice(ctx, nqn)
	if err != nil {
		return "", err
	}
	if dev == "" {
		return "", fmt.Errorf("nvme device for %s not found after connect", nqn)
	}
	return dev, nil
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
// accepts the namespace block device and rescans its controller.
func (m *hostMounter) RescanNVMe(ctx context.Context, device string) error {
	if device == "" {
		return fmt.Errorf("device is empty")
	}
	_, err := m.run(ctx, "nvme", "ns-rescan", device)
	return err
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

// nvmeDevice returns the block device path (e.g. "/dev/nvme1n1") for a connected
// subsystem NQN, or "" if not connected. It parses `nvme list-subsys -o json`.
func (m *hostMounter) nvmeDevice(ctx context.Context, nqn string) (string, error) {
	out, err := m.run(ctx, "nvme", "list", "-o", "json")
	if err != nil {
		return "", nil
	}
	var parsed struct {
		Devices []struct {
			DevicePath   string `json:"DevicePath"`
			SubsystemNQN string `json:"SubsystemNQN"`
		} `json:"Devices"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return "", nil
	}
	for _, d := range parsed.Devices {
		if d.SubsystemNQN == nqn {
			return d.DevicePath, nil
		}
	}
	return "", nil
}
