package csi

import "testing"

// nqn used across the parsing cases.
const testNQN = "nqn.2025-01.io.simple-zfs-csi:talos-1:pvc-abc"

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
