package csi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// nqn used across the parsing cases.
const testNQN = "nqn.2025-01.io.simple-zfs-csi:talos-1:pvc-abc"

// writeFile is a test helper creating a file with parents.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNVMeNamespaceFromSysfs(t *testing.T) {
	root := t.TempDir()
	// nvme0 -> the target subsystem, with namespace nvme0n1 (and a multipath
	// nvme0c0n1 path that must be ignored).
	writeFile(t, filepath.Join(root, "nvme0", "subsysnqn"), testNQN+"\n")
	if err := os.MkdirAll(filepath.Join(root, "nvme0", "nvme0c0n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nvme0", "nvme0n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// nvme1 -> an unrelated subsystem.
	writeFile(t, filepath.Join(root, "nvme1", "subsysnqn"), "nqn.other\n")
	if err := os.MkdirAll(filepath.Join(root, "nvme1", "nvme1n1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := nvmeNamespaceFromSysfs(root, testNQN); got != "/dev/nvme0n1" {
		t.Errorf("nvmeNamespaceFromSysfs() = %q, want /dev/nvme0n1", got)
	}
	if got := nvmeControllerFromSysfs(root, testNQN); got != "/dev/nvme0" {
		t.Errorf("nvmeControllerFromSysfs() = %q, want /dev/nvme0", got)
	}
	// Subsystem present but namespace not yet enumerated -> "".
	root2 := t.TempDir()
	writeFile(t, filepath.Join(root2, "nvme0", "subsysnqn"), testNQN+"\n")
	if got := nvmeNamespaceFromSysfs(root2, testNQN); got != "" {
		t.Errorf("no-namespace nvmeNamespaceFromSysfs() = %q, want empty", got)
	}
	// Not connected at all -> "".
	if got := nvmeControllerFromSysfs(root2, "nqn.missing"); got != "" {
		t.Errorf("missing nvmeControllerFromSysfs() = %q, want empty", got)
	}

	// Multipath-enabled kernel (CONFIG_NVME_MULTIPATH, the modern default): the
	// controller carries ONLY the path device (nvme3c3n1); the usable head block
	// device (nvme3n1) lives under the subsystem, not the controller. Resolution
	// must derive and return the head.
	root3 := t.TempDir()
	writeFile(t, filepath.Join(root3, "nvme3", "subsysnqn"), testNQN+"\n")
	if err := os.MkdirAll(filepath.Join(root3, "nvme3", "nvme3c3n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := nvmeNamespaceFromSysfs(root3, testNQN); got != "/dev/nvme3n1" {
		t.Errorf("multipath nvmeNamespaceFromSysfs() = %q, want /dev/nvme3n1", got)
	}
}

func TestNVMeDeviceReady(t *testing.T) {
	root := t.TempDir()
	// A ready device: /sys/block/<dev>/size present and non-zero.
	writeFile(t, filepath.Join(root, "nvme0n1", "size"), "204800\n")
	// Present but zero-sized: namespace not yet usable (the connect-time race).
	writeFile(t, filepath.Join(root, "nvme1n1", "size"), "0\n")

	if !nvmeDeviceReady(root, "/dev/nvme0n1") {
		t.Errorf("nvmeDeviceReady(nvme0n1) = false, want true")
	}
	if nvmeDeviceReady(root, "/dev/nvme1n1") {
		t.Errorf("nvmeDeviceReady(nvme1n1) = true, want false (zero size)")
	}
	// No sysfs block entry at all (device node not created yet) -> not ready.
	if nvmeDeviceReady(root, "/dev/nvme2n1") {
		t.Errorf("nvmeDeviceReady(missing) = true, want false")
	}
	// Empty device path -> not ready.
	if nvmeDeviceReady(root, "") {
		t.Errorf("nvmeDeviceReady(\"\") = true, want false")
	}
}

func TestParseNVMeListDevice(t *testing.T) {
	cases := []struct {
		name string
		out  string
		nqn  string
		want string
	}{
		{
			name: "flat schema (nvme-cli 1.x)",
			out: `{"Devices":[
				{"DevicePath":"/dev/nvme0n1","SubsystemNQN":"nqn.other"},
				{"DevicePath":"/dev/nvme1n1","SubsystemNQN":"` + testNQN + `"}
			]}`,
			nqn:  testNQN,
			want: "/dev/nvme1n1",
		},
		{
			name: "nested schema (nvme-cli 2.x)",
			out: `{"Devices":[{
				"HostNQN":"nqn.2014-08.org.nvmexpress:uuid:x",
				"Subsystems":[
					{"SubsystemNQN":"nqn.other","Namespaces":[{"NameSpace":"nvme0n1"}]},
					{"SubsystemNQN":"` + testNQN + `","Namespaces":[{"NameSpace":"nvme1n1"}]}
				]}]}`,
			nqn:  testNQN,
			want: "/dev/nvme1n1",
		},
		{
			name: "nested schema lowercase Namespace key",
			out: `{"Devices":[{
				"Subsystems":[
					{"SubsystemNQN":"` + testNQN + `","Namespaces":[{"Namespace":"nvme2n1"}]}
				]}]}`,
			nqn:  testNQN,
			want: "/dev/nvme2n1",
		},
		{
			name: "subsystem present but no namespace attached",
			out: `{"Devices":[{
				"Subsystems":[{"SubsystemNQN":"` + testNQN + `","Namespaces":[]}]
			}]}`,
			nqn:  testNQN,
			want: "",
		},
		{
			name: "not connected",
			out:  `{"Devices":[]}`,
			nqn:  testNQN,
			want: "",
		},
		{
			name: "invalid json",
			out:  `not json`,
			nqn:  testNQN,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNVMeListDevice([]byte(tc.out), tc.nqn)
			if got != tc.want {
				t.Errorf("parseNVMeListDevice() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHostMounterUnmount covers docs/known-pitfalls.md class 16: Unmount must
// never block indefinitely on a hard NFS/NVMe-oF mount to a vanished server, so
// it bounds the plain `umount` with a timeout and falls back to a lazy
// force-unmount (`umount -f -l`) rather than waiting on a call that may be
// stuck in uninterruptible (D-state) sleep.
func TestHostMounterUnmount(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var calls []string
		m := &hostMounter{
			unmountTimeout: 50 * time.Millisecond,
			run: func(ctx context.Context, name string, args ...string) (string, error) {
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				return "", nil
			},
		}
		if err := m.Unmount("/mnt/x"); err != nil {
			t.Fatalf("Unmount() = %v, want nil", err)
		}
		if want := []string{"umount /mnt/x"}; len(calls) != 1 || calls[0] != want[0] {
			t.Errorf("calls = %v, want %v", calls, want)
		}
	})

	t.Run("already unmounted", func(t *testing.T) {
		m := &hostMounter{
			unmountTimeout: 50 * time.Millisecond,
			run: func(ctx context.Context, name string, args ...string) (string, error) {
				return "", errors.New("umount: /mnt/x: not mounted.")
			},
		}
		if err := m.Unmount("/mnt/x"); err != nil {
			t.Fatalf("Unmount() = %v, want nil", err)
		}
	})

	t.Run("plain umount fails, lazy fallback succeeds", func(t *testing.T) {
		var mu sync.Mutex
		var calls []string
		m := &hostMounter{
			unmountTimeout:   50 * time.Millisecond,
			forceLazyUnmount: true,
			run: func(ctx context.Context, name string, args ...string) (string, error) {
				mu.Lock()
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				mu.Unlock()
				if len(args) == 1 {
					// plain `umount <target>`
					return "", errors.New("umount: /mnt/x: target is busy.")
				}
				return "", nil
			},
		}
		if err := m.Unmount("/mnt/x"); err != nil {
			t.Fatalf("Unmount() = %v, want nil", err)
		}
		mu.Lock()
		defer mu.Unlock()
		want := []string{"umount /mnt/x", "umount -f -l /mnt/x"}
		if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
			t.Errorf("calls = %v, want %v", calls, want)
		}
	})

	t.Run("plain umount fails, lazy fallback disabled by default returns the error", func(t *testing.T) {
		var mu sync.Mutex
		var calls []string
		m := &hostMounter{
			unmountTimeout: 50 * time.Millisecond,
			run: func(ctx context.Context, name string, args ...string) (string, error) {
				mu.Lock()
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				mu.Unlock()
				return "", errors.New("umount: /mnt/x: target is busy.")
			},
		}
		if err := m.Unmount("/mnt/x"); err == nil {
			t.Fatal("Unmount() = nil, want an error (ForceLazyUnmount defaults to false)")
		}
		mu.Lock()
		defer mu.Unlock()
		if want := []string{"umount /mnt/x"}; len(calls) != 1 || calls[0] != want[0] {
			t.Errorf("calls = %v, want %v (no lazy fallback)", calls, want)
		}
	})

	t.Run("plain umount hangs past timeout, lazy fallback unblocks", func(t *testing.T) {
		var mu sync.Mutex
		var calls []string
		m := &hostMounter{
			unmountTimeout:   20 * time.Millisecond,
			forceLazyUnmount: true,
			run: func(ctx context.Context, name string, args ...string) (string, error) {
				mu.Lock()
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				mu.Unlock()
				if len(args) == 1 {
					// Simulate a hard NFS mount stuck in uninterruptible sleep:
					// this deliberately never returns, even after ctx is done, so
					// Unmount must not wait on it.
					select {}
				}
				return "", nil
			},
		}
		start := time.Now()
		if err := m.Unmount("/mnt/x"); err != nil {
			t.Fatalf("Unmount() = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Unmount() took %s, want bounded by unmountTimeout", elapsed)
		}
		mu.Lock()
		defer mu.Unlock()
		want := []string{"umount /mnt/x", "umount -f -l /mnt/x"}
		if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
			t.Errorf("calls = %v, want %v", calls, want)
		}
	})
}
