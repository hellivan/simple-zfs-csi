//go:build zfsreal

package zfsconf_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
	"github.com/hellivan/simple-zfs-csi/internal/zpool/zfsconf"
)

// TestRealZFS runs the suite against the `zfs` binary on PATH. It refuses to
// run unless ZFS_CONF_ROOT is an existing dataset whose path contains
// "verify_zfs_functions", so it can never touch real data.
//
//	CGO_ENABLED=0 go test -c -tags zfsreal -o zfsconf.test ./internal/zpool/zfsconf
//	ZFS_CONF_ROOT=pool/verify_zfs_functions ./zfsconf.test -test.v
func TestRealZFS(t *testing.T) {
	base := os.Getenv("ZFS_CONF_ROOT")
	if !strings.Contains(base, "verify_zfs_functions") {
		t.Skip("set ZFS_CONF_ROOT to a dataset path containing verify_zfs_functions")
	}
	root := fmt.Sprintf("%s/conf-%d", base, time.Now().Unix())
	zfsconf.Run(t, zpool.NewZFS(nil), root, zfsconf.Options{Pause: 1100 * time.Millisecond})
}
