package csi

import (
	"os"
	"path/filepath"
	"testing"
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
