package memzfs_test

import (
	"testing"

	"github.com/hellivan/simple-zfs-csi/internal/zpool/memzfs"
	"github.com/hellivan/simple-zfs-csi/internal/zpool/zfsconf"
)

func TestConformance(t *testing.T) {
	m := memzfs.New("tank")
	zfsconf.Run(t, m, "tank/conf", zfsconf.Options{Check: m.Check})
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
}
